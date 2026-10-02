/* modality：记下最近一次操作来自键盘还是指针（触屏 / 鼠标 / 笔），写在 <html data-input>。
 *
 * 不能全信浏览器的 :focus-visible：iOS Safari 点按钮不会让按钮获焦，紧接着 showModal()
 * 或菜单把焦点挪到第一项时，WebKit 找不到「上一次是指针点的」依据，就按键盘焦点处理——
 * 手指点开的弹窗，关闭按钮上套着焦点环、还弹出「关闭」气泡。桌面 Safari 点按钮同样不获焦。
 * 焦点提示（tip.ts）与按钮焦点环（base.css）因此只在键盘操作时出现；外接键盘一按键就切回来。 */
"use strict";

const root = document.documentElement;

/** 还没有任何操作时按键盘算，不改变浏览器原本的判断 */
export const keyboardInput = () => root.dataset.input !== "pointer";

document.addEventListener("pointerdown", () => { root.dataset.input = "pointer"; }, true);
document.addEventListener("keydown", (e) => {
  // 组字中的按键是在输入文字；快捷键组合与单按修饰键（切应用、复制）也不是在用键盘挪焦点
  if (e.isComposing || e.keyCode === 229 || e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.key === "Shift" || e.key === "Control" || e.key === "Alt" || e.key === "Meta") return;
  root.dataset.input = "keyboard";
}, true);
