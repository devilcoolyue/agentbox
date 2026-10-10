import { setText, setTextRender, t as i18nText } from "./i18n.js";
/* acct-usage：查看当前会话所用 Claude 账号的订阅额度。
 *
 * 数据来自服务端代查的官方 /usage 接口（见 server/acctusage.go），展示的东西
 * 与 CLI 里 claude-hud 状态栏、/usage 命令是同一份：5 小时窗口 + 周窗口的
 * 使用率和重置时间。只读，不做任何写操作。 */
"use strict";
import { S } from "./state.js";
import { $, btnBusy, btnDone, fmtClock } from "./util.js";
import { api } from "./api.js";
import { keepCount, skelBar, skelShell } from "./skeleton.js";
const dlg = () => $("dlg-ausage");
/* 订阅档位代号 → 展示名。认不出的原样显示，别把新档位吃掉。 */
const PLAN = { pro: "Pro", max: "Max", team: "Team", enterprise: "Enterprise" };
/* 使用率分档，决定进度条与数字的颜色：八成以内正常，八成起转琥珀，
 * 满格转红。和状态栏的观感保持一致。 */
function tone(pct) {
    if (pct >= 100)
        return "full";
    if (pct >= 80)
        return "warn";
    return "";
}
/* 距离重置还有多久。超过一天按「N 天 N 小时」，一小时内只报分钟；
 * 已经过点了说明上游还没滚窗口，显示「即将重置」而不是负数。 */
function untilReset(iso) {
    if (!iso)
        return "";
    const ms = new Date(iso).getTime() - Date.now();
    if (!Number.isFinite(ms))
        return "";
    if (ms <= 0)
        return i18nText("即将重置");
    const mins = Math.floor(ms / 60000);
    const h = Math.floor(mins / 60);
    const d = Math.floor(h / 24);
    if (d >= 1)
        return i18nText("{p0} 天 {p1} 小时后重置", { p0: String(d), p1: String(h % 24) });
    if (h >= 1)
        return i18nText("{p0} 小时 {p1} 分钟后重置", { p0: String(h), p1: String(mins % 60) });
    return i18nText("{p0} 分钟后重置", { p0: String(mins) });
}
function winRow(w) {
    const row = document.createElement("div");
    row.className = "au-row";
    const head = document.createElement("div");
    head.className = "au-row-head";
    const label = document.createElement("span");
    label.className = "au-row-label";
    const labels = { five_hour: "5 小时额度", seven_day: "周额度", seven_day_opus: "Opus 周额度", seven_day_sonnet: "Sonnet 周额度", extra: "额外用量" };
    if (labels[w.key])
        setText(label, labels[w.key]);
    else
        label.textContent = w.label;
    const pct = document.createElement("span");
    pct.className = "au-row-pct mono " + tone(w.percent);
    pct.textContent = Math.round(w.percent) + "%";
    head.append(label, pct);
    const track = document.createElement("div");
    track.className = "au-track";
    const fill = document.createElement("div");
    fill.className = "au-fill " + tone(w.percent);
    // 上游偶尔会给出略超 100 的值，夹住免得填充溢出轨道。
    fill.style.width = Math.max(0, Math.min(100, w.percent)) + "%";
    track.appendChild(fill);
    row.append(head, track);
    const reset = untilReset(w.resets_at);
    if (reset) {
        const sub = document.createElement("div");
        sub.className = "au-row-sub";
        setTextRender(sub, () => untilReset(w.resets_at));
        row.appendChild(sub);
    }
    return row;
}
function extraRow(x) {
    const cur = x.currency === "USD" ? "$" : (x.currency || "") + " ";
    const row = winRow({
        key: "extra",
        label: i18nText("额外用量"),
        percent: x.percent,
        resets_at: undefined,
    });
    const sub = document.createElement("div");
    sub.className = "au-row-sub";
    setText(sub, "已用 {p0}{p1} / {p2}{p3}", { p0: String(cur), p1: String(x.used.toFixed(2)), p2: String(cur), p3: String(x.limit.toFixed(2)) });
    row.appendChild(sub);
    return row;
}
function render(u) {
    $("au-label").textContent = u.account_label || u.account_id;
    const plan = $("au-plan");
    plan.textContent = u.plan ? (PLAN[u.plan] || u.plan) : "";
    plan.classList.toggle("hidden", !u.plan);
    const body = $("au-body");
    body.replaceChildren();
    if (!u.windows.length) {
        const empty = document.createElement("div");
        empty.className = "au-msg";
        setText(empty, "该账号暂时没有可显示的限额窗口");
        body.appendChild(empty);
    }
    else {
        for (const w of u.windows)
            body.appendChild(winRow(w));
    }
    if (u.extra && u.extra.enabled)
        body.appendChild(extraRow(u.extra));
    setTextRender($("au-fetched"), () => i18nText("更新于 ") + fmtClock(u.fetched_at, false));
}
/* 打开时正在查的会话。查询期间用户可能切走，回来的数据就不该再往弹窗里塞。 */
let forSession = "";
/* 读取中的骨架：形状对上 winRow（标签 + 百分比、轨道、重置时间），条数沿用上一次显示的
 * 窗口数（首次按常见的 5 小时 + 周额度两条）。刷新时弹窗高度原样不动。 */
function skeletonRows(count) {
    const wrap = skelShell(i18nText("查询中…"), "au-body-skeleton");
    for (let i = 0; i < count; i++) {
        const row = document.createElement("div");
        row.className = "au-row";
        row.setAttribute("aria-hidden", "true");
        const head = document.createElement("div");
        head.className = "au-row-head";
        head.append(skelBar(28 + i * 9, "text au-row-label"), skelBar("2.6em", "text au-row-pct"));
        const track = document.createElement("div");
        track.className = "au-track";
        const sub = document.createElement("div");
        sub.className = "au-row-sub";
        sub.append(skelBar(36 + i * 7, "text"));
        row.append(head, track, sub);
        wrap.append(row);
    }
    return wrap;
}
async function load() {
    const sid = forSession;
    const body = $("au-body");
    body.replaceChildren(skeletonRows(keepCount(body, ".au-row", 2)));
    $("au-fetched").replaceChildren(skelBar("8em", "inline text"));
    try {
        const u = await api(`/sessions/${sid}/account/usage`);
        if (forSession !== sid)
            return;
        render(u);
    }
    catch (e) {
        if (forSession !== sid)
            return;
        body.replaceChildren();
        $("au-fetched").textContent = "";
        const err = document.createElement("div");
        err.className = "au-msg err";
        err.textContent = e.message;
        body.appendChild(err);
    }
}
export async function openAcctUsage() {
    const sess = S.current;
    if (!sess)
        return;
    forSession = sess.id;
    $("au-label").textContent = sess.account_label || sess.account_id;
    $("au-plan").classList.add("hidden");
    dlg().showModal();
    await load();
}
$("au-close").addEventListener("click", () => dlg().close());
dlg().addEventListener("close", () => { forSession = ""; });
$("au-refresh").addEventListener("click", async () => {
    const btn = $("au-refresh");
    btnBusy(btn, () => i18nText("查询中…"));
    try {
        await load();
    }
    finally {
        btnDone(btn);
    }
});
