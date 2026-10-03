//! One native-owned sync task at a time. Paths come from the picker/app data
//! directory; bearer credentials come from the current native connection.
use crate::sync_progress::{ProgressEvent, Relay};
use crate::{
    remote::{Error, Result},
    sidecar::Sidecar,
    Desktop,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::sync::atomic::Ordering;
use std::sync::Arc;
use tauri::{ipc::Channel, AppHandle, Manager, State};
use tokio::sync::oneshot;

// The renderer chooses identities, never credentials or local source paths.
// Each call pins the instance shown by the current connection and the Go peer
// rechecks ownership and the instance again before network/file operations.
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct OrphanQuery {
    workspace: String,
    server_id: String,
    #[serde(default)]
    device: String,
    #[serde(default)]
    cursor: String,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct OrphanOperation {
    workspace: String,
    server_id: String,
    device: String,
    operation_id: String,
}

#[tauri::command]
pub async fn sync_orphan_list(
    app: AppHandle,
    state: State<'_, Desktop>,
    query: OrphanQuery,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_orphan_list",
        json!(query),
        false,
        Some(events),
    )
    .await
}

#[tauri::command]
pub async fn sync_orphan_review(
    app: AppHandle,
    state: State<'_, Desktop>,
    operation: OrphanOperation,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_orphan_review",
        json!(operation),
        false,
        Some(events),
    )
    .await
}

#[tauri::command]
pub async fn sync_orphan_export(
    app: AppHandle,
    state: State<'_, Desktop>,
    operation: OrphanOperation,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_orphan_export",
        json!(operation),
        false,
        Some(events),
    )
    .await
}

#[tauri::command]
pub async fn sync_orphan_retire(
    app: AppHandle,
    state: State<'_, Desktop>,
    operation: OrphanOperation,
    confirmation: String,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    let mut payload = json!(operation);
    payload["confirmation"] = json!(confirmation);
    run(
        app,
        state,
        "sync_orphan_retire",
        payload,
        false,
        Some(events),
    )
    .await
}

fn require_capability(
    capabilities: Option<&crate::remote::Capabilities>,
    kind: &str,
    server_id: Option<&str>,
) -> Result<()> {
    if matches!(
        kind,
        "sync_orphan_list" | "sync_orphan_review" | "sync_orphan_export" | "sync_orphan_retire"
    ) {
        let capabilities = capabilities
            .ok_or_else(|| Error::new("unsupported", "此服务端尚不支持独立恢复记录管理"))?;
        if capabilities.features.sync_recovery_inspect != 1 {
            return Err(Error::new(
                "unsupported",
                "此服务端尚不支持独立恢复记录管理",
            ));
        }
        if kind == "sync_orphan_retire" && capabilities.features.sync_recovery_gc != 1 {
            return Err(Error::new(
                "unsupported",
                "此服务端尚不支持服务器恢复内容清理",
            ));
        }
        if capabilities.server_id.len() != 64
            || !capabilities
                .server_id
                .bytes()
                .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
            || server_id != Some(capabilities.server_id.as_str())
        {
            return Err(Error::new(
                "sync_identity",
                "服务器身份已变化，请重新登录后核对",
            ));
        }
    } else if capabilities.is_none_or(|caps| caps.features.sync != 1) {
        return Err(Error::new("unsupported", "此服务端尚未开启桌面文件同步"));
    }
    Ok(())
}

async fn run(
    app: AppHandle,
    state: State<'_, Desktop>,
    kind: &str,
    payload: Value,
    pick: bool,
    events: Option<Channel<ProgressEvent>>,
) -> Result<Value> {
    let _exclusive = state
        .sync_work
        .try_lock()
        .map_err(|_| Error::new("sync_busy", "已有同步操作正在进行"))?;
    let (cancel, mut canceled) = oneshot::channel();
    let (remote, user, progress) = {
        let mut data = state.inner.lock().await;
        if data.disconnecting {
            return Err(Error::new("state", "退出登录进行中，请重试退出"));
        }
        let connection = data
            .connection
            .as_ref()
            .ok_or_else(|| Error::new("unauthorized", "请先登录"))?;
        require_capability(
            connection.capabilities.as_ref(),
            kind,
            payload["server_id"].as_str(),
        )?;
        let user = connection.user.clone();
        let remote = data
            .remote
            .clone()
            .ok_or_else(|| Error::new("unauthorized", "请先登录"))?;
        data.sync_cancel = Some(cancel);
        let progress = events.map(|events| {
            Arc::new(Relay::new(
                (state.next_sync.fetch_add(1, Ordering::Relaxed) + 1).to_string(),
                events,
            ))
        });
        data.sync_progress = progress.clone();
        (remote, user, progress)
    };
    let mut worker: Option<Sidecar> = None;
    let task = async {
        let mut request = payload;
        request["version"] = json!(1);
        request["id"] = json!("sync");
        request["type"] = json!(kind);
        request["server"] = json!(remote.base.as_str());
        request["user"] = json!(user);
        request["token"] = json!(remote.token());
        // An established HTTP connection already required explicit opt-in at
        // login (or loopback). Do not ask again or upgrade another identity.
        request["allow_http"] = json!(remote.base.scheme() == "http");
        let directory = app
            .path()
            .app_data_dir()
            .map_err(|_| Error::new("path", "无法定位应用数据目录"))?
            .join("sync");
        #[cfg(feature = "desktop-smoke")]
        let directory = app
            .state::<crate::smoke::Fixture>()
            .sync
            .as_ref()
            .map(|fixture| fixture.state.clone())
            .unwrap_or(directory);
        request["state_dir"] = json!(directory
            .to_str()
            .ok_or_else(|| Error::new("path", "应用数据路径无法用 UTF-8 表示"))?);
        let exporting = matches!(kind, "sync_export" | "sync_orphan_export");
        if pick || exporting {
            let title = if exporting {
                "选择恢复副本导出目录（项目目录之外）"
            } else {
                "选择同步的本地项目目录"
            };
            #[cfg(feature = "desktop-smoke")]
            let injected = app
                .state::<crate::smoke::Fixture>()
                .sync
                .as_ref()
                .map(|fixture| fixture.selected(exporting));
            #[cfg(not(feature = "desktop-smoke"))]
            let injected: Option<std::path::PathBuf> = None;
            let selected = if injected.is_some() {
                injected
            } else {
                rfd::AsyncFileDialog::new()
                    .set_title(title)
                    .pick_folder()
                    .await
                    .map(|selected| selected.path().to_path_buf())
            };
            let Some(selected) = selected else {
                return Ok(Value::Null);
            };
            let field = if exporting {
                "export_directory"
            } else {
                "directory"
            };
            request[field] = json!(selected
                .to_str()
                .ok_or_else(|| Error::new("path", "目录名称无法用 UTF-8 表示"))?);
        }
        let exe = std::env::current_exe().map_err(Error::network)?;
        let parent = exe
            .parent()
            .ok_or_else(|| Error::new("sidecar_path", "无法定位后台程序"))?;
        let name = if cfg!(windows) {
            "abox-sync.exe"
        } else {
            "abox-sync"
        };
        worker = Some(Sidecar::start(&parent.join(name)).await?);
        let worker = worker.as_mut().expect("started");
        if let Some(progress) = &progress {
            worker
                .sync_request_progress(request, |update| progress.offer(update))
                .await
        } else {
            worker.sync_request(request).await
        }
    };
    let result = tokio::select! {
        biased;
        _ = &mut canceled => Err(Error::new("sync_canceled", "同步已取消；已开始的操作将保留待核对状态")),
        result = task => result,
    };
    if let Some(progress) = &progress {
        progress.close();
    }
    if let Some(worker) = worker {
        worker.stop().await;
    }
    let mut data = state.inner.lock().await;
    data.sync_progress = None;
    if !data
        .remote
        .as_ref()
        .is_some_and(|current| Arc::ptr_eq(current, &remote))
        || data.sync_cancel.take().is_none()
    {
        return Err(Error::new("sync_canceled", "同步已取消或登录已改变"));
    }
    result
}

#[tauri::command]
pub async fn sync_bind(
    app: AppHandle,
    state: State<'_, Desktop>,
    session: String,
    project: String,
    project_path: String,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_bind",
        json!({"binding":{"workspace":session,"project":project,"project_path":project_path}}),
        true,
        Some(events),
    )
    .await
}
#[tauri::command]
pub async fn sync_list(
    app: AppHandle,
    state: State<'_, Desktop>,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(app, state, "sync_list", json!({}), false, Some(events)).await
}
#[tauri::command]
pub async fn sync_preview(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    direction: String,
    choices: Option<std::collections::BTreeMap<String, String>>,
    basis_digest: Option<String>,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_preview",
        json!({"binding_id":binding_id,"direction":direction,"choices":choices,"basis_digest":basis_digest.unwrap_or_default()}),
        false,
        Some(events),
    )
    .await
}
#[tauri::command]
pub async fn sync_apply(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    preview: Value,
    confirmation: String,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_apply",
        json!({"binding_id":binding_id,"preview":preview,"confirmation":confirmation}),
        false,
        Some(events),
    )
    .await
}
#[tauri::command]
pub async fn sync_cancel(state: State<'_, Desktop>) -> Result<()> {
    let mut data = state.inner.lock().await;
    if let Some(progress) = data.sync_progress.take() {
        progress.close();
    }
    if let Some(cancel) = data.sync_cancel.take() {
        let _ = cancel.send(());
    }
    Ok(())
}

#[tauri::command]
pub async fn sync_review(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_review",
        json!({"binding_id":binding_id}),
        false,
        Some(events),
    )
    .await
}
#[tauri::command]
pub async fn sync_resolve(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    confirmation: String,
    action: String,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_resolve",
        json!({"binding_id":binding_id,"confirmation":confirmation,"action":action}),
        false,
        Some(events),
    )
    .await
}
#[tauri::command]
pub async fn sync_history(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    cursor: Option<String>,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_history",
        json!({"binding_id":binding_id,"cursor":cursor.unwrap_or_default()}),
        false,
        Some(events),
    )
    .await
}
#[tauri::command]
pub async fn sync_export(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    batch_id: String,
    operation_id: String,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_export",
        json!({"binding_id":binding_id,"batch_id":batch_id,"operation_id":operation_id}),
        false,
        Some(events),
    )
    .await
}

#[tauri::command]
pub async fn sync_progress_ack(
    state: State<'_, Desktop>,
    task: String,
    sequence: u64,
) -> Result<()> {
    let data = state.inner.lock().await;
    if let Some(progress) = &data.sync_progress {
        if progress.id == task {
            progress.ack(sequence);
        }
    }
    Ok(())
}

#[tauri::command]
pub async fn sync_archive(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    revision: i64,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_archive",
        json!({"binding_id":binding_id,"revision":revision}),
        false,
        Some(events),
    )
    .await
}

#[tauri::command]
pub async fn sync_abandon_review(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_abandon_review",
        json!({"binding_id":binding_id}),
        false,
        Some(events),
    )
    .await
}
#[tauri::command]
pub async fn sync_abandon(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    confirmation: String,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_abandon",
        json!({"binding_id":binding_id,"confirmation":confirmation}),
        false,
        Some(events),
    )
    .await
}

#[tauri::command]
pub async fn sync_history_cleanup(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    batch_id: String,
    revision: i64,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_history_cleanup",
        json!({"binding_id":binding_id,"batch_id":batch_id,"revision":revision}),
        false,
        Some(events),
    )
    .await
}

#[tauri::command]
pub async fn sync_recovery_discard(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    batch_id: String,
    operation_id: String,
    revision: i64,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_recovery_discard",
        json!({"binding_id":binding_id,"batch_id":batch_id,"operation_id":operation_id,"revision":revision}),
        false,
        Some(events),
    )
    .await
}

#[tauri::command]
pub async fn sync_remote_cleanup_review(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    batch_id: String,
    revision: i64,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_remote_cleanup_review",
        json!({"binding_id":binding_id,"batch_id":batch_id,"revision":revision}),
        false,
        Some(events),
    )
    .await
}

#[tauri::command]
pub async fn sync_remote_cleanup(
    app: AppHandle,
    state: State<'_, Desktop>,
    binding_id: String,
    batch_id: String,
    revision: i64,
    confirmation: String,
    events: Channel<ProgressEvent>,
) -> Result<Value> {
    run(
        app,
        state,
        "sync_remote_cleanup",
        json!({"binding_id":binding_id,"batch_id":batch_id,"revision":revision,"confirmation":confirmation}),
        false,
        Some(events),
    )
    .await
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::remote::{Capabilities, Features};

    #[test]
    fn recovery_maintenance_does_not_enable_sync_or_cross_instances() {
        let mut caps = Capabilities {
            protocol_version: 1,
            server_id: "a".repeat(64),
            features: Features {
                sync_recovery_inspect: 1,
                sync_recovery_gc: 1,
                ..Features::default()
            },
        };
        for kind in [
            "sync_orphan_list",
            "sync_orphan_review",
            "sync_orphan_export",
            "sync_orphan_retire",
        ] {
            assert!(require_capability(Some(&caps), kind, Some(&caps.server_id)).is_ok());
            assert!(require_capability(Some(&caps), kind, Some(&"b".repeat(64))).is_err());
            assert!(require_capability(None, kind, Some(&caps.server_id)).is_err());
        }
        assert!(require_capability(Some(&caps), "sync_apply", None).is_err());
        assert!(require_capability(Some(&caps), "sync_bind", None).is_err());
        caps.features.sync_recovery_gc = 0;
        assert!(
            require_capability(Some(&caps), "sync_orphan_review", Some(&caps.server_id)).is_ok()
        );
        assert!(
            require_capability(Some(&caps), "sync_orphan_retire", Some(&caps.server_id)).is_err()
        );
        caps.features.sync = 1;
        caps.features.sync_recovery_inspect = 0;
        assert!(require_capability(Some(&caps), "sync_apply", None).is_ok());
        assert!(
            require_capability(Some(&caps), "sync_orphan_retire", Some(&caps.server_id)).is_err()
        );
    }
}
