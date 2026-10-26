/* 对话刻度导航（features/chat/outline.ts）的个人偏好，和语言、风格一起在用户弹层里选
 * （shell.ts 的 pref-picker）。只存在当前浏览器的 localStorage，像主题一样不进服务端：
 * - auto：电脑上常显；手机上自己滚动对话时浮现，停下 1.5 秒隐去（默认）
 * - always：手机上也一直显示
 * - off：不显示刻度，手机线程栏的「对话目录」按钮也一并收起
 * 刻度的线长、鼓起半径等是设计参数，写死在 outline.ts，不开放调。 */
"use strict";
export const RAIL_MODES = ["auto", "always", "off"];
/** 偏好变了（本页选的或其他标签页改的）；outline.ts 跟着重排，用户弹层跟着换选中态 */
export const RAIL_EVENT = "agentbox-outline-rail-change";
const KEY = "agentbox_outline_rail";
function stored() {
    try {
        const value = localStorage.getItem(KEY);
        return RAIL_MODES.includes(value) ? value : "auto";
    }
    catch {
        return "auto";
    }
}
let current = stored();
export const railMode = () => current;
export function setRailMode(mode) {
    if (mode === current)
        return;
    current = mode;
    try {
        if (mode === "auto")
            localStorage.removeItem(KEY);
        else
            localStorage.setItem(KEY, mode);
    }
    catch {
        // 隐私模式下 localStorage 可能不可写：本次会话仍生效，只是记不住
    }
    window.dispatchEvent(new CustomEvent(RAIL_EVENT));
}
// 同一浏览器的其他标签页改了偏好，这里跟着换（storage 事件只发给别的页面）
window.addEventListener("storage", e => {
    if (e.key !== KEY && e.key !== null)
        return;
    const next = stored();
    if (next === current)
        return;
    current = next;
    window.dispatchEvent(new CustomEvent(RAIL_EVENT));
});
