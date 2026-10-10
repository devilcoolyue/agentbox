import { setAttrRender, setTextRender, t as i18nText } from "./i18n.js";
import { api } from "./api.js";
import { S, emit } from "./state.js";
import { $, askConfirm, fmtBytes, fmtDateTime, fmtTime } from "./util.js";
import { hideTip } from "./tip.js";
import { buttonLabel } from "./icons.js";
import { initUpdateComponents } from "./features/settings/update-components.js";
import { markdownPreview } from "./markdown-preview.js";
import { ProgressBar, RateMeter } from "./progress.js";
const interval = 4 * 60 * 60 * 1000;
const releasesURL = "https://github.com/devilcoolyue/agentbox/releases";
const versionLabel = (version) => /^\d/.test(version) ? "v" + version : version;
/* 升级任务的步骤（update.py 的 phase）与各步在整条进度里的权重：下载与备份最耗时。
 * 权重只是估计，用来让进度条大致匀速；准确的只有下载那一段（按字节）。 */
const UPGRADE_STEPS = ["downloading", "verifying", "staging", "checking", "stopping", "backup", "switching", "restarting", "health"];
const STEP_WEIGHT = { downloading: 40, verifying: 6, staging: 6, checking: 4, stopping: 4, backup: 20, switching: 3, restarting: 10, health: 7 };
const stepNames = {
    downloading: () => i18nText("下载"), verifying: () => i18nText("校验"), staging: () => i18nText("安装"),
    checking: () => i18nText("兼容检查"), stopping: () => i18nText("停止服务"), backup: () => i18nText("备份数据"),
    switching: () => i18nText("切换版本"), restarting: () => i18nText("启动"), health: () => i18nText("验证"),
};
/* 任务状态文案按 phase 本地翻译；认不出的 phase 用服务端给的原文 */
const phaseMessages = {
    queued: () => i18nText("升级任务已提交"), downloading: () => i18nText("正在下载发布包"),
    verifying: () => i18nText("正在校验发布包"), staging: () => i18nText("正在安装新版本"),
    checking: () => i18nText("正在检查配置和数据库兼容性"), stopping: () => i18nText("正在停止服务"),
    backup: () => i18nText("正在备份并验证数据"), switching: () => i18nText("正在切换版本"),
    restarting: () => i18nText("正在启动新版本"), health: () => i18nText("正在验证服务状态"),
    succeeded: () => i18nText("升级完成"), failed: () => i18nText("升级失败，请查看升级任务日志"),
};
/** 整条进度（0～1）；还说不出进度（刚提交、旧版脚本不报下载字节）时为 null */
function upgradeFraction(job) {
    if (job.phase === "succeeded")
        return 1;
    const phase = job.phase === "failed" ? job.failed_phase || "" : job.phase;
    const index = UPGRADE_STEPS.indexOf(phase);
    if (index < 0)
        return null;
    if (phase === "downloading" && !job.total)
        return job.phase === "failed" ? 0 : null;
    const before = UPGRADE_STEPS.slice(0, index).reduce((sum, step) => sum + STEP_WEIGHT[step], 0);
    const within = phase === "downloading" ? Math.min(1, (job.downloaded || 0) / job.total) : 0;
    return (before + STEP_WEIGHT[phase] * within) / 100;
}
/** App-owned state: logout aborts requests, listeners and scheduled checks. */
export function initUpdates() {
    const badge = $("version-badge");
    const menu = $("version-menu");
    badge.classList.toggle("hidden", S.role !== "admin");
    if (S.role !== "admin")
        return () => { };
    const disposeComponents = initUpdateComponents();
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
    let renderedNotes;
    const upgrading = () => !!upgrade?.job && !["succeeded", "failed"].includes(upgrade.job.phase);
    const meter = new ProgressBar(() => i18nText("升级进度"));
    const downloadRate = new RateMeter();
    let rateJob = "";
    $("upgrade-meter").replaceChildren(meter.el);
    const stepItems = UPGRADE_STEPS.map(step => {
        const li = document.createElement("li");
        setTextRender(li, stepNames[step]);
        return li;
    });
    $("upgrade-steps").replaceChildren(...stepItems);
    /* 进度条、百分比、下载字节与步骤条。服务重启期间读不到状态，停在最后一次读到的位置。 */
    function renderMeter(job, verified) {
        for (const id of ["upgrade-meter", "upgrade-steps"])
            $(id).classList.toggle("hidden", !job);
        if (!job) {
            $("upgrade-pct").textContent = "";
            $("upgrade-detail").classList.add("hidden");
            return;
        }
        const value = upgradeFraction(job), failed = job.phase === "failed", running = !failed && job.phase !== "succeeded";
        // 失败时停在出错的那一步并转红；旧版脚本没记下是哪一步，就整条标红
        meter.set(value ?? (failed ? 1 : null), running).tone(failed ? "error" : verified ? "ok" : "");
        $("upgrade-pct").textContent = value === null || failed ? "" : Math.floor(value * 100) + "%";
        if (rateJob !== job.id) {
            rateJob = job.id;
            downloadRate.reset();
        }
        const downloading = job.phase === "downloading" && !!job.total;
        const speed = downloading ? downloadRate.sample(job.downloaded || 0, job.updated_at) : 0;
        setTextRender($("upgrade-detail"), () => downloading
            ? i18nText("已下载 {p0} / {p1}", { p0: fmtBytes(job.downloaded || 0), p1: fmtBytes(job.total) }) + (speed > 0 ? " · " + fmtBytes(Math.round(speed)) + "/s" : "")
            : "");
        $("upgrade-detail").classList.toggle("hidden", !downloading);
        const phase = failed ? job.failed_phase || "" : job.phase;
        const current = job.phase === "succeeded" ? UPGRADE_STEPS.length : UPGRADE_STEPS.indexOf(phase);
        stepItems.forEach((li, i) => {
            li.className = i < current ? "done" : i === current ? (failed ? "failed" : "current") : "";
            if (i === current)
                li.setAttribute("aria-current", "step");
            else
                li.removeAttribute("aria-current");
        });
    }
    function renderUpgrade() {
        const job = upgrade?.job;
        const busy = submitting || awaitingSubmission || upgrading();
        const install = $("update-install");
        buttonLabel(install, () => info?.comparable === false ? i18nText("切换到正式版并重启") : i18nText("升级并重启"), "download");
        install.classList.toggle("hidden", !upgrade?.supported || !info?.available);
        install.disabled = busy || readingUpgrade || checking || !!error;
        setTextRender($("upgrade-support"), () => upgrade ? upgrade.reason : i18nText("正在读取在线升级支持状态…"));
        $("upgrade-progress").classList.toggle("hidden", !job && !upgradeError && !submissionError && !submitting && !awaitingSubmission);
        const verified = job?.phase === "succeeded" && upgrade?.current_version === job.version;
        setTextRender($("upgrade-message"), () => submitting ? i18nText("正在提交升级任务…") : awaitingSubmission ? i18nText("正在确认升级任务是否已提交…")
            : verified ? i18nText("升级完成，当前运行 {p0}。刷新页面加载新版界面。", { p0: String(job.version) })
                : job?.phase === "succeeded" ? i18nText("任务已结束，但当前运行版本与目标不一致，请检查服务器。")
                    : job ? `${job.version} · ${phaseMessages[job.phase]?.() || job.message}` : "");
        renderMeter(job, verified);
        const detail = upgradeError || submissionError || job?.error || "";
        $("upgrade-error").textContent = detail;
        $("upgrade-error").classList.toggle("hidden", !detail);
        setTextRender($("upgrade-log"), () => job ? i18nText("任务日志：journalctl -u agentbox-upgrade-{p0}.service", { p0: String(job.id) }) : "");
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
            upgradeError = i18nText("长时间未能确认升级结果，自动查询已暂停。请刷新升级状态或在服务器检查任务日志。");
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
                    ? i18nText("暂时无法连接服务，正在等待恢复。若持续无法恢复，请在服务器查看服务和升级任务日志。")
                    : i18nText("读取升级状态失败：") + e.message;
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
        const version = info.latest_version, currentVersion = info.current_version;
        const switching = !info.comparable;
        // Lock the UI while the dialog is open; duplicate clicks cannot stack it.
        submitting = true;
        renderUpgrade();
        const confirmed = await askConfirm(() => switching ? i18nText("从开发构建 {p0} 切换至正式版 {p1} 并重启服务？", { p0: String(currentVersion), p1: String(version) }) : i18nText("升级至 {p0} 并重启服务？", { p0: String(version) }), {
            get title() { return switching ? i18nText("切换到正式版") : i18nText("升级服务端"); }, get okLabel() { return switching ? i18nText("切换并重启") : i18nText("升级并重启"); }, icon: "download",
            get hint() { return (switching ? i18nText("正式版可能不包含当前开发功能；配置或数据库不兼容时会阻止切换。") : "") + i18nText("升级前会备份系统数据。正在进行的对话可能中断，网页连接会短暂断开；工作空间文件保留，会话镜像单独管理。"); },
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
        const availableStatus = info?.comparable ? i18nText("新版本 {p0} 可用", { p0: String(versionLabel(info.latest_version)) }) : i18nText("可切换至正式版 {p0}", { p0: String(versionLabel(info?.latest_version || "")) });
        let status = i18nText("尚未检查更新");
        if (checking)
            status = i18nText("正在检查更新…");
        else if (error)
            status = available ? i18nText("{p0}（上次检查结果）", { p0: String(availableStatus) }) : i18nText("检查未完成，请重试");
        else if (available)
            status = availableStatus;
        else if (info?.checked_at) {
            status = !info.latest_version ? i18nText("暂无正式发布的版本") : !info.comparable ? i18nText("开发构建，无法比较版本") : i18nText("已是最新版本");
        }
        badge.classList.toggle("has-update", available);
        setTextRender($("version-label"), () => info ? version : i18nText("版本"));
        setAttrRender(badge, "aria-label", () => i18nText("当前版本 {p0}，{p1}，查看版本与更新", { p0: String(version), p1: String(status) }));
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
        const checked = info?.checked_at ? i18nText("上次检查 ") + fmtTime(info.checked_at) : i18nText("尚未检查");
        for (const el of document.querySelectorAll("[data-update-time]"))
            el.textContent = checked;
        $("version-current").classList.toggle("hidden", !(info?.checked_at && info.latest_version && info.comparable && !available && !error && !checking));
        $("version-upgrade").classList.toggle("hidden", !available);
        setTextRender($("update-build"), () => info ? [info.built_at !== "unknown" ? i18nText("构建于 ") + (isNaN(Date.parse(info.built_at)) ? info.built_at : fmtDateTime(info.built_at, false)) : "", info.revision !== "unknown" ? i18nText("提交 ") + info.revision.slice(0, 12) : ""].filter(Boolean).join(" · ") : "");
        const notes = info?.notes || "";
        // GitHub release body is Markdown. It goes through the file preview's sanitizer;
        // only absolute https images load and repository-relative links stay inert.
        // Re-parse only on change so polling keeps the reader's scroll and selection.
        if (notes !== renderedNotes) {
            renderedNotes = notes;
            $("update-notes").replaceChildren(...notes ? [markdownPreview(notes, {
                    image: src => /^https:\/\//i.test(src.trim()) ? src.trim() : "",
                    file: () => undefined,
                }).content] : []);
        }
        $("update-notes-wrap").classList.toggle("hidden", !notes);
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
                error = i18nText("检查更新失败：") + e.message;
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
        disposeComponents();
        lifetime.abort();
        clearTimeout(timer);
        clearTimeout(upgradeTimer);
        menu.hidePopover();
        badge.classList.add("hidden");
        badge.ariaExpanded = "false";
    };
}
