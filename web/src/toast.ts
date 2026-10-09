/* 全站操作反馈：单例节点、显示层级和计时器统一由组件管理。
 * 样式见 css/toast.css；业务只需 toast(message, isError?)。 */
import { svgIcon } from "./icons.js";
import { leave } from "./motion.js";

let box: HTMLDivElement | undefined;
let timer: ReturnType<typeof setTimeout> | undefined;
/** 正在淡出的那一下：新提示到来时取消它，节点直接改回显示状态 */
let leaving: Animation | null = null;

function toastBox(): HTMLDivElement {
  if (!box) {
    box = document.createElement("div");
    box.id = "toast";
    box.className = "toast";
    box.setAttribute("role", "status");
    box.setAttribute("aria-live", "polite");
    box.setAttribute("aria-atomic", "true");
    // 与 dialog 同处 top layer，避免保存失败等提示被弹窗遮挡；不抢焦点。
    if (typeof box.showPopover === "function") box.popover = "manual";
    document.body.append(box);
  }
  return box;
}

/** 新提示替换旧提示并重新计时；错误保留更久，文字始终按纯文本渲染。 */
export function toast(message: string, isError = false): void {
  const el = toastBox();
  clearTimeout(timer);
  leaving?.cancel();
  leaving = null;
  el.inert = false;
  // 重新置顶：当前提示显示期间，用户可能又打开了一个原生 dialog。
  if (el.popover) el.hidePopover();
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
  if (el.popover) el.showPopover();
  timer = setTimeout(() => {
    timer = undefined;
    // 朝入场的方向（上方）淡出；播完先撤掉保持终态的动画，再真正隐藏
    leaving = leave(el, () => {
      leaving?.cancel();
      leaving = null;
      el.classList.remove("show");
      if (el.popover) el.hidePopover();
      el.textContent = "";
      el.inert = false;
    }, -8);
  }, isError ? 4200 : 2600);
}
