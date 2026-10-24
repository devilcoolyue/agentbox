/* 系统通告横幅。管理员在「系统设置 → 系统通告」发布，或发布脚本经
 * `agentbox announce` 写入；登录后所有页面在主区顶部显示。
 *
 * 变更经 usage-live 的同一条推送连接到达（"announcement-changed"），所以登录期间
 * 一直挂着它；推送连不上时每 2 分钟读一次兜底，切回标签页时也读一次。发版类显示
 * 倒计时，到点后显示「正在发版」；新服务启动后服务端把它改成「已升级到 vX」，
 * 10 分钟后不再返回。用户可以收起；重新发布（ID 变了）或升级完成会再出现。 */
import { bus } from "./state.js";
import { api } from "./api.js";
import { setTextRender, t } from "./i18n.js";
import { usageLiveConnected, watchUsageLive } from "./usage-live.js";
import { $, fmtTime } from "./util.js";
const DISMISSED = "agentbox_announcement_dismissed";
const FALLBACK_POLL = 120_000;
const TICK = 15_000;
/** 离发版不到这么久时横幅加强提示 */
const SOON = 5 * 60_000;
export function announcementPhase(a, now) {
    if (a.kind === "notice")
        return "notice";
    if (a.completed_at)
        return "done";
    return a.starts_at && now < a.starts_at ? "scheduled" : "live";
}
function remaining(ms) {
    const minutes = Math.ceil(ms / 60_000);
    if (minutes <= 1)
        return t("不到 1 分钟");
    if (minutes < 60)
        return t("还有 {p0} 分钟", { p0: minutes });
    return t("还有 {p0} 小时 {p1} 分钟", { p0: Math.floor(minutes / 60), p1: minutes % 60 });
}
/** 横幅的标题行；管理员页的预览也用它。message 另起一段，原样显示。 */
export function announcementTitle(a, now) {
    const downtime = a.duration_min || 0;
    switch (announcementPhase(a, now)) {
        case "notice":
            return t("管理员通知");
        case "done":
            return t("服务已升级到 {p0}，可以正常使用。", { p0: a.completed_version || "" });
        case "live":
            return downtime
                ? t("正在发版升级，预计中断约 {p0} 分钟，页面和终端可能短暂断开。", { p0: downtime })
                : t("正在发版升级，页面和终端可能短暂断开。");
        case "scheduled": {
            const at = fmtTime(a.starts_at), left = remaining(a.starts_at - now);
            return downtime
                ? t("计划于 {p0} 发版升级（{p1}），预计中断约 {p2} 分钟。请先保存进度，避免在此期间开始长任务。", { p0: at, p1: left, p2: downtime })
                : t("计划于 {p0} 发版升级（{p1}）。请先保存进度，避免在此期间开始长任务。", { p0: at, p1: left });
        }
    }
}
let current = null;
let skew = 0; // 服务端时钟 - 本机时钟
let lastFetch = 0;
let seq = 0;
const serverNow = () => Date.now() + skew;
function dismissKey(a) {
    return a.id + (a.completed_at ? ":done" : "");
}
function dismissed(a) {
    try {
        return localStorage.getItem(DISMISSED) === dismissKey(a);
    }
    catch {
        return false;
    }
}
function render() {
    const bar = $("announce-bar");
    const a = current;
    if (!a || dismissed(a)) {
        bar.hidden = true;
        return;
    }
    const now = serverNow();
    const phase = announcementPhase(a, now);
    bar.dataset.phase = phase;
    bar.classList.toggle("soon", phase === "scheduled" && a.starts_at - now <= SOON);
    setTextRender($("announce-title"), () => announcementTitle(a, serverNow()));
    const msg = $("announce-msg");
    msg.textContent = phase === "done" ? "" : a.message;
    msg.hidden = !msg.textContent;
    bar.hidden = false;
}
async function refresh() {
    const mine = ++seq;
    lastFetch = Date.now();
    try {
        const v = await api("/announcement");
        if (mine !== seq)
            return;
        skew = v.now ? v.now - Date.now() : 0;
        current = v.announcement;
    }
    catch {
        return; // 读不到就保持原样；离线横幅会说明连接问题
    }
    render();
}
/** 登录期间的生命周期。 */
export function initAnnouncement() {
    const bar = $("announce-bar");
    const close = $("announce-close");
    const onClose = () => {
        if (current)
            try {
                localStorage.setItem(DISMISSED, dismissKey(current));
            }
            catch { /* 存不了就只收起这一次 */ }
        bar.hidden = true;
    };
    const onChanged = () => void refresh();
    const onVisible = () => { if (document.visibilityState === "visible")
        void refresh(); };
    close.addEventListener("click", onClose);
    bus.addEventListener("announcement-changed", onChanged);
    document.addEventListener("visibilitychange", onVisible);
    const tick = setInterval(() => {
        if (!usageLiveConnected() && Date.now() - lastFetch >= FALLBACK_POLL)
            void refresh();
        else
            render();
    }, TICK);
    watchUsageLive("announcement", true);
    void refresh();
    return () => {
        seq++;
        clearInterval(tick);
        close.removeEventListener("click", onClose);
        bus.removeEventListener("announcement-changed", onChanged);
        document.removeEventListener("visibilitychange", onVisible);
        watchUsageLive("announcement", false);
        current = null;
        bar.hidden = true;
    };
}
