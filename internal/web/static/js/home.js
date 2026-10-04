import { setText, setTextRender, t as i18nText } from "./i18n.js";
/* home：工作台首页。已有空间时列出最近使用的工作空间（运行状态、最近一条对话、
 * 今天的花费），点一下直接进入；一个空间都没有时才显示新手引导和吉祥物。 */
"use strict";
import { S, bus, emit } from "./state.js";
import { $, fmtAgo, todayWall } from "./util.js";
import { api } from "./api.js";
import { agentAvatar, agentName } from "./brand.js";
import { fmtUSD } from "./quota.js";
import { sessionState } from "./session-state.js";
import { setTip } from "./tip.js";
const RECENT = 6;
const EXTRA_TTL = 60_000;
const extras = new Map();
const inflight = new Set();
function homeVisible() {
    return S.view === "work" && !S.current && !$("empty").classList.contains("hidden");
}
function recentSessions() {
    return [...S.sessions]
        .sort((a, b) => Date.parse(b.updated_at || "") - Date.parse(a.updated_at || ""))
        .slice(0, RECENT);
}
async function loadExtra(sess) {
    if (inflight.has(sess.id))
        return;
    const cached = extras.get(sess.id);
    if (cached && Date.now() - cached.at < EXTRA_TTL)
        return;
    inflight.add(sess.id);
    const token = S.token;
    const since = encodeURIComponent(todayWall());
    const [threads, usage] = await Promise.allSettled([
        api(`/sessions/${sess.id}/chat/threads`),
        api(`/usage/events?limit=1&session=${encodeURIComponent(sess.id)}&since=${since}`),
    ]);
    inflight.delete(sess.id);
    if (token !== S.token)
        return;
    const extra = { at: Date.now() };
    if (threads.status === "fulfilled") {
        const list = threads.value?.threads || [];
        const active = list.find((t) => t.id === threads.value?.active && t.turns > 0)
            || list.filter((t) => t.turns > 0).sort((a, b) => Date.parse(b.updated) - Date.parse(a.updated))[0];
        if (active) {
            extra.title = active.title;
            extra.updated = active.updated;
        }
    }
    else
        extra.failed = true;
    const cost = usage.status === "fulfilled" ? usage.value?.total?.cost_micro_usd : undefined;
    if (typeof cost === "number")
        extra.cost = cost;
    extras.set(sess.id, extra);
    if (homeVisible())
        renderHome();
}
function card(sess) {
    const el = document.createElement("button");
    el.type = "button";
    el.className = "home-card";
    el.addEventListener("click", () => emit("open-session", sess));
    const state = sessionState(sess);
    const av = agentAvatar(sess.agent, { size: 34, icon: 18, led: true });
    if (state.cls === "run")
        av.querySelector(".led").classList.add("on");
    const head = document.createElement("span");
    head.className = "home-card-head";
    const name = Object.assign(document.createElement("span"), { className: "home-card-name", textContent: sess.name });
    const pill = Object.assign(document.createElement("span"), { className: "state-pill " + state.cls, textContent: state.label });
    setTextRender(pill, () => sessionState(sess).label);
    setTip(pill, () => sessionState(sess).tip);
    head.append(name, pill);
    const meta = Object.assign(document.createElement("span"), {
        className: "home-card-meta", textContent: `${agentName(sess.agent)} · ${sess.account_label || sess.account_id}`,
    });
    const extra = extras.get(sess.id);
    const last = document.createElement("span");
    last.className = "home-card-last";
    if (!extra)
        setText(last, "读取最近对话…");
    else if (extra.title) {
        last.append(Object.assign(document.createElement("span"), { className: "home-card-title", textContent: extra.title }), Object.assign(document.createElement("span"), { className: "home-card-when", textContent: extra.updated ? fmtAgo(extra.updated) : "" }));
    }
    else
        setTextRender(last, () => extra.failed ? i18nText("最近对话读取失败") : i18nText("还没有对话"));
    const spend = document.createElement("span");
    spend.className = "home-card-spend";
    if (extra && typeof extra.cost === "number") {
        const today = document.createTextNode("");
        setTextRender(today, () => i18nText("今天 "));
        spend.append(today, Object.assign(document.createElement("span"), { className: "num", textContent: fmtUSD(extra.cost, 2) }));
    }
    const body = document.createElement("span");
    body.className = "home-card-body";
    body.append(head, meta, last, spend);
    el.append(av, body);
    return el;
}
export function renderHome() {
    const has = S.sessions.length > 0;
    $("home-intro").classList.toggle("hidden", has);
    $("home-recent").classList.toggle("hidden", !has);
    $("empty").classList.toggle("has-recent", has);
    if (!has)
        return;
    const list = recentSessions();
    const running = S.sessions.filter((s) => s.status === "running").length;
    setTextRender($("home-recent-sub"), () => i18nText("共 {p0} 个工作空间，{p1} 个运行中", { p0: String(S.sessions.length), p1: String(running) })
        + (S.sessions.length > RECENT ? i18nText("。这里列出最近的 {p0} 个，其余在左侧列表", { p0: String(RECENT) }) : ""));
    $("home-list").replaceChildren(...list.map(card));
    if (homeVisible())
        for (const sess of list)
            void loadExtra(sess);
}
$("home-new").addEventListener("click", () => $("btn-new").click());
bus.addEventListener("data-updated", () => { if (homeVisible())
    renderHome(); });
bus.addEventListener("navigation-changed", () => { if (homeVisible())
    queueMicrotask(renderHome); });
bus.addEventListener("view-changed", () => { if (homeVisible())
    queueMicrotask(renderHome); });
