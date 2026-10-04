//! Private, versioned stdio supervisor. Local preflight is separate from sync.
use crate::remote::{Error, Result};
use crate::sync_progress::Progress;
use serde_json::Value;
use std::{path::Path, process::Stdio, time::Duration};
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    process::{Child, ChildStdin, ChildStdout, Command},
    time::timeout,
};

pub struct Sidecar {
    // Field order is deliberate: signal the owned process group before Child's
    // Drop can reap its leader and release the PID for reuse.
    #[cfg(target_os = "macos")]
    group: macos_group::Group,
    child: Child,
    input: ChildStdin,
    output: BufReader<ChildStdout>,
    #[cfg(windows)]
    _job: windows_job::Job,
}

fn transient_preview_error(command: Option<&str>, code: Option<&str>) -> Option<Error> {
    (command == Some("sync_preview") && code == Some("sync_preview_retryable")).then(|| {
        Error::new(
            "sync_preview_retryable",
            "服务器连接暂时不可用，可以稍后重新检查变更",
        )
    })
}

impl Sidecar {
    pub async fn start(path: &Path) -> Result<Self> {
        let mut command = Command::new(path);
        command
            .arg("--desktop")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .kill_on_drop(true);
        #[cfg(windows)]
        command.creation_flags(0x08000000); // CREATE_NO_WINDOW, never flash a console.
        #[cfg(target_os = "macos")]
        command.process_group(0);
        let mut child = command
            .spawn()
            .map_err(|_| Error::new("sidecar_start", "同步后台启动失败，可重试"))?;
        #[cfg(target_os = "macos")]
        let group = macos_group::Group::new(&child)?;
        #[cfg(windows)]
        let job = windows_job::Job::assign(&child)?;
        let input = child
            .stdin
            .take()
            .ok_or_else(|| Error::new("sidecar_pipe", "无法打开后台输入管道"))?;
        let output = BufReader::new(
            child
                .stdout
                .take()
                .ok_or_else(|| Error::new("sidecar_pipe", "无法打开后台输出管道"))?,
        );
        let mut sidecar = Self {
            #[cfg(target_os = "macos")]
            group,
            child,
            input,
            output,
            #[cfg(windows)]
            _job: job,
        };
        let ready = sidecar.read_event().await?;
        if ready["version"] != 1 || ready["type"] != "ready" || !ready["capabilities"].is_array() {
            return Err(Error::new("sidecar_version", "同步后台协议不兼容"));
        }
        Ok(sidecar)
    }

    async fn read_event(&mut self) -> Result<Value> {
        self.read_event_with_timeout(Duration::from_secs(5)).await
    }

    async fn read_event_with_timeout(&mut self, wait: Duration) -> Result<Value> {
        let result = timeout(wait, async {
            let mut bytes = Vec::new();
            loop {
                let chunk = self.output.fill_buf().await.map_err(Error::network)?;
                if chunk.is_empty() {
                    return Err(Error::new("sidecar_exit", "同步后台已退出"));
                }
                let end = chunk
                    .iter()
                    .position(|b| *b == b'\n')
                    .map(|index| index + 1);
                let take = end.unwrap_or(chunk.len());
                if bytes.len() + take > 4 * 1024 * 1024 {
                    return Err(Error::new("sidecar_protocol", "后台响应超过限制"));
                }
                bytes.extend_from_slice(&chunk[..take]);
                self.output.consume(take);
                if end.is_some() {
                    return serde_json::from_slice(&bytes)
                        .map_err(|_| Error::new("sidecar_protocol", "后台响应无效"));
                }
            }
        })
        .await;
        result.map_err(|_| Error::new("sidecar_timeout", "同步后台未响应，可重试"))?
    }

    pub async fn ping(&mut self) -> Result<()> {
        #[cfg(target_os = "macos")]
        if self.group.exited().map_err(Error::network)? {
            // Never reap the leader before retiring the group guard.
            self.group.terminate();
            let _ = self.child.wait().await;
            return Err(Error::new("sidecar_exit", "同步后台已退出"));
        }
        #[cfg(not(target_os = "macos"))]
        if self.child.try_wait().map_err(Error::network)?.is_some() {
            return Err(Error::new("sidecar_exit", "同步后台已退出"));
        }
        timeout(
            Duration::from_secs(5),
            self.input
                .write_all(b"{\"version\":1,\"id\":\"health\",\"type\":\"ping\"}\n"),
        )
        .await
        .map_err(Error::network)?
        .map_err(Error::network)?;
        let event = self.read_event().await?;
        if event["version"] != 1 || event["type"] != "pong" || event["id"] != "health" {
            return Err(Error::new("sidecar_protocol", "后台健康检查失败"));
        }
        Ok(())
    }

    pub async fn inspect(&mut self, directory: &Path) -> Result<Value> {
        let directory = directory
            .to_str()
            .ok_or_else(|| Error::new("path", "目录名称无法用 UTF-8 表示"))?;
        let mut command = serde_json::to_vec(
            &serde_json::json!({"version":1,"id":"inspect","type":"inspect","directory":directory}),
        )
        .map_err(Error::network)?;
        command.push(b'\n');
        if command.len() > 4 * 1024 * 1024 {
            return Err(Error::new("path", "目录名称过长"));
        }
        timeout(Duration::from_secs(5), self.input.write_all(&command))
            .await
            .map_err(Error::network)?
            .map_err(Error::network)?;
        let event = self
            .read_event_with_timeout(Duration::from_secs(120))
            .await?;
        if event["version"] != 1 || event["id"] != "inspect" {
            return Err(Error::new("sidecar_protocol", "后台响应无效"));
        }
        if event["type"] == "error" {
            let message = match event["error"].as_str() {
                Some("inspection_limit") => "目录超过扫描或文件大小上限，请缩小目录或调整忽略规则",
                Some("inspection_changed") => "扫描时文件或目录发生变化，请停止编辑后重试",
                Some("inspection_unsafe") => "目录包含链接、不兼容路径或忽略规则，请检查后重试",
                Some("inspection_unsupported_volume") => {
                    "请选择内置固定磁盘上的目录；网络、外置或无法确认的磁盘暂不支持同步"
                }
                Some("inspection_canceled") => "检查已取消",
                _ => "无法检查目录，请确认目录权限及本地磁盘状态",
            };
            return Err(Error::new("inspection", message));
        }
        if event["type"] != "inspected" || !event["inspection"].is_object() {
            return Err(Error::new("sidecar_protocol", "后台检查结果无效"));
        }
        Ok(event["inspection"].clone())
    }

    async fn sync_command(
        &mut self,
        value: Value,
        id: &str,
        mut progress: impl FnMut(Progress),
    ) -> Result<Value> {
        let mut command = serde_json::to_vec(&value).map_err(Error::network)?;
        command.push(b'\n');
        if command.len() > 4 * 1024 * 1024 {
            return Err(Error::new("limit", "同步预览超过消息上限"));
        }
        timeout(Duration::from_secs(10), self.input.write_all(&command))
            .await
            .map_err(Error::network)?
            .map_err(Error::network)?;
        let deadline = tokio::time::Instant::now() + Duration::from_secs(15 * 60);
        let mut sequence = 0;
        let mut count = 0;
        let event = loop {
            let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
            let event = self.read_event_with_timeout(remaining).await?;
            if event["version"] != 1 || event["id"] != id {
                return Err(Error::new("sidecar_protocol", "同步后台响应无效"));
            }
            if event["type"] != "sync_progress" {
                break event;
            }
            count += 1;
            let update: Progress = serde_json::from_value(event["progress"].clone())
                .map_err(|_| Error::new("sidecar_protocol", "同步进度无效"))?;
            if value["progress"] != true
                || !update.valid()
                || update.sequence <= sequence
                || count > 10_000
            {
                return Err(Error::new("sidecar_protocol", "同步进度超过限制或顺序无效"));
            }
            sequence = update.sequence;
            progress(update);
        };
        if event["type"] == "error" {
            if let Some(error) =
                transient_preview_error(value["type"].as_str(), event["error"].as_str())
            {
                return Err(error);
            }
            let message = match event["error"].as_str() {
                Some("sync_canceled") => "同步已取消；未完成操作会保留待核对记录",
                Some("sync_identity") => "当前登录身份与同步请求不匹配",
                Some("sync_disabled") => "服务端尚未开启同步功能",
                Some("sync_pending") => "上一次同步尚未核对完成，请先处理待定操作",
                Some("sync_binding") => "同步绑定已失效，请重新选择目录并绑定",
                Some("sync_stale") => "记录或预览已过期，请重新加载后再操作",
                Some("sync_rules_changed") => "忽略规则发生变化，请重新检查同步范围",
                Some("sync_changed") => "同步期间文件发生变化，请重新预览",
                Some("sync_lease_expired") => "同步租约已失效，请重试",
                Some("sync_limit") => "同步日志或目录超过容量限制",
                Some("sync_unsupported") => "此服务端尚不支持服务器同步历史清理",
                Some("sync_unsupported_volume") => {
                    "请选择内置固定磁盘上的目录；网络、外置或无法确认的磁盘暂不支持同步"
                }
                _ => "同步失败，请检查目录和服务器状态",
            };
            return Err(Error::new("sync", message));
        }
        let valid = match value["type"].as_str() {
            Some("sync_orphan_list") => {
                event["type"] == "sync_orphan_listed"
                    && event["orphan_page"]["items"]
                        .as_array()
                        .is_some_and(|items| items.len() <= 50)
                    && event["orphan_page"]["next_cursor"].is_string()
            }
            Some("sync_orphan_review") => {
                event["type"] == "sync_orphan_reviewed" && event["orphan_review"].is_object()
            }
            Some("sync_orphan_retire") => {
                event["type"] == "sync_orphan_retired" && event["orphan_review"].is_object()
            }
            Some("sync_orphan_export") => {
                event["type"] == "sync_orphan_exported" && event["filename"].is_string()
            }
            Some("sync_bind") => event["type"] == "sync_bound" && event["binding"].is_object(),
            Some("sync_abandon_review") => {
                event["type"] == "sync_abandon_reviewed" && event["abandon"].is_object()
            }
            Some("sync_abandon") => {
                event["type"] == "sync_abandoned" && event["binding"].is_object()
            }
            Some("sync_recovery_discard") => {
                event["type"] == "sync_recovery_discarded" && event["binding"].is_object()
            }
            Some("sync_remote_cleanup_review") => {
                event["type"] == "sync_remote_cleanup_reviewed"
                    && event["cleanup"].is_object()
                    && event["cleanup"]["items"]
                        .as_array()
                        .is_some_and(|items| items.len() <= 1000)
            }
            Some("sync_remote_cleanup") => {
                event["type"] == "sync_remote_cleaned" && event["binding"].is_object()
            }
            Some("sync_history_cleanup") => {
                event["type"] == "sync_history_cleaned" && event["binding"].is_object()
            }
            Some("sync_archive") => {
                event["type"] == "sync_archived" && event["binding"].is_object()
            }
            Some("sync_list") => {
                event["type"] == "sync_listed"
                    && (event["bindings"].is_array() || event["bindings"].is_null())
            }
            Some("sync_preview") => {
                event["type"] == "sync_previewed" && event["preview"].is_object()
            }
            Some("sync_apply") => event["type"] == "sync_applied" && event["binding"].is_object(),
            Some("sync_review") => event["type"] == "sync_reviewed" && event["review"].is_object(),
            Some("sync_resolve") => {
                event["type"] == "sync_resolved" && event["binding"].is_object()
            }
            Some("sync_history") => {
                event["type"] == "sync_history"
                    && event["page"]["history"].is_array()
                    && event["page"]["next_cursor"].is_string()
                    && event["page"]["revision"].as_u64().is_some()
            }
            Some("sync_export") => {
                event["type"] == "sync_exported" && event["filename"].is_string()
            }
            _ => false,
        };
        if !valid {
            return Err(Error::new("sidecar_protocol", "同步结果无效"));
        }
        Ok(event)
    }

    // Only native callers construct this request; credentials and absolute
    // paths never originate in the renderer command payload.
    pub async fn sync_request(&mut self, value: Value) -> Result<Value> {
        self.sync_command(value, "sync", |_| {}).await
    }

    pub async fn sync_request_progress(
        &mut self,
        mut value: Value,
        progress: impl FnMut(Progress),
    ) -> Result<Value> {
        value["progress"] = serde_json::json!(true);
        self.sync_command(value, "sync", progress).await
    }

    pub async fn stop(self) {
        let _ = self.stop_with_grace(Duration::from_secs(3)).await;
    }

    // Updates must know the old executable is no longer in use before handing
    // its bundle to an installer. Ordinary cancellation still uses best effort.
    pub async fn stop_for_update(self) -> Result<()> {
        self.stop_with_grace(Duration::from_secs(3))
            .await
            .ok_or_else(|| Error::new("sidecar_stop", "后台尚未确认退出，请稍后重新检查更新"))?;
        Ok(())
    }

    async fn stop_with_grace(mut self, grace: Duration) -> Option<std::process::ExitStatus> {
        drop(self.input);
        // EOF cancels inspection and lets Go close its pinned handles. On macOS
        // observe exit without reaping, preserving the leader PID until the
        // whole group (including any stuck diskutil) has been signalled.
        #[cfg(target_os = "macos")]
        {
            let _ = timeout(grace, self.group.wait_for_exit()).await;
            self.group.terminate();
            timeout(Duration::from_secs(1), self.child.wait())
                .await
                .ok()?
                .ok()
        }
        #[cfg(not(target_os = "macos"))]
        {
            timeout(grace, self.child.wait()).await.ok()?.ok()
        }
    }
}

#[cfg(target_os = "macos")]
mod macos_group {
    use super::*;
    use std::io;

    // Constructed only from our process_group(0) child. The leader must never be
    // reaped while this guard is armed: a zombie still reserves its PID/PGID.
    // WNOWAIT lets graceful shutdown and early-exit detection keep that fence.
    pub(super) struct Group {
        leader: Option<libc::pid_t>,
    }

    impl Group {
        pub(super) fn new(child: &Child) -> Result<Self> {
            let leader = child
                .id()
                .and_then(|id| libc::pid_t::try_from(id).ok())
                .filter(|id| *id > 1)
                .ok_or_else(|| Error::new("sidecar_group", "无法监管后台进程，已取消启动"))?;
            // The command configures its private group atomically before exec.
            // Never signal our application's group or an unexpected group.
            if unsafe { libc::getpgid(leader) != leader || libc::getpgrp() == leader } {
                return Err(Error::new("sidecar_group", "无法监管后台进程，已取消启动"));
            }
            Ok(Self {
                leader: Some(leader),
            })
        }

        fn observe(leader: libc::pid_t) -> io::Result<bool> {
            let mut info = unsafe { std::mem::zeroed::<libc::siginfo_t>() };
            // P_PID only accepts a direct, unreaped child owned by this process.
            // No wildcard wait or process enumeration is used.
            let result = unsafe {
                libc::waitid(
                    libc::P_PID,
                    leader as libc::id_t,
                    &mut info,
                    libc::WEXITED | libc::WNOHANG | libc::WNOWAIT,
                )
            };
            if result != 0 {
                return Err(io::Error::last_os_error());
            }
            Ok(info.si_pid == leader)
        }

        pub(super) fn exited(&self) -> io::Result<bool> {
            match self.leader {
                Some(leader) => Self::observe(leader),
                None => Ok(true),
            }
        }

        pub(super) async fn wait_for_exit(&self) -> io::Result<()> {
            loop {
                match self.exited() {
                    Ok(true) => return Ok(()),
                    Ok(false) => {}
                    Err(error) if error.kind() == io::ErrorKind::Interrupted => {}
                    Err(error) => return Err(error),
                }
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
        }

        pub(super) fn terminate(&mut self) {
            let Some(leader) = self.leader.take() else {
                return;
            };
            if leader <= 1 || unsafe { libc::getpgrp() == leader } {
                return;
            }
            // A reaped/lost child must never authorize signalling a possibly
            // reused PGID. EINTR retries are bounded even from a Drop path.
            for _ in 0..3 {
                match Self::observe(leader) {
                    Ok(_) => {
                        // No await/reap between ownership proof and this kill;
                        // even an asynchronously exiting leader retains its PID.
                        unsafe { libc::kill(-leader, libc::SIGKILL) };
                        return;
                    }
                    Err(error) if error.kind() == io::ErrorKind::Interrupted => continue,
                    Err(_) => return,
                }
            }
        }
    }

    impl Drop for Group {
        fn drop(&mut self) {
            self.terminate();
        }
    }
}

#[cfg(all(test, target_os = "macos"))]
mod macos_group_tests {
    use super::*;
    use std::{os::unix::fs::PermissionsExt, path::PathBuf};

    struct WorkerFixture {
        _directory: tempfile::TempDir,
        worker: PathBuf,
        helper_pid: PathBuf,
        graceful: PathBuf,
    }

    impl WorkerFixture {
        fn new(mode: &str) -> Self {
            let directory = tempfile::tempdir().unwrap();
            let worker = directory.path().join("worker.sh");
            let helper_pid = directory.path().join("helper.pid");
            let graceful = directory.path().join("graceful");
            let quote =
                |path: &Path| format!("'{}'", path.to_str().unwrap().replace('\'', "'\\''"));
            let ending = match mode {
                "graceful" => format!(
                    "while IFS= read -r line; do :; done\nkill \"$helper\"\nwait \"$helper\" 2>/dev/null\nprintf done > {}\nexit 0\n",
                    quote(&graceful)
                ),
                "early_exit" => "exit 0\n".to_owned(),
                "blocked" => "wait \"$helper\"\n".to_owned(),
                _ => panic!("invalid synthetic worker mode"),
            };
            let script = format!(
                "#!/bin/sh\n/bin/sleep 300 &\nhelper=$!\nprintf '%s\\n' \"$helper\" > {}\nprintf '%s\\n' '{{\"version\":1,\"type\":\"ready\",\"capabilities\":[]}}'\n{}",
                quote(&helper_pid), ending
            );
            std::fs::write(&worker, script).unwrap();
            std::fs::set_permissions(&worker, std::fs::Permissions::from_mode(0o700)).unwrap();
            Self {
                _directory: directory,
                worker,
                helper_pid,
                graceful,
            }
        }

        fn helper(&self) -> i32 {
            std::fs::read_to_string(&self.helper_pid)
                .unwrap()
                .trim()
                .parse()
                .unwrap()
        }
    }

    async fn assert_gone(pid: i32) {
        timeout(Duration::from_secs(5), async {
            loop {
                if unsafe { libc::kill(pid, 0) } != 0
                    && std::io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH)
                {
                    return;
                }
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
        })
        .await
        .unwrap_or_else(|_| panic!("synthetic process {pid} survived group cleanup"));
    }

    #[tokio::test]
    async fn macos_group_drop_cleans_worker_and_helper_only() {
        let fixture = WorkerFixture::new("blocked");
        let mut unrelated = Command::new("/bin/sleep")
            .arg("300")
            .kill_on_drop(true)
            .spawn()
            .unwrap();
        let sidecar = Sidecar::start(&fixture.worker).await.unwrap();
        let leader = sidecar.child.id().unwrap() as i32;
        assert_eq!(unsafe { libc::getpgid(leader) }, leader);
        assert_ne!(leader, unsafe { libc::getpgrp() });
        let helper = fixture.helper();
        assert_eq!(unsafe { libc::getpgid(helper) }, leader);
        drop(sidecar);
        assert_gone(leader).await;
        assert_gone(helper).await;
        assert!(unrelated.try_wait().unwrap().is_none());
        unrelated.kill().await.unwrap();
    }

    #[tokio::test]
    async fn macos_group_timeout_cleans_worker_and_helper() {
        let fixture = WorkerFixture::new("blocked");
        let sidecar = Sidecar::start(&fixture.worker).await.unwrap();
        let leader = sidecar.child.id().unwrap() as i32;
        let helper = fixture.helper();
        let status = sidecar
            .stop_with_grace(Duration::from_millis(100))
            .await
            .unwrap();
        assert!(!status.success());
        assert_gone(leader).await;
        assert_gone(helper).await;
        assert!(!fixture.graceful.exists());
    }

    #[tokio::test]
    async fn macos_group_preserves_graceful_eof() {
        let fixture = WorkerFixture::new("graceful");
        let sidecar = Sidecar::start(&fixture.worker).await.unwrap();
        let helper = fixture.helper();
        assert!(sidecar
            .stop_with_grace(Duration::from_secs(3))
            .await
            .unwrap()
            .success());
        assert_eq!(std::fs::read_to_string(&fixture.graceful).unwrap(), "done");
        assert_gone(helper).await;
    }

    #[tokio::test]
    async fn macos_group_exited_leader_stays_reserved_until_helper_cleanup() {
        let fixture = WorkerFixture::new("early_exit");
        let mut sidecar = Sidecar::start(&fixture.worker).await.unwrap();
        let helper = fixture.helper();
        timeout(Duration::from_secs(3), sidecar.group.wait_for_exit())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(unsafe { libc::kill(helper, 0) }, 0);
        // Ping must use WNOWAIT too. Its failure retires the group before any
        // Child.wait reaps the zombie leader and permits PID/PGID reuse.
        assert!(sidecar.ping().await.is_err());
        assert!(sidecar.group.exited().unwrap());
        assert_gone(helper).await;
        drop(sidecar);
    }
}

#[cfg(windows)]
mod windows_job {
    use super::*;
    use windows_sys::Win32::{
        Foundation::{CloseHandle, HANDLE},
        System::JobObjects::{
            AssignProcessToJobObject, CreateJobObjectW, JobObjectExtendedLimitInformation,
            SetInformationJobObject, JOBOBJECT_EXTENDED_LIMIT_INFORMATION,
            JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
        },
    };

    pub struct Job(HANDLE);
    // This exclusively owned kernel handle is only closed by Drop; moving it
    // across executor threads is supported by the Windows handle API.
    unsafe impl Send for Job {}
    impl Job {
        pub fn assign(child: &Child) -> Result<Self> {
            let process = child
                .raw_handle()
                .ok_or_else(|| Error::new("sidecar_exit", "后台已退出"))?;
            unsafe {
                let handle = CreateJobObjectW(std::ptr::null(), std::ptr::null());
                if handle.is_null() {
                    return Err(Error::new("sidecar_job", "无法创建后台进程监管"));
                }
                let job = Self(handle);
                let mut limits: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = std::mem::zeroed();
                limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
                if SetInformationJobObject(
                    handle,
                    JobObjectExtendedLimitInformation,
                    &limits as *const _ as *const _,
                    std::mem::size_of_val(&limits) as u32,
                ) == 0
                    || AssignProcessToJobObject(handle, process as HANDLE) == 0
                {
                    return Err(Error::new("sidecar_job", "无法监管后台进程，已取消启动"));
                }
                Ok(job)
            }
        }
    }
    impl Drop for Job {
        fn drop(&mut self) {
            unsafe {
                CloseHandle(self.0);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn transient_preview_kind_is_never_forwarded_for_a_write_or_unclassified_failure() {
        assert_eq!(
            transient_preview_error(Some("sync_preview"), Some("sync_preview_retryable"))
                .unwrap()
                .kind,
            "sync_preview_retryable"
        );
        for command in [
            None,
            Some("sync_apply"),
            Some("sync_resolve"),
            Some("sync_bind"),
            Some("sync_orphan_retire"),
        ] {
            assert!(transient_preview_error(command, Some("sync_preview_retryable")).is_none());
        }
        for code in [
            None,
            Some("network"),
            Some("sync_failed"),
            Some("sync_identity"),
            Some("sync_canceled"),
            Some("sync_pending"),
        ] {
            assert!(transient_preview_error(Some("sync_preview"), code).is_none());
        }
    }
    #[tokio::test]
    async fn compiled_sidecar_handshake_ping_and_parent_eof() {
        let target = if cfg!(target_os = "windows") {
            "x86_64-pc-windows-msvc.exe"
        } else if cfg!(target_arch = "aarch64") {
            "aarch64-apple-darwin"
        } else {
            "x86_64-apple-darwin"
        };
        let path =
            Path::new(env!("CARGO_MANIFEST_DIR")).join(format!("binaries/abox-sync-{target}"));
        let mut sidecar = Sidecar::start(&path)
            .await
            .expect("run npm run sidecar before cargo test");
        sidecar.ping().await.unwrap();
        let directory = std::env::temp_dir().join(format!(
            "agentbox-sidecar-inspect-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir(&directory).unwrap();
        std::fs::write(directory.join("fixture.txt"), b"hello\r\n").unwrap();
        let inspected = sidecar.inspect(&directory.canonicalize().unwrap()).await;
        std::fs::remove_dir_all(&directory).unwrap();
        let inspected = inspected.unwrap();
        assert_eq!(inspected["files"], 1);
        assert_eq!(inspected["bytes"], 7);
        sidecar.ping().await.unwrap();
        assert!(sidecar
            .stop_with_grace(Duration::from_secs(3))
            .await
            .unwrap()
            .success());
    }
    #[tokio::test]
    async fn compiled_sidecar_binding_survives_worker_restart() {
        use sha2::{Digest, Sha256};
        use tokio::{io::AsyncReadExt, net::TcpListener};
        let target = if cfg!(windows) {
            "x86_64-pc-windows-msvc.exe"
        } else if cfg!(target_arch = "aarch64") {
            "aarch64-apple-darwin"
        } else {
            "x86_64-apple-darwin"
        };
        let binary =
            Path::new(env!("CARGO_MANIFEST_DIR")).join(format!("binaries/abox-sync-{target}"));
        let base_dir = std::env::temp_dir().join(format!(
            "agentbox-sync-wire-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(base_dir.join("local")).unwrap();
        let base_dir = base_dir.canonicalize().unwrap();
        // Windows canonicalize returns the native extended prefix. The Go
        // boundary handles that representation through its native drive parser.
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = format!("http://{}/", listener.local_addr().unwrap());
        let server_id = format!("{:x}", Sha256::digest(b"wire-server"));
        let rules = format!("{:x}", Sha256::digest(b"agentbox-ignore-v1\n"));
        let manifest = format!(r#"{{"version":1,"rules_hash":"{rules}","entries":{{}}}}"#);
        let digest = format!("{:x}", Sha256::digest(manifest.as_bytes()));
        let caps = serde_json::json!({"protocol_version":1,"server_id":server_id,"user":"alice","features":{"sync":1}}).to_string();
        let tree = format!(
            r#"{{"manifest":{manifest},"digest":"{digest}","project":"project","project_revision":1,"project_path":"."}}"#
        );
        let server = tokio::spawn(async move {
            for (route, status, body) in [
                ("/api/clients/capabilities", "200 OK", caps.clone()),
                (
                    "/api/sessions/space/sync/manifest?project=project",
                    "200 OK",
                    tree,
                ),
                ("/api/clients/capabilities", "200 OK", caps),
                (
                    "/api/clients/capabilities",
                    "503 Service Unavailable",
                    "{}".into(),
                ),
                (
                    "/api/clients/capabilities",
                    "503 Service Unavailable",
                    "{}".into(),
                ),
            ] {
                let (mut socket, _) = listener.accept().await.unwrap();
                let mut headers = Vec::new();
                while !headers.ends_with(b"\r\n\r\n") {
                    headers.push(socket.read_u8().await.unwrap());
                    assert!(headers.len() < 8192);
                }
                let headers = String::from_utf8(headers).unwrap();
                assert!(headers.starts_with(&format!("GET {route} HTTP/1.1")));
                assert!(headers
                    .to_lowercase()
                    .contains("authorization: bearer private-sync-fixture"));
                assert!(!headers
                    .lines()
                    .next()
                    .unwrap()
                    .contains("private-sync-fixture"));
                let response=format!("HTTP/1.1 {status}\r\nContent-Type: application/json\r\nX-Agentbox-Server-ID: {server_id}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",body.len());
                socket.write_all(response.as_bytes()).await.unwrap();
            }
        });
        let common = serde_json::json!({"version":1,"id":"sync","server":address,"token":"private-sync-fixture","user":"alice","state_dir":base_dir.join("state")});
        let mut request = common.clone();
        request["type"] = serde_json::json!("sync_bind");
        request["directory"] = serde_json::json!(base_dir.join("local"));
        request["binding"] =
            serde_json::json!({"workspace":"space","project":"project","project_path":"."});
        let mut worker = Sidecar::start(&binary).await.unwrap();
        let mut progress = Vec::new();
        let bound = worker
            .sync_request_progress(request, |update| progress.push(update))
            .await
            .unwrap();
        assert!(!progress.is_empty());
        assert!(progress.iter().all(|update| update.valid()));
        assert_eq!(bound["binding"]["binding"]["user"], "alice");
        assert!(bound["binding"].get("baseline").is_none());
        assert!(!bound.to_string().contains("private-sync-fixture"));
        worker.stop().await;
        let mut worker = Sidecar::start(&binary).await.unwrap();
        let mut request = common.clone();
        request["type"] = serde_json::json!("sync_list");
        let listed = worker.sync_request(request).await.unwrap();
        assert_eq!(listed["bindings"][0]["id"], bound["binding"]["id"]);
        // Exercise the compiled Go process through the real Rust command path:
        // identical transient identity failures permit only a read-only preview
        // retry, never a write retry. No mutation route is requested.
        for (command, expected_kind) in [
            ("sync_preview", "sync_preview_retryable"),
            ("sync_apply", "sync"),
        ] {
            let mut request = common.clone();
            request["type"] = serde_json::json!(command);
            request["binding_id"] = bound["binding"]["id"].clone();
            let error = worker.sync_request(request).await.unwrap_err();
            assert_eq!(error.kind, expected_kind, "{command}");
        }
        worker.stop().await;
        timeout(Duration::from_secs(3), server)
            .await
            .unwrap()
            .unwrap();
        std::fs::remove_dir_all(base_dir).unwrap();
    }
}
