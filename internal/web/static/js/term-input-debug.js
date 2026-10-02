/* 终端输入诊断（默认关闭）：地址带 ?imedebug 时，记录 xterm 输入框收到的键盘、组字与输入
 * 事件，以及最终发往 PTY 的数据，点「上传」存进共享目录的附件区（/shared/.file/），用来排查
 * 移动端输入法在真机上的事件顺序。只在开启期间记录（会包含这期间输入的字符），不自动上传。 */
"use strict";
import { S } from "./state.js";
import { $, toast } from "./util.js";
import { uploadAttachment } from "./api.js";
const enabled = /[?&]imedebug(?:[=&]|$)/.test(location.search);
const MAX = 500;
const log = [];
const t0 = performance.now();
let count = null;
function push(entry) {
    log.push({ t: Math.round(performance.now() - t0), ...entry });
    if (log.length > MAX)
        log.shift();
    if (count)
        count.textContent = String(log.length);
}
/** 记录实际发往 PTY 的数据：xterm 自己发的与补发的分开标记 */
export function traceTermInput(source, data) {
    if (enabled)
        push({ ev: "send", source, data });
}
export function attachTermInputDebug(host, ta) {
    if (!enabled)
        return;
    const tail = () => ({ len: ta.value.length, tail: ta.value.slice(-4) });
    const keys = (e) => ({
        key: e.key, code: e.code, keyCode: e.keyCode, which: e.which, charCode: e.charCode,
        composing: e.isComposing, repeat: e.repeat, prevented: e.defaultPrevented,
    });
    const describe = (e) => {
        if (e instanceof KeyboardEvent)
            return keys(e);
        if (e instanceof CompositionEvent)
            return { data: e.data };
        if (e instanceof InputEvent)
            return { inputType: e.inputType, data: e.data, composing: e.isComposing, prevented: e.defaultPrevented };
        return {};
    };
    // 捕获阶段在 xterm 之前看到原始事件；冒泡阶段收不到说明 xterm 拦下（stopPropagation）了。
    for (const type of ["keydown", "keypress", "keyup", "beforeinput", "input", "compositionstart", "compositionupdate", "compositionend"]) {
        host.addEventListener(type, (e) => { if (e.target === ta)
            push({ ev: type, ...describe(e), ...tail() }); }, true);
        host.addEventListener(type, (e) => { if (e.target === ta)
            push({ ev: type + ":after", ...describe(e), ...tail() }); });
    }
    const box = document.createElement("div");
    box.className = "term-input-debug";
    const label = Object.assign(document.createElement("span"), { textContent: "输入诊断 · " });
    count = Object.assign(document.createElement("b"), { textContent: "0" });
    label.append(count, " 条");
    const button = (text, fn) => {
        const b = Object.assign(document.createElement("button"), { type: "button", className: "btn btn-sm", textContent: text });
        b.addEventListener("click", fn);
        return b;
    };
    box.append(label, button("清空", () => { log.length = 0; push({ ev: "clear" }); }), button("上传", async () => {
        if (!S.current)
            return;
        const body = JSON.stringify({ ua: navigator.userAgent, at: new Date().toISOString(), events: log }, null, 1);
        try {
            const res = await uploadAttachment(new File([body], "term-input.txt", { type: "text/plain" }));
            toast("已上传：" + res.path);
        }
        catch (e) {
            toast("上传失败：" + e.message, true);
        }
    }));
    $("tab-term").append(box);
}
