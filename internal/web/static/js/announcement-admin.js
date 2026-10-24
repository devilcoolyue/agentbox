/* 「系统设置 → 系统通告」：发布 / 撤下全站横幅，以及「现在重启会影响谁」。
 * 横幅本身在 announcement.ts；这里只管管理员页面。在线列表来自推送连接数，
 * 工作空间列表是挂着终端/对话连接、有对话回合在跑或 30 分钟内用过的空间。 */
import { api } from "./api.js";
import { announcementTitle } from "./announcement.js";
import { setTextRender, t } from "./i18n.js";
import { setSelectValue } from "./select.js";
import { S, bus } from "./state.js";
import { toast } from "./toast.js";
import { $, askConfirm, btnBusy, btnDone, fmtAgo, fmtTime, keepScroll } from "./util.js";
const REFRESH = 15_000;
let current = null;
let skew = 0;
let timer = null;
let page = null;
let wired = false;
const serverNow = () => Date.now() + skew;
function renderCurrent() {
    const box = $("ann-current");
    const meta = $("ann-current-meta");
    $("ann-clear").classList.toggle("hidden", !current);
    box.replaceChildren();
    if (!current) {
        setTextRender(box, () => t("暂无通告"));
        box.classList.add("note");
        meta.textContent = "";
        return;
    }
    box.classList.remove("note");
    const a = current;
    const title = document.createElement("strong");
    setTextRender(title, () => announcementTitle(a, serverNow()));
    box.append(title);
    if (a.message && !a.completed_at) {
        const msg = document.createElement("p");
        msg.className = "ann-message";
        msg.textContent = a.message;
        box.append(msg);
    }
    setTextRender(meta, () => t("{p0} 于 {p1} 发布", { p0: a.posted_by, p1: fmtTime(a.posted_at) }));
}
function renderActivity(v) {
    const online = v.online.map(u => u.pages > 1 ? t("{p0}（{p1} 个页面）", { p0: u.user, p1: u.pages }) : u.user);
    setTextRender($("ann-online"), () => online.length
        ? t("开着页面的用户（{p0}）：{p1}", { p0: online.length, p1: online.join(t("、")) })
        : t("现在没有用户开着页面。"));
    const body = $("ann-tbody");
    body.replaceChildren();
    if (!v.workspaces.length) {
        const row = body.insertRow();
        const cell = row.insertCell();
        cell.colSpan = 4;
        cell.className = "note";
        setTextRender(cell, () => t("30 分钟内没有工作空间在用，现在发版不会打断任何人。"));
        return;
    }
    for (const w of v.workspaces) {
        const row = body.insertRow();
        row.insertCell().textContent = w.name || w.id;
        row.insertCell().textContent = w.user;
        const state = row.insertCell();
        setTextRender(state, () => {
            const parts = [];
            if (w.active_turns)
                parts.push(t("{p0} 个对话回合在运行", { p0: w.active_turns }));
            if (w.attached)
                parts.push(t("{p0} 个终端/对话连接", { p0: w.attached }));
            return parts.length ? parts.join(t("，")) : t("空闲");
        });
        if (w.active_turns || w.attached)
            state.classList.add("ann-busy");
        const seen = row.insertCell();
        seen.textContent = w.last_active ? fmtAgo(w.last_active) : "—";
    }
}
async function loadCurrent(signal) {
    try {
        const v = await api("/announcement", { signal });
        skew = v.now ? v.now - Date.now() : 0;
        current = v.announcement;
        renderCurrent();
    }
    catch (e) {
        if (!signal.aborted)
            toast(t("读取系统通告失败：") + e.message, true);
    }
}
async function loadActivity(signal) {
    try {
        const v = await api("/announcement/activity", { signal });
        keepScroll($("ann-tbody"), () => renderActivity(v)); // 15 秒一刷，别把往下翻着看的人拽回顶
    }
    catch (e) {
        if (!signal.aborted)
            toast(t("读取在线情况失败：") + e.message, true);
    }
}
function syncForm() {
    const release = $("ann-kind").value === "release";
    $("ann-release-fields").classList.toggle("hidden", !release);
    $("ann-at-field").classList.toggle("hidden", $("ann-lead").value !== "at");
}
function formInput() {
    const kind = $("ann-kind").value;
    const message = $("ann-message").value.trim();
    if (kind === "notice") {
        if (!message) {
            toast(t("普通通知需要填写内容"), true);
            return null;
        }
        return { kind, message, starts_at: 0, duration_min: 0 };
    }
    const lead = $("ann-lead").value;
    let startsAt;
    if (lead === "at") {
        startsAt = new Date($("ann-at").value).getTime();
        if (!Number.isFinite(startsAt)) {
            toast(t("请选择开始时间"), true);
            return null;
        }
    }
    else {
        startsAt = serverNow() + Number(lead) * 60_000;
    }
    const duration = Number($("ann-duration").value || 0);
    if (!Number.isInteger(duration) || duration < 0 || duration > 240) {
        toast(t("预计中断时间须在 0–240 分钟之间"), true);
        return null;
    }
    return { kind, message, starts_at: startsAt, duration_min: duration };
}
function wire() {
    if (wired)
        return;
    wired = true;
    $("ann-kind").addEventListener("change", syncForm);
    $("ann-lead").addEventListener("change", syncForm);
    $("ann-form").addEventListener("submit", async (e) => {
        e.preventDefault();
        const input = formInput();
        if (!input)
            return;
        const ok = await askConfirm(() => t("发布后所有登录用户的页面顶部都会显示这条通告。"), {
            title: t("发布系统通告"), okLabel: t("发布"), icon: "send",
            hint: current ? t("会替换当前通告。") : undefined,
        });
        if (!ok)
            return;
        const btn = $("ann-post");
        btnBusy(btn, () => t("发布中…"));
        try {
            const v = await api("/announcement", { method: "PUT", body: JSON.stringify(input) });
            current = v.announcement;
            renderCurrent();
            $("ann-message").value = "";
            toast(t("通告已发布"));
        }
        catch (err) {
            toast(t("发布失败：") + err.message, true);
        }
        finally {
            btnDone(btn);
        }
    });
    $("ann-clear").addEventListener("click", async () => {
        const ok = await askConfirm(() => t("撤下后所有页面上的横幅会消失。"), { title: t("撤下通告"), okLabel: t("撤下"), icon: "trash", danger: true });
        if (!ok)
            return;
        const btn = $("ann-clear");
        btnBusy(btn, () => t("撤下中…"));
        try {
            await api("/announcement", { method: "DELETE" });
            current = null;
            renderCurrent();
            toast(t("通告已撤下"));
        }
        catch (err) {
            toast(t("撤下失败：") + err.message, true);
        }
        finally {
            btnDone(btn);
        }
    });
    $("ann-refresh").addEventListener("click", () => { if (page)
        void loadActivity(page.signal); });
}
/** 进入分区：读当前通告与在线情况，之后每 15 秒刷新在线情况与倒计时。 */
export function openAnnouncementAdmin(signal) {
    wire();
    leaveAnnouncementAdmin();
    const ctl = new AbortController();
    page = ctl;
    signal.addEventListener("abort", () => ctl.abort(), { once: true });
    setSelectValue($("ann-kind"), "release");
    syncForm();
    const onChanged = () => void loadCurrent(ctl.signal);
    bus.addEventListener("announcement-changed", onChanged, { signal: ctl.signal });
    void loadCurrent(ctl.signal);
    void loadActivity(ctl.signal);
    timer = setInterval(() => {
        // 切到别的视图时控制器不一定调 leave：同运维监控，在这里自己停。
        if (S.view !== "settings" || S.sec !== "announce") {
            leaveAnnouncementAdmin();
            return;
        }
        keepScroll($("ann-current"), renderCurrent);
        void loadActivity(ctl.signal);
    }, REFRESH);
}
/** 离开分区：停刷新、作废未完成的请求。 */
export function leaveAnnouncementAdmin() {
    if (timer) {
        clearInterval(timer);
        timer = null;
    }
    page?.abort();
    page = null;
}
