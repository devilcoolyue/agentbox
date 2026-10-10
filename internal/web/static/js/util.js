/* util：DOM / 格式化 / 加载态 / 气泡 / 灯箱等无业务依赖的小工具。 */
import { t as i18nText, setTextRender, setAttrRender } from "./i18n.js";
"use strict";
import { actionButton } from "./icons.js";
import { S } from "./state.js";
// 保留统一入口，已有业务调用无需感知 Toast 组件的拆分。
export { toast } from "./toast.js";
/* $ 按 id 取元素，返回类型断言成非空：这些 id 全部写死在 index.html 里，取不到
 * 就是模板被改坏了，属于开发期错误，不值得让每个调用点都写一遍空值判断。需要
 * input/dialog 等具体接口时用类型参数收窄，例如 $<HTMLInputElement>("login-user")。
 * 少数确实可能缺失的挂载点（如按视图渲染的容器），调用点仍保留了原有的判空。 */
export const $ = (id) => document.getElementById(id);
/* 全站统一移动端断点，与 css/base.css 的约定一致 */
const mq = window.matchMedia("(max-width: 760px)");
export const isMobile = () => mq.matches;
export const onMobileChange = (fn) => mq.addEventListener("change", fn);
/* 输入法组字中的回车只是让候选上屏（拼音里打英文、选词），不能当成「提交」。
 * Safari 组字结束那一下 isComposing 已是 false，但 keyCode 仍报 229。 */
export const isImeEnter = (e) => e.isComposing || e.keyCode === 229;
/* 纯触屏设备（手机、无键盘平板）的软键盘没有 Shift，回车只能用来换行，
 * 发送交给按钮；接了键盘/触控板的平板仍按桌面习惯回车发送。 */
const touchOnly = window.matchMedia("(hover: none) and (pointer: coarse)");
export const enterInsertsNewline = () => touchOnly.matches;
/* ---- 加载态 ---- */
export function spinEl() {
    const s = document.createElement("span");
    s.className = "spin";
    return s;
}
export function withSpin(text) {
    const f = document.createDocumentFragment();
    const caption = document.createTextNode("");
    if (typeof text === "function")
        setTextRender(caption, text);
    else
        caption.textContent = text;
    f.append(spinEl(), caption);
    return f;
}
/* 按钮进入/退出加载态：转圈 + 禁用，防止重复提交 */
const btnSaved = new WeakMap(); // btn -> 原始子节点，含图标等元素，不能退化成纯文本
export function btnBusy(btn, label) {
    if (btn.classList.contains("loading"))
        return;
    btnSaved.set(btn, [...btn.childNodes]);
    btn.disabled = true;
    btn.setAttribute("aria-busy", "true");
    btn.classList.add("loading");
    const caption = document.createElement("span");
    caption.className = "action-label";
    if (typeof label === "function")
        setTextRender(caption, label);
    else
        caption.textContent = label;
    btn.replaceChildren(spinEl(), caption);
}
export function btnDone(btn) {
    if (!btn.classList.contains("loading"))
        return;
    btn.classList.remove("loading");
    btn.removeAttribute("aria-busy");
    btn.replaceChildren(...(btnSaved.get(btn) || []));
    btnSaved.delete(btn);
    btn.disabled = false;
}
/* 工作区生命周期遮罩：启动/停止时罩住头部按钮以下的整块工作区，
 * 毛玻璃 + 绿/红点缀，给窄屏（按钮藏在 ⋯ 菜单里）一个明确的进行中反馈。 */
const WB_BUSY_LABEL = { get start() { return i18nText("正在启动工作空间"); }, get stop() { return i18nText("正在停止工作空间"); } };
export function wbBusy(kind) {
    const el = $("wb-busy");
    if (!el)
        return;
    setTextRender($("wb-busy-label"), () => WB_BUSY_LABEL[kind] || i18nText("处理中"));
    el.classList.remove("start", "stop");
    el.classList.add(kind, "show");
}
export function wbIdle() {
    $("wb-busy")?.classList.remove("show");
}
/* ---- 格式化 ---- */
/* 全站时间一律按系统时区（config.timezone，经 /me 下发）显示，不跟随访问者电脑。
 * 不要再写不带参数的 toLocaleString()：它同时套用浏览器的时区和语言，
 * 同一时刻会在不同页面显示成 22:44 与 2:53:28 PM。 */
const timeFormatters = new Map();
function zonedParts(ms) {
    const zone = S.timeZone || "Asia/Shanghai";
    let fmt = timeFormatters.get(zone);
    if (!fmt) {
        fmt = new Intl.DateTimeFormat("en-CA", {
            timeZone: zone,
            year: "numeric", month: "2-digit", day: "2-digit",
            hour: "2-digit", minute: "2-digit", second: "2-digit",
            hourCycle: "h23",
        });
        timeFormatters.set(zone, fmt);
    }
    const p = {};
    for (const part of fmt.formatToParts(new Date(ms))) {
        if (part.type !== "literal")
            p[part.type] = part.value;
    }
    return p;
}
/* 简短时间：09-29 22:44，用于对话、列表这类「最近」的时间 */
export function fmtTime(ms) {
    const p = zonedParts(ms);
    return `${p.month}-${p.day} ${p.hour}:${p.minute}`;
}
/* 系统时区下今天 0 点的墙上时间（YYYY-MM-DDT00:00），用作用量接口的 since */
export function todayWall() {
    const p = zonedParts(Date.now());
    return `${p.year}-${p.month}-${p.day}T00:00`;
}
/* 相对时间：刚刚 / 5 分钟前 / 3 小时前，超过一天回落到 fmtTime */
export function fmtAgo(ms) {
    const t = new Date(ms).getTime();
    if (isNaN(t))
        return "";
    const s = Math.max(0, (Date.now() - t) / 1000);
    if (s < 60)
        return i18nText("刚刚");
    if (s < 3600)
        return Math.floor(s / 60) + i18nText(" 分钟前");
    if (s < 86400)
        return Math.floor(s / 3600) + i18nText(" 小时前");
    return fmtTime(t);
}
/* 时刻：22:44:05，用于「已保存 / 更新于」这类当天的状态提示 */
export function fmtClock(ms, seconds = true) {
    const p = zonedParts(ms);
    return `${p.hour}:${p.minute}` + (seconds ? `:${p.second}` : "");
}
/* 完整时间：2026-09-29 22:44:05，用于文件修改时间、操作记录、版本信息 */
export function fmtDateTime(ms, seconds = true) {
    const t = new Date(ms);
    if (isNaN(t.getTime()))
        return "—";
    const p = zonedParts(t);
    return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}` + (seconds ? `:${p.second}` : "");
}
/* n 允许 undefined：目录项没有 size，原来靠 typeof 守卫返回空串 */
export function fmtSize(n) {
    if (typeof n !== "number")
        return "";
    if (n < 1024)
        return n + " B";
    if (n < 1 << 20)
        return (n / 1024).toFixed(1) + " KB";
    return (n / (1 << 20)).toFixed(1) + " MB";
}
/* 字节量：fmtSize 到 MB 就封顶，监控里主机内存动辄几十 GB，这里补到 TB。 */
export function fmtBytes(n) {
    if (typeof n !== "number" || !isFinite(n) || n < 0)
        return "—";
    if (n < 1024)
        return n + " B";
    const u = ["KB", "MB", "GB", "TB"];
    let v = n / 1024, i = 0;
    while (v >= 1024 && i < u.length - 1) {
        v /= 1024;
        i++;
    }
    return (v < 10 ? v.toFixed(1) : Math.round(v)) + " " + u[i];
}
export function fmtUptime(ms) {
    const s = Math.max(0, Math.floor(ms / 1000));
    const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
    if (d > 0)
        return i18nText("{p0} 天 {p1} 小时", { p0: String(d), p1: String(h) });
    if (h > 0)
        return i18nText("{p0} 小时 {p1} 分钟", { p0: String(h), p1: String(m) });
    return i18nText("{p0} 分钟", { p0: String(m) });
}
/* 延迟展示：<1ms 收成「<1」，个位数保留一位小数，其余取整 */
export function fmtLatency(ms) {
    if (ms < 1)
        return "<1 ms";
    if (ms < 10)
        return ms.toFixed(1) + " ms";
    return Math.round(ms) + " ms";
}
export function insertAtCursor(t, text) {
    const start = t.selectionStart ?? t.value.length;
    t.setRangeText(text, start, t.selectionEnd ?? start, "end");
    t.focus();
}
/* 触发浏览器下载：临时 <a download> 点一下即可，不必真的把文件读进内存。
 * 直链带 ?token=，所以只能用在 GET 接口上（见 AGENTS.md 的令牌约定）。 */
export function startDownload(url) {
    const a = document.createElement("a");
    a.href = url;
    a.download = "";
    a.click();
}
/* ---- 通用确认 / 输入对话框 ----
 * 替代原生 alert/confirm/prompt：原生弹窗阻塞 JS、样式与深色主题割裂，
 * 移动端（尤其 iOS 的 prompt）体验尤差。这里保持 Promise 化的调用形状，
 * 调用点仍然是一行 await。 */
/* 弹窗的 close 事件是在之后的任务里派发的（Chromium 排到下一帧）。上一次关闭的事件还没到就重新打开
 * 同一个弹窗，新的监听会把上一次的回答当成这一次的，真正的回答反而没人接——所以先等它到。 */
const closePending = new WeakMap();
function dlgOnce(dialog, resolveWith, onShow) {
    const show = () => new Promise((resolve) => {
        let delivered;
        const pending = new Promise(r => { delivered = r; });
        closePending.set(dialog, pending);
        dialog.addEventListener("close", () => {
            if (closePending.get(dialog) === pending)
                closePending.delete(dialog);
            delivered();
            resolve(resolveWith(dialog.returnValue));
        }, { once: true });
        dialog.showModal();
        onShow?.();
    });
    const previous = dialog.open ? undefined : closePending.get(dialog);
    return previous ? previous.then(show) : show();
}
/* 确认：true=确定，false=取消/关闭。 */
export function askConfirm(text, opts = {}) {
    setTextRender($("ask-title"), () => opts.title || i18nText("确认"));
    if (typeof text === "function")
        setTextRender($("ask-text"), text);
    else
        $("ask-text").textContent = text;
    const hint = $("ask-hint");
    setTextRender(hint, () => opts.hint || "");
    hint.classList.toggle("hidden", !opts.hint);
    const ok = $("ask-ok");
    ok.className = "btn " + (opts.danger ? "btn-danger" : "btn-primary");
    actionButton(ok, () => opts.okLabel || i18nText("确定"), opts.icon || "check");
    return dlgOnce($("dlg-ask"), (v) => v === "ok");
}
$("ask-ok").addEventListener("click", () => $("dlg-ask").close("ok"));
$("ask-cancel").addEventListener("click", () => $("dlg-ask").close(""));
$("ask-close").addEventListener("click", () => $("dlg-ask").close(""));
/* 输入：返回字符串，取消返回 null（与 window.prompt 语义一致）。 */
export function askPrompt(opts = {}) {
    setTextRender($("ask-input-title"), () => opts.title || i18nText("输入"));
    setTextRender($("ask-input-label"), () => opts.label || "");
    const field = $("ask-input-field");
    field.value = opts.value || "";
    field.type = opts.password ? "password" : "text";
    setAttrRender(field, "placeholder", () => opts.placeholder || "");
    const hint = $("ask-input-hint");
    setTextRender(hint, () => opts.hint || "");
    hint.classList.toggle("hidden", !opts.hint);
    askInputValidate = opts.validate || null;
    setAskInputError("");
    return dlgOnce($("dlg-ask-input"), (v) => (v === "ok" ? field.value : null), () => {
        field.focus();
        field.select();
    });
}
let askInputValidate = null;
function setAskInputError(msg) {
    const el = $("ask-input-error");
    el.textContent = msg || "";
    el.classList.toggle("hidden", !msg);
}
function submitAskInput() {
    const value = $("ask-input-field").value;
    if (askInputValidate) {
        const err = askInputValidate(value);
        if (err) {
            setAskInputError(err);
            return;
        }
    }
    $("dlg-ask-input").close("ok");
}
$("ask-input-ok").addEventListener("click", submitAskInput);
$("ask-input-cancel").addEventListener("click", () => $("dlg-ask-input").close(""));
$("ask-input-close").addEventListener("click", () => $("dlg-ask-input").close(""));
$("ask-input-field").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !isImeEnter(e)) {
        e.preventDefault();
        submitAskInput();
    }
});
/* ---- 图片灯箱（对话缩略图 / 终端路径预览共用） ---- */
export function openLightbox(url, title) {
    $("lb-img").src = url;
    $("lb-name").textContent = title || "";
    $("dlg-img").showModal();
}
$("lb-close").addEventListener("click", () => $("dlg-img").close());
$("dlg-img").addEventListener("click", (e) => {
    if (e.target === $("dlg-img") || e.target === $("lb-img"))
        $("dlg-img").close();
});
