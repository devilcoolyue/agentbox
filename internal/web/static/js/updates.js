import { api } from "./api.js";
import { S, emit } from "./state.js";
import { $, askConfirm, fmtDateTime, fmtTime } from "./util.js";
import { hideTip } from "./tip.js";
import { buttonLabel } from "./icons.js";
const interval = 4 * 60 * 60 * 1000;
const releasesURL = "https://github.com/devilcoolyue/agentbox/releases";
const versionLabel = (version) => /^\d/.test(version) ? "v" + version : version;
/** App-owned state: logout aborts requests, listeners and scheduled checks. */
export function initUpdates() {
    const badge = $("version-badge");
    const menu = $("version-menu");
    badge.classList.toggle("hidden", S.role !== "admin");
    if (S.role !== "admin")
        return () => { };
    const lifetime = new AbortController();
    const { signal } = lifetime;
    let info;
    let checking = false;
    let error = "";
    let lastAttempt = 0;
    let timer;
    let upgrade;
    let upgradeError = "";
    let submissionError = "";
    let submitting = false;
    let readingUpgrade = false;
    let awaitingSubmission = false;
    let upgradeTimer;
    let pollingUntil = 0;
    const upgrading = () => !!upgrade?.job && !["succeeded", "failed"].includes(upgrade.job.phase);
    function renderUpgrade() {
        const job = upgrade?.job;
        const busy = submitting || awaitingSubmission || upgrading();
        const install = $("update-install");
        buttonLabel(install, info?.comparable === false ? "切换到正式版并重启" : "升级并重启", "download");
        install.classList.toggle("hidden", !upgrade?.supported || !info?.available);
        install.disabled = busy || readingUpgrade || checking || !!error;
        $("upgrade-support").textContent = upgrade ? upgrade.reason : "正在读取在线升级支持状态…";
        $("upgrade-progress").classList.toggle("hidden", !job && !upgradeError && !submissionError && !submitting && !awaitingSubmission);
        const verified = job?.phase === "succeeded" && upgrade?.current_version === job.version;
        $("upgrade-message").textContent = submitting ? "正在提交升级任务…" : awaitingSubmission ? "正在确认升级任务是否已提交…"
            : verified ? `升级完成，当前运行 ${job.version}。刷新页面加载新版界面。`
                : job?.phase === "succeeded" ? "任务已结束，但当前运行版本与目标不一致，请检查服务器。"
                    : job ? `${job.version} · ${job.message}` : "";
        const detail = upgradeError || submissionError || job?.error || "";
        $("upgrade-error").textContent = detail;
        $("upgrade-error").classList.toggle("hidden", !detail);
        $("upgrade-log").textContent = job ? `任务日志：journalctl -u agentbox-upgrade-${job.id}.service` : "";
        $("upgrade-log").classList.toggle("hidden", !detail && job?.phase !== "failed" && !(job?.phase === "succeeded" && !verified));
        $("upgrade-reload").classList.toggle("hidden", !verified);
        $("upgrade-refresh").disabled = readingUpgrade || submitting;
    }
    function scheduleUpgrade() {
        clearTimeout(upgradeTimer);
        if (signal.aborted || document.hidden || !navigator.onLine)
            return;
        if ((upgrading() || awaitingSubmission || upgradeError) && Date.now() < pollingUntil) {
            upgradeTimer = setTimeout(() => void readUpgrade(), 3000);
        }
        else if ((upgrading() || awaitingSubmission) && pollingUntil && Date.now() >= pollingUntil) {
            upgradeError = "长时间未能确认升级结果，自动查询已暂停。请刷新升级状态或在服务器检查任务日志。";
            renderUpgrade();
        }
    }
    async function readUpgrade() {
        if (readingUpgrade || submitting || signal.aborted)
            return;
        readingUpgrade = true;
        renderUpgrade();
        try {
            const result = await api("/updates/upgrade", { signal: AbortSignal.any([signal, AbortSignal.timeout(25000)]) });
            if (signal.aborted)
                return;
            upgrade = result;
            upgradeError = "";
            awaitingSubmission = false;
            if (upgrading())
                submissionError = "";
            if (upgrading() && !pollingUntil)
                pollingUntil = Date.now() + 32 * 60 * 1000;
            if (upgrade.job?.phase === "succeeded" && info && upgrade.current_version === upgrade.job.version && info.current_version !== upgrade.current_version) {
                info.current_version = upgrade.current_version;
                // Clear stale build details until metadata is fetched from the new process.
                info.revision = "unknown";
                info.built_at = "unknown";
                info.comparable = true;
                if (info.latest_version === upgrade.current_version)
                    info.available = false;
                render();
            }
        }
        catch (e) {
            if (!signal.aborted)
                upgradeError = upgrading() || awaitingSubmission
                    ? "暂时无法连接服务，正在等待恢复。若持续无法恢复，请在服务器查看服务和升级任务日志。"
                    : "读取升级状态失败：" + e.message;
        }
        finally {
            readingUpgrade = false;
            if (!signal.aborted) {
                renderUpgrade();
                scheduleUpgrade();
            }
        }
    }
    async function installUpdate() {
        if (!upgrade?.supported || !info?.available || submitting || readingUpgrade || upgrading() || awaitingSubmission)
            return;
        const version = info.latest_version;
        const switching = !info.comparable;
        // Lock the UI while the dialog is open; duplicate clicks cannot stack it.
        submitting = true;
        renderUpgrade();
        const confirmed = await askConfirm(switching ? `从开发构建 ${info.current_version} 切换至正式版 ${version} 并重启服务？` : `升级至 ${version} 并重启服务？`, {
            title: switching ? "切换到正式版" : "升级服务端", okLabel: switching ? "切换并重启" : "升级并重启", icon: "download",
            hint: (switching ? "正式版可能不包含当前开发功能；配置或数据库不兼容时会阻止切换。" : "") + "升级前会备份系统数据。正在进行的对话可能中断，网页连接会短暂断开；工作空间文件保留，会话镜像单独管理。",
        });
        if (signal.aborted)
            return;
        if (!confirmed) {
            submitting = false;
            renderUpgrade();
            return;
        }
        upgradeError = "";
        submissionError = "";
        pollingUntil = Date.now() + 32 * 60 * 1000;
        try {
            upgrade = await api("/updates/upgrade", {
                method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ version }),
                signal: AbortSignal.any([signal, AbortSignal.timeout(25000)]),
            });
        }
        catch (e) {
            if (!signal.aborted) {
                submissionError = e.message;
                // A lost response doesn't mean the independent task failed to start.
                awaitingSubmission = true;
            }
        }
        finally {
            submitting = false;
            if (!signal.aborted) {
                renderUpgrade();
                void readUpgrade();
            }
        }
    }
    function render() {
        const available = !!info?.available;
        const version = info ? versionLabel(info.current_version) : "—";
        const availableStatus = info?.comparable ? `新版本 ${versionLabel(info.latest_version)} 可用` : `可切换至正式版 ${versionLabel(info?.latest_version || "")}`;
        let status = "尚未检查更新";
        if (checking)
            status = "正在检查更新…";
        else if (error)
            status = available ? `${availableStatus}（上次检查结果）` : "检查未完成，请重试";
        else if (available)
            status = availableStatus;
        else if (info?.checked_at) {
            status = !info.latest_version ? "暂无正式发布的版本" : !info.comparable ? "开发构建，无法比较版本" : "已是最新版本";
        }
        badge.classList.toggle("has-update", available);
        $("version-label").textContent = info ? version : "版本";
        badge.ariaLabel = `当前版本 ${version}，${status}，查看版本与更新`;
        for (const el of document.querySelectorAll("[data-update-version]"))
            el.textContent = version;
        for (const el of document.querySelectorAll("[data-update-status]")) {
            el.textContent = status;
            el.classList.toggle("available", available);
        }
        for (const el of document.querySelectorAll("[data-update-error]")) {
            el.textContent = error;
            el.classList.toggle("hidden", !error);
        }
        for (const el of document.querySelectorAll("[data-update-check]")) {
            el.disabled = checking;
            el.classList.toggle("checking", checking);
        }
        for (const el of document.querySelectorAll("[data-update-release]")) {
            // Release links are constrained even when metadata is stale or malformed.
            const url = info?.release_url || releasesURL;
            el.href = url === releasesURL || url.startsWith(releasesURL + "/tag/") ? url : releasesURL;
        }
        const checked = info?.checked_at ? "上次检查 " + fmtTime(info.checked_at) : "尚未检查";
        for (const el of document.querySelectorAll("[data-update-time]"))
            el.textContent = checked;
        $("version-current").classList.toggle("hidden", !(info?.checked_at && info.latest_version && info.comparable && !available && !error && !checking));
        $("version-upgrade").classList.toggle("hidden", !available);
        $("update-build").textContent = info ? [info.built_at !== "unknown" ? "构建于 " + (isNaN(Date.parse(info.built_at)) ? info.built_at : fmtDateTime(info.built_at, false)) : "", info.revision !== "unknown" ? "提交 " + info.revision.slice(0, 12) : ""].filter(Boolean).join(" · ") : "";
        $("update-notes").textContent = info?.notes || "";
        $("update-notes-wrap").classList.toggle("hidden", !info?.notes);
        positionMenu();
        renderUpgrade();
    }
    function schedule() {
        clearTimeout(timer);
        if (signal.aborted || document.hidden || !navigator.onLine)
            return;
        timer = setTimeout(() => void check(false), Math.max(0, lastAttempt + interval - Date.now()));
    }
    async function check(force) {
        if (checking || signal.aborted)
            return;
        checking = true;
        error = "";
        lastAttempt = Date.now();
        render();
        try {
            const result = await api("/updates/check" + (force ? "?force=1" : ""), { method: "POST", signal });
            if (signal.aborted)
                return;
            info = result;
            error = result.error;
            lastAttempt = result.attempted_at || lastAttempt;
        }
        catch (e) {
            if (!signal.aborted)
                error = "检查更新失败：" + e.message;
        }
        finally {
            checking = false;
            if (!signal.aborted) {
                render();
                schedule();
                void readUpgrade();
            }
        }
    }
    function positionMenu() {
        if (!menu.matches(":popover-open"))
            return;
        const rect = badge.getBoundingClientRect();
        menu.style.left = Math.max(12, Math.min(rect.right - menu.offsetWidth, window.innerWidth - menu.offsetWidth - 12)) + "px";
        menu.style.top = Math.max(12, Math.min(rect.bottom + 10, window.innerHeight - menu.offsetHeight - 12)) + "px";
    }
    badge.addEventListener("click", () => {
        hideTip();
        menu.togglePopover();
        positionMenu();
        if (menu.matches(":popover-open"))
            menu.querySelector("button")?.focus();
    }, { signal });
    menu.addEventListener("keydown", e => {
        if (e.key === "Escape") {
            e.preventDefault();
            e.stopPropagation();
            menu.hidePopover();
            badge.focus();
        }
    }, { signal });
    menu.addEventListener("toggle", () => { badge.ariaExpanded = String(menu.matches(":popover-open")); }, { signal });
    window.addEventListener("resize", positionMenu, { signal });
    for (const el of document.querySelectorAll("[data-update-check]"))
        el.addEventListener("click", () => void check(true), { signal });
    for (const el of document.querySelectorAll("[data-update-about]"))
        el.addEventListener("click", () => {
            menu.hidePopover();
            S.sec = "about";
            emit("open-settings");
        }, { signal });
    document.addEventListener("visibilitychange", schedule, { signal });
    window.addEventListener("online", schedule, { signal });
    window.addEventListener("offline", schedule, { signal });
    $("update-install").addEventListener("click", () => void installUpdate(), { signal });
    $("upgrade-refresh").addEventListener("click", () => {
        pollingUntil = Date.now() + 32 * 60 * 1000;
        void readUpgrade();
    }, { signal });
    $("upgrade-reload").addEventListener("click", () => location.reload(), { signal });
    document.addEventListener("visibilitychange", scheduleUpgrade, { signal });
    window.addEventListener("online", scheduleUpgrade, { signal });
    window.addEventListener("offline", scheduleUpgrade, { signal });
    render();
    void readUpgrade();
    // Render local build metadata before making the (potentially slow) upstream request.
    void (async () => {
        try {
            const result = await api("/updates", { signal });
            if (signal.aborted || checking || lastAttempt)
                return;
            info = result;
            error = result.error;
            lastAttempt = result.attempted_at;
            render();
        }
        catch { /* The scheduled check surfaces errors and allows a retry. */ }
        if (!signal.aborted)
            schedule();
    })();
    return () => {
        lifetime.abort();
        clearTimeout(timer);
        clearTimeout(upgradeTimer);
        menu.hidePopover();
        badge.classList.add("hidden");
        badge.ariaExpanded = "false";
    };
}
