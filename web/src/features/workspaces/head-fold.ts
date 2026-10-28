/* head-fold：对话进行中收起工作台顶部栏（标题行 + 标签栏），把高度让给对话流。
 * - 只在电脑宽度生效（窄屏标题已并入顶栏，收起样式写在 work.css 的宽屏媒体查询里）；
 * - 空间运行中、对话页且不是空对话的问候页时收起；已停止或休眠（顶部有「启动」）、正在启动/停止、
 *   切到终端、文件等页，或首次引导打开时照常显示；
 * - 收起后线程栏最左侧出现空间名：鼠标悬停或点击，顶部栏以浮层盖在对话上方滑出，
 *   移开或点别处收回；浮层里的按钮可「固定显示」，标题行的同一按钮再「收起」。
 * 固定与否是个人偏好，只存在当前浏览器的 localStorage，与对话刻度偏好同一口径。 */
"use strict";

import { S, bus } from "../../state.js";
import { $ } from "../../util.js";
import { t } from "../../i18n.js";
import { actionButton } from "../../icons.js";
import { agentAvatar, agentName } from "../../brand.js";
import { setTip, hideTip } from "../../tip.js";

const KEY = "agentbox_head_fold";
/** 鼠标划过空间名到浮层出来的停顿：只是路过线程栏时不弹 */
const HOVER_OPEN_MS = 120;
/** 移出浮层后多久收回：斜着移向菜单、气泡时不至于一闪就没 */
const HOVER_CLOSE_MS = 280;

function pinned() {
  try { return localStorage.getItem(KEY) === "off"; } catch { return false; }
}

export function initHeadFold() {
  const lifetime = new AbortController(), options = { signal: lifetime.signal };
  const workbench = $("workbench"), top = $("wb-top"), inner = top.firstElementChild as HTMLElement;
  const peek = $<HTMLButtonElement>("wb-peek"), toggle = $<HTMLButtonElement>("btn-wb-fold");
  const more = $("btn-wb-more"), guide = $("workspace-guide"), busy = $("wb-busy");
  let folded = false, open = false, keep = pinned(), timer = 0, toggleKey = "", peekKey = "";

  // 窄屏标题已并入顶栏，标签栏要常在：断点与 work.css 的窄屏布局一致
  const wide = matchMedia("(min-width: 761px)");
  const eligible = () => wide.matches && S.current?.status === "running" && S.tab === "chat"
    && $("chat-hero").classList.contains("hidden") && !busy.classList.contains("show")
    && !guide.matches(":popover-open");

  const clearTimer = () => { clearTimeout(timer); timer = 0; };
  const setOpen = (value: boolean) => {
    clearTimer();
    if (open === value) return;
    open = value;
    workbench.classList.toggle("head-peek", open);
    peek.setAttribute("aria-expanded", String(open));
    inner.inert = folded && !open;
    renderToggle();
  };

  const renderToggle = () => {
    toggle.classList.toggle("hidden", !eligible());
    const key = folded ? "pin" : "fold";
    if (key === toggleKey) return;
    toggleKey = key;
    if (folded) actionButton(toggle, "", "wb.pin", () => t("固定显示顶部栏"));
    else actionButton(toggle, "", "wb.fold", () => t("收起顶部栏"));
  };

  // 轮询每次都会发 data-updated：名称、Agent、运行状态没变就不重建，免得悬停气泡被换掉
  const renderPeek = () => {
    const sess = S.current;
    if (!sess) return;
    const key = [sess.id, sess.name, sess.agent, sess.status].join("\n");
    if (key === peekKey) return;
    peekKey = key;
    const avatar = agentAvatar(sess.agent, { size: 20, icon: 12, led: true });
    if (sess.status === "running") avatar.querySelector(".led")!.classList.add("on");
    setTip(avatar, agentName(sess.agent));
    $("wb-peek-avatar").replaceChildren(avatar);
    $("wb-peek-name").textContent = sess.name;
  };

  const sync = () => {
    const next = !keep && eligible();
    if (next !== folded) {
      folded = next;
      if (!folded) setOpen(false);
      workbench.classList.toggle("head-folded", folded);
      peek.hidden = !folded;
      inner.inert = folded && !open;
    }
    renderPeek();
    renderToggle();
  };

  const setKeep = (value: boolean) => {
    keep = value;
    try {
      if (value) localStorage.setItem(KEY, "off");
      else localStorage.removeItem(KEY);
    } catch {
      // 隐私模式下 localStorage 可能不可写：本次仍生效，只是记不住
    }
    hideTip();
    sync();
  };

  /* 浮层：悬停空间名打开，移出空间名与浮层后收回；⋯ 菜单开着时不收，菜单挂在 body 上，
   * 鼠标移进菜单就算离开了浮层，收回会让菜单悬在空处。 */
  const scheduleClose = () => {
    clearTimer();
    timer = window.setTimeout(() => {
      timer = 0;
      const focused = document.activeElement;
      if (more.getAttribute("aria-expanded") === "true" || focused && top.contains(focused) && focused.matches(":focus-visible")) {
        scheduleClose();
        return;
      }
      setOpen(false);
    }, HOVER_CLOSE_MS);
  };
  // 只认鼠标的进出：触屏点一下也会发 pointerenter/leave，交给下面的 click 切换
  peek.addEventListener("pointerenter", (e) => {
    if (e.pointerType !== "mouse" || !folded) return;
    clearTimer();
    if (!open) timer = window.setTimeout(() => setOpen(true), HOVER_OPEN_MS);
  }, options);
  for (const el of [peek, top]) {
    el.addEventListener("pointerleave", (e) => {
      if (e.pointerType === "mouse" && folded && (open || timer)) scheduleClose();
    }, options);
  }
  top.addEventListener("pointerenter", (e) => { if (e.pointerType === "mouse" && open) clearTimer(); }, options);

  // 点击（触屏、键盘）切换浮层；键盘打开时把焦点送进当前标签，方便直接方向键/Tab 走
  peek.addEventListener("click", (e) => {
    if (!folded) return;
    const opening = !open;
    setOpen(opening);
    if (opening && e.detail === 0) top.querySelector<HTMLElement>(".tab.active")?.focus();
  }, options);
  // 收起时（浮层里）这是「固定显示」，常态下是「收起」
  toggle.addEventListener("click", () => setKeep(folded), options);

  // 点在浮层和空间名之外、按 Esc、焦点离开浮层：收回
  document.addEventListener("pointerdown", (e) => {
    if (!open) return;
    const target = e.target as Node;
    if (top.contains(target) || peek.contains(target)) return;
    if (target instanceof Element && target.closest(".menu-pop, [popover]")) return;
    setOpen(false);
  }, options);
  document.addEventListener("keydown", (e) => {
    if (!open || e.key !== "Escape" || e.defaultPrevented) return;
    if (more.getAttribute("aria-expanded") === "true") return; // 先让菜单自己收
    setOpen(false);
    peek.focus();
  }, options);
  top.addEventListener("focusout", (e) => {
    const next = e.relatedTarget as Node | null;
    if (open && next && !top.contains(next) && next !== peek && !(next instanceof Element && next.closest(".menu-pop"))) setOpen(false);
  }, options);
  // 浮层里点了标签：切到别的页时顶部栏回到常态；点的还是对话就收回浮层
  top.addEventListener("click", (e) => {
    if (open && (e.target as Element).closest(".tab")) setOpen(false);
  }, options);

  bus.addEventListener("chat-view-updated", sync, options);
  bus.addEventListener("navigation-changed", sync, options);
  bus.addEventListener("data-updated", sync, options);
  guide.addEventListener("toggle", sync, options);
  wide.addEventListener("change", sync, options);
  // 启停遮罩亮起时顶部栏回到常态：遮罩只让出头部，进度和按钮都要看得见
  const busyWatch = new MutationObserver(sync);
  busyWatch.observe(busy, { attributes: true, attributeFilter: ["class"] });
  window.addEventListener("storage", (e) => { if (e.key === KEY) { keep = pinned(); sync(); } }, options);
  sync();

  return () => {
    if (lifetime.signal.aborted) return;
    lifetime.abort();
    busyWatch.disconnect();
    clearTimer();
    open = false; folded = false;
    workbench.classList.remove("head-folded", "head-peek");
    peek.hidden = true;
    peek.setAttribute("aria-expanded", "false");
    inner.inert = false;
    toggle.classList.add("hidden");
  };
}
