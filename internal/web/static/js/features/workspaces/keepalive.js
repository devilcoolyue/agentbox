/* 缓存保活（server/keepalive.go）：空间「⋯ → 缓存保活…」弹窗，以及对话输入栏、
 * 终端顶栏上的状态小图标。设置按空间存在服务端——用户离开后是服务端在续，
 * 不能做成浏览器偏好；全站开关、默认值与上限在「系统设置 → 容器与资源」。 */
import { api } from "../../api.js";
import { S, bus, emit } from "../../state.js";
import { setText, setTextRender, t as i18nText } from "../../i18n.js";
import { $, btnBusy, btnDone, fmtClock, toast } from "../../util.js";
import { setTip } from "../../tip.js";
import { fmtTokens } from "../../usage-math.js";
import { fmtUSD } from "../../quota.js";
import { actionButton, svgIcon } from "../../icons.js";
import { agentKey } from "../../brand.js";
/** 设置或计划变了（detail 是空间 ID），各处的小图标据此重读 */
export const KEEPALIVE_CHANGED = "keepalive-changed";
export function keepaliveSupported(sess) {
    return !!sess && agentKey(sess.agent) === "claude";
}
export function loadKeepalive(id) {
    return api(`/sessions/${encodeURIComponent(id)}/keepalive`);
}
const POLL_MS = 30_000;
const REFRESH_MS = 5 * 60_000;
const dlg = () => $("dlg-keepalive");
let forSession = "";
let poll;
let loadGen = 0;
function reasonText(reason) {
    switch (reason) {
        case "disabled": return i18nText("管理员已关闭缓存保活。");
        case "agent": return i18nText("只有 Claude 空间支持缓存保活。");
        case "account": return i18nText("当前账号是 API Key 或中转账号，Claude Code 只写 5 分钟缓存，隔几十分钟续不上，保活不起作用。");
        default: return "";
    }
}
/** 一条计划的名字：对话用线程标题，终端用目录 */
export function planName(p) {
    if (p.source === "chat")
        return p.title ? i18nText("对话「{title}」", { title: p.title }) : i18nText("对话");
    return i18nText("终端 {cwd}", { cwd: p.cwd || "" });
}
/** 计划的进度与费用，弹窗和小图标的提示共用 */
export function planMeta(p) {
    return i18nText("{tokens} tokens · 下次 {time} · 已续 {runs}/{max} 次 · 每次约 {cost}", {
        tokens: fmtTokens(p.prompt_tokens), time: fmtClock(p.next_at, false),
        runs: String(p.runs), max: String(p.max_runs), cost: fmtUSD(p.keep_micro_usd, 3),
    });
}
function numberOrEmpty(input, value) {
    input.value = value > 0 ? String(value) : "";
}
function fill(v) {
    const enabled = $("ka-enabled"), terminal = $("ka-terminal");
    const hours = $("ka-hours"), minTokens = $("ka-min");
    enabled.checked = v.enabled;
    terminal.checked = v.terminal;
    numberOrEmpty(hours, v.max_hours);
    numberOrEmpty(minTokens, v.min_context_tokens);
    hours.max = String(v.max_hours_limit);
    hours.placeholder = String(v.defaults.max_hours);
    minTokens.placeholder = String(v.defaults.min_context_tokens);
    setText($("ka-hours-label"), "最长保活（小时，最多 {n}）", { n: String(v.max_hours_limit) });
    setTextRender($("ka-defaults"), () => i18nText("留空使用默认值：{tokens} token、{hours} 小时。每 {minutes} 分钟续一次，由管理员设置。", {
        tokens: v.defaults.min_context_tokens.toLocaleString("en-US"), hours: String(v.defaults.max_hours), minutes: String(v.interval_minutes),
    }));
    const reason = $("ka-unavailable");
    reason.classList.toggle("hidden", v.available);
    setTextRender(reason, () => reasonText(v.reason));
    enabled.disabled = !v.available && !v.enabled; // 已开着的仍能关掉
    syncControls();
}
/** 总开关关着时，终端与阈值都没有意义，置灰 */
function syncControls() {
    const on = $("ka-enabled").checked;
    for (const id of ["ka-terminal", "ka-hours", "ka-min"])
        $(id).disabled = !on;
    $("ka-options").classList.toggle("off", !on);
}
function renderPlans(v) {
    const host = $("ka-plans");
    if (!v.plans.length) {
        const empty = document.createElement("p");
        empty.className = "note ka-empty";
        setText(empty, v.enabled
            ? "暂无保活计划。上下文达到门槛的对话，每轮结束后自动排上。"
            : "暂无保活计划。");
        host.replaceChildren(empty);
        return;
    }
    host.replaceChildren(...v.plans.map((p) => {
        const row = document.createElement("div");
        row.className = "ka-plan";
        const icon = svgIcon(p.source === "chat" ? "message" : "tool", 15);
        icon.classList.add("ka-plan-ic");
        const text = document.createElement("div");
        text.className = "ka-plan-text";
        const name = document.createElement("span");
        name.className = "ka-plan-name";
        setTextRender(name, () => planName(p));
        const meta = document.createElement("span");
        meta.className = "ka-plan-meta";
        setTextRender(meta, () => planMeta(p));
        text.append(name, meta);
        const stop = actionButton(document.createElement("button"), () => i18nText("停止"), "stop", () => i18nText("停止这条保活，下一次请求后再按设置排上"));
        stop.type = "button";
        stop.className = "btn btn-sm";
        stop.addEventListener("click", () => void stopPlan(p.id, stop));
        row.append(icon, text, stop);
        return row;
    }));
}
async function refresh(fillForm) {
    const id = forSession, gen = ++loadGen;
    if (!id)
        return;
    try {
        const v = await loadKeepalive(id);
        if (id !== forSession || gen !== loadGen)
            return;
        if (fillForm)
            fill(v);
        renderPlans(v);
        $("ka-error").classList.add("hidden");
    }
    catch (e) {
        if (id !== forSession)
            return;
        showError(i18nText("读取失败：") + e.message);
    }
}
function showError(text) {
    const el = $("ka-error");
    el.textContent = text;
    el.classList.remove("hidden");
}
async function stopPlan(planID, btn) {
    const id = forSession;
    btnBusy(btn, () => i18nText("停止中…"));
    try {
        const v = await api(`/sessions/${encodeURIComponent(id)}/keepalive/stop`, {
            method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ id: planID }),
        });
        if (id === forSession)
            renderPlans(v);
        emit(KEEPALIVE_CHANGED, id);
    }
    catch (e) {
        showError(i18nText("停止失败：") + e.message);
    }
    finally {
        btnDone(btn);
    }
}
function intField(id) {
    const v = $(id).value.trim();
    return v ? Math.round(Number(v)) : 0;
}
async function save(ev) {
    ev.preventDefault();
    const id = forSession;
    if (!id)
        return;
    const form = $("ka-form");
    if (!form.reportValidity())
        return;
    const btn = $("ka-save");
    btnBusy(btn, () => i18nText("保存中…"));
    try {
        const v = await api(`/sessions/${encodeURIComponent(id)}/keepalive`, {
            method: "PUT", headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
                enabled: $("ka-enabled").checked,
                terminal: $("ka-terminal").checked,
                max_hours: intField("ka-hours"),
                min_context_tokens: intField("ka-min"),
            }),
        });
        if (id !== forSession)
            return;
        fill(v);
        renderPlans(v);
        $("ka-error").classList.add("hidden");
        toast(i18nText("缓存保活设置已保存"));
        emit(KEEPALIVE_CHANGED, id);
    }
    catch (e) {
        showError(i18nText("保存失败：") + e.message);
    }
    finally {
        btnDone(btn);
    }
}
export async function openKeepalive() {
    const sess = S.current;
    if (!keepaliveSupported(sess))
        return;
    forSession = sess.id;
    $("ka-plans").replaceChildren();
    $("ka-error").classList.add("hidden");
    $("ka-unavailable").classList.add("hidden");
    dlg().showModal();
    clearInterval(poll);
    poll = setInterval(() => { if (!document.hidden)
        void refresh(false); }, POLL_MS);
    await refresh(true);
}
function closeDialog() {
    forSession = "";
    loadGen++;
    clearInterval(poll);
    poll = undefined;
}
$("ka-form").addEventListener("submit", (e) => void save(e));
$("ka-enabled").addEventListener("change", syncControls);
for (const id of ["ka-close", "ka-cancel"])
    $(id).addEventListener("click", () => dlg().close());
dlg().addEventListener("close", closeDialog);
bus.addEventListener("signed-out", () => dlg().close());
/** 对话输入栏：开着时说明状态；没开但值得开（订阅账号、上下文过门槛）时灰着提示 */
export function chatChipState(v) {
    if (!v || v.reason === "agent")
        return { show: false };
    const interval = String(v.interval_minutes);
    const chat = v.chat;
    if (v.enabled && v.available) {
        const plan = v.plans.find((p) => p.source === "chat");
        if (plan)
            return { show: true, on: true, tip: () => [i18nText("缓存保活中"), planMeta(plan), i18nText("点击查看或调整")].join("\n") };
        const why = !chat ? i18nText("下一轮结束后，上下文达到门槛就开始保活。")
            : chat.ttl !== "1h" ? i18nText("这一轮写的不是 1 小时缓存，续不上。")
                : chat.prompt_tokens < v.effective.min_context_tokens
                    ? i18nText("当前上下文 {tokens}，未到 {min} 门槛。", { tokens: fmtTokens(chat.prompt_tokens), min: fmtTokens(v.effective.min_context_tokens) })
                    : i18nText("下一轮结束后开始保活。");
        return { show: true, on: true, tip: () => [i18nText("缓存保活已开启"), why, i18nText("点击查看或调整")].join("\n") };
    }
    if (!v.enabled && v.available && chat && chat.ttl === "1h" && chat.prompt_tokens >= v.effective.min_context_tokens) {
        return { show: true, on: false, tip: () => [
                i18nText("离开超过 1 小时再回来，要重写约 {tokens} token 的缓存（≈{cost}）。", { tokens: fmtTokens(chat.prompt_tokens), cost: fmtUSD(chat.rewrite_micro_usd, 2) }),
                i18nText("开启缓存保活后每 {minutes} 分钟续一次，每次约 {cost}。点击设置", { minutes: interval, cost: fmtUSD(chat.keep_micro_usd, 3) }),
            ].join("\n") };
    }
    return { show: false };
}
/** 终端顶栏：只在终端会话有保活计划时出现 */
export function termChipState(v) {
    const plans = v?.enabled && v.terminal ? v.plans.filter((p) => p.source === "terminal") : [];
    if (!plans.length)
        return { show: false };
    return { show: true, on: true, tip: () => [
            i18nText("终端会话缓存保活中"),
            ...plans.map((p) => `${p.cwd || ""} · ${planMeta(p)}`),
            i18nText("点击查看或调整"),
        ].join("\n") };
}
export function renderChip(el, state) {
    el.classList.toggle("hidden", !state.show);
    if (!state.show)
        return;
    el.classList.toggle("on", state.on);
    setTip(el, state.tip);
    el.setAttribute("aria-label", state.tip().split("\n")[0]);
}
/** 一个小图标的刷新器：按空间读、作废过期结果；设置变了或定时重读。 */
export function chipRefresher(el, state) {
    let gen = 0;
    let timer;
    let session = "";
    async function load() {
        const sess = S.current, my = ++gen;
        if (!keepaliveSupported(sess)) {
            session = "";
            renderChip(el(), { show: false });
            return;
        }
        session = sess.id;
        try {
            const v = await loadKeepalive(session);
            if (my === gen && S.current?.id === session)
                renderChip(el(), state(v));
        }
        catch { /* 读不到就保持原样，不闪 */ }
    }
    bus.addEventListener(KEEPALIVE_CHANGED, (e) => {
        if (e.detail === session)
            soon(0);
    });
    // 计划每几十分钟才动一次（下次时间、已续次数），低频重读就够
    setInterval(() => { if (session && S.current?.id === session && !document.hidden)
        void load(); }, REFRESH_MS);
    function soon(ms) {
        clearTimeout(timer);
        timer = setTimeout(() => void load(), ms);
    }
    function reset() { gen++; clearTimeout(timer); session = ""; renderChip(el(), { show: false }); }
    bus.addEventListener("signed-out", reset);
    return {
        /** 稍后重读（回合刚结束时服务端还在排计划） */
        soon,
        reset,
    };
}
