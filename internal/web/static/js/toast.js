/* 全站操作反馈：单例节点、显示层级和计时器统一由组件管理。
 * 样式见 css/toast.css；业务只需 toast(message, isError?)。 */
import { svgIcon } from "./icons.js";
let box;
let timer;
function toastBox() {
    if (!box) {
        box = document.createElement("div");
        box.id = "toast";
        box.className = "toast";
        box.setAttribute("role", "status");
        box.setAttribute("aria-live", "polite");
        box.setAttribute("aria-atomic", "true");
        // 与 dialog 同处 top layer，避免保存失败等提示被弹窗遮挡；不抢焦点。
        if (typeof box.showPopover === "function")
            box.popover = "manual";
        document.body.append(box);
    }
    return box;
}
/** 新提示替换旧提示并重新计时；错误保留更久，文字始终按纯文本渲染。 */
export function toast(message, isError = false) {
    const el = toastBox();
    clearTimeout(timer);
    // 重新置顶：当前提示显示期间，用户可能又打开了一个原生 dialog。
    if (el.popover)
        el.hidePopover();
    el.classList.remove("show");
    el.classList.toggle("err", isError);
    el.style.setProperty("--toast-duration", isError ? "4.2s" : "2.6s");
    const icon = document.createElement("span");
    icon.className = "toast-icon";
    icon.setAttribute("aria-hidden", "true");
    icon.append(svgIcon(isError ? "info" : "check", 18));
    const content = document.createElement("span");
    content.className = "toast-message";
    content.textContent = message;
    el.replaceChildren(icon, content);
    // 强制重新计算，让连续提示也重新播放入场和底部进度动画。
    void el.offsetWidth;
    el.classList.add("show");
    if (el.popover)
        el.showPopover();
    timer = setTimeout(() => {
        el.classList.remove("show");
        if (el.popover)
            el.hidePopover();
        el.textContent = "";
        timer = undefined;
    }, isError ? 4200 : 2600);
}
