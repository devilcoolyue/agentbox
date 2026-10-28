/* head-fold：空间运行中收起工作台顶部栏（标题行 + 标签栏），把高度让给对话、终端、文件等内容。
 * - 只在电脑宽度生效（窄屏标题已并入顶栏，收起样式写在 work.css 的宽屏媒体查询里）；
 * - 各标签页都收；对话页的空对话问候页、已停止或休眠（顶部有「启动」）、正在启动/停止，
 *   或首次引导打开时照常显示；
 * - 收起后当前标签页第一行工具栏的最左侧出现空间名和当前标签页图标（对话页是线程栏）：
 *   鼠标悬停或点击，顶部栏从空间名处像水滴一样滴出、铺开成浮在内容上的卡片，移开或点别处
 *   收回成一小滴被空间名吸回去；卡片里的图钉「固定」后铺满成常态顶部栏，再点「取消固定」收起。
 * 形状用可打断的弹簧逐帧计算（与 SwiftUI spring 同一套 response / damping 参数），进行中掉头
 * 会带着当前速度转向；「减少动态效果」时直接跳到终点。固定与否是个人偏好，只存在当前浏览器的
 * localStorage，与对话刻度偏好同一口径。 */
"use strict";

import { S, bus } from "../../state.js";
import { $ } from "../../util.js";
import { t } from "../../i18n.js";
import { actionButton, svgIcon, type IconSlot } from "../../icons.js";
import { agentAvatar, agentName } from "../../brand.js";
import { setTip, hideTip } from "../../tip.js";
import { motionOff } from "../../motion.js";

const KEY = "agentbox_head_fold";
/** 鼠标划过空间名到浮层出来的停顿：只是路过工具栏时不弹 */
const HOVER_OPEN_MS = 110;
/** 移出浮层后多久收回：斜着移向菜单、气泡时不至于一闪就没 */
const HOVER_CLOSE_MS = 280;
/** 浮层卡片离工作台边缘的距离与圆角 */
const CARD_X = 8, CARD_Y = 6, CARD_R = 16;

/** 各标签页放空间名的位置：第一行工具栏里靠左的那一组，排在最前。两端对齐的工具栏
 * （文件、MCP）放进左侧那组，不然空间名会把左组挤到中间。 */
const PEEK_HOSTS: Record<string, string> = {
  chat: "#tab-chat > .thread-bar",
  term: "#tab-term > .term-bar",
  browser: "#tab-browser > .browser-toolbar",
  files: "#tab-files > .files-bar > .files-left",
  changes: "#tab-changes > .changes-bar",
  skills: "#tab-skills > .skills-bar > .skills-left",
  mcp: "#tab-mcp > .mcp-toolbar > .mcp-scope-group",
};
/** 空间名后面的当前标签页徽标，与标签栏用同一图标位，管理员换图标两处一起变 */
const TAB_ICONS: Record<string, IconSlot> = {
  chat: "tab.chat", term: "tab.terminal", browser: "tab.browser", files: "tab.files",
  changes: "tab.changes", skills: "tab.skills", mcp: "tab.mcp",
};

/** 阻尼弹簧：response 是无阻尼时一次振荡的秒数，damping < 1 会回弹 */
class Spring {
  x = 0; v = 0; target = 0; private k = 0; private c = 0;
  tune(response: number, damping: number) {
    this.k = (2 * Math.PI / response) ** 2;
    this.c = 4 * Math.PI * damping / response;
  }
  step(dt: number) {
    const n = Math.max(1, Math.ceil(dt * 240)), h = dt / n; // 小步长半隐式欧拉，掉帧也不会发散
    for (let i = 0; i < n; i++) { this.v += (-this.k * (this.x - this.target) - this.c * this.v) * h; this.x += this.v * h; }
  }
  get rest() { return Math.abs(this.x - this.target) < 4e-4 && Math.abs(this.v) < 4e-3; }
  set(value: number) { this.x = this.target = value; this.v = 0; }
}

interface Rect { x: number; y: number; w: number; h: number; r: number }
const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));
const lerp = (a: number, b: number, k: number) => a + (b - a) * k;

function pinned() {
  try { return localStorage.getItem(KEY) === "off"; } catch { return false; }
}

export function initHeadFold() {
  const lifetime = new AbortController(), options = { signal: lifetime.signal };
  const workbench = $("workbench"), top = $("wb-top"), inner = top.firstElementChild as HTMLElement;
  const drop = $("wb-drop"), pill = drop.firstElementChild as HTMLElement, blob = drop.lastElementChild as HTMLElement;
  const peek = $<HTMLButtonElement>("wb-peek"), badge = $("wb-peek-tab"), toggle = $<HTMLButtonElement>("btn-wb-fold");
  const more = $("btn-wb-more"), guide = $("workspace-guide"), busy = $("wb-busy");
  let folded = false, open = false, keep = pinned(), timer = 0, toggleKey = "", peekKey = "";
  let docking = false, absorbed = true, hovering = false, push = 0, raf = 0, last = 0, synced = "";

  // 展开进度拆成横、纵、圆角、内容四根弹簧：纵向先坠、横向后铺，形状才像水滴；sp 是固定（铺满）进度
  const sx = new Spring(), sy = new Spring(), sr = new Spring(), sc = new Spring(), sp = new Spring();
  const springs = [sx, sy, sr, sc, sp];
  sp.tune(0.46, 0.88);
  const retune = () => {
    if (open) { sy.tune(0.34, 0.64); sx.tune(0.58, 0.74); sr.tune(0.5, 0.84); sc.tune(0.28, 1); }
    // 收起放慢：内容先淡出，形状先收高度、再收宽度，从容地缩回空间名
    else { sy.tune(0.5, 0.84); sx.tune(0.66, 0.82); sr.tune(0.5, 1); sc.tune(0.22, 1); }
  };

  // 窄屏标题已并入顶栏，标签栏要常在：断点与 work.css 的窄屏布局一致
  const wide = matchMedia("(min-width: 761px)");
  const eligible = () => wide.matches && S.current?.status === "running" && !!PEEK_HOSTS[S.tab]
    && (S.tab !== "chat" || $("chat-hero").classList.contains("hidden")) && !busy.classList.contains("show")
    && !guide.matches(":popover-open");

  /* ---------- 逐帧绘制 ---------- */

  const render = () => {
    if (!folded) return;
    const box = workbench.getBoundingClientRect();
    const card: Rect = { x: CARD_X, y: CARD_Y, w: box.width - 2 * CARD_X, h: 0, r: CARD_R };
    const full: Rect = { x: 0, y: 0, w: box.width, h: 0, r: 0 };
    const k = sp.x;
    const T: Rect = { x: lerp(card.x, full.x, k), y: lerp(card.y, full.y, k), w: lerp(card.w, full.w, k), h: 0, r: lerp(card.r, full.r, clamp(k, 0, 1)) };
    inner.style.width = T.w + "px";
    T.h = inner.offsetHeight;
    // 空间名的位置按不推开内容时算：取消固定时下方内容正往上收，水滴要落在空间名最终停的地方
    const c = peek.getBoundingClientRect();
    const C = { x: c.left - box.left, y: c.top - box.top - push, w: c.width, h: c.height };
    // 起点 / 终点是空间名中心的一小滴
    const d = C.h * 0.7, D: Rect = { x: C.x + C.w / 2 - d / 2, y: C.y + C.h / 2 - d / 2, w: d, h: d, r: d / 2 };
    const R: Rect = {
      x: lerp(D.x, T.x, sx.x), w: Math.max(d * 0.4, lerp(D.w, T.w, sx.x)),
      y: lerp(D.y, T.y, sy.x), h: Math.max(d * 0.4, lerp(D.h, T.h, sy.x)), r: 0,
    };
    R.r = clamp(lerp(D.r, T.r, clamp(sr.x, 0, 1.15)), 0, Math.min(R.w, R.h) / 2);
    const q = Math.min(clamp((Math.min(sx.x, sy.x) - 0.5) / 0.42, 0, 1), clamp(sc.x, 0, 1));
    const shown = open || sx.x > 0.004 || sy.x > 0.004 || k > 0.004;

    push = clamp(k, 0, 1.06) * T.h;
    top.style.height = push + "px";
    drop.style.visibility = inner.style.visibility = shown ? "visible" : "hidden";
    drop.classList.toggle("docked", k > 0.9);
    Object.assign(blob.style, {
      transform: `translate(${R.x}px, ${R.y}px)`, width: R.w + "px", height: R.h + "px", borderRadius: R.r + "px",
      // 收起最后一程：小滴变淡，滤镜的透明度阈值让它像被吸进去一样缩没
      opacity: open ? "1" : String(clamp(Math.max(sx.x, sy.x) / 0.1, 0, 1)),
    });
    Object.assign(pill.style, {
      transform: `translate(${C.x}px, ${C.y + push}px)`, width: C.w + "px", height: C.h + "px", borderRadius: C.h / 2 + "px",
    });
    pill.classList.toggle("show", (hovering || shown) && k < 0.05);
    const inset = [R.y - T.y, T.x + T.w - (R.x + R.w), T.y + T.h - (R.y + R.h), R.x - T.x].map(v => v.toFixed(2) + "px").join(" ");
    Object.assign(inner.style, {
      clipPath: `inset(${inset} round ${R.r.toFixed(2)}px)`,
      opacity: q.toFixed(3),
      filter: q > 0.995 ? "" : `blur(${((1 - q) * 6).toFixed(2)}px)`,
      transform: `translate(${T.x}px, ${(T.y + (1 - q) * -6).toFixed(2)}px) scale(${(0.97 + 0.03 * q).toFixed(4)})`,
    });
    // 空间名先让位，避免和标题叠成两层字；内容盖住它时不再接点击
    peek.style.opacity = (1 - clamp(q * 2.5, 0, 1)).toFixed(3);
    peek.style.pointerEvents = q > 0.6 ? "none" : "";
    if (badge.dataset.tab !== S.tab && q < 0.35) swapBadge(true);
    // 水滴吸回空间名的那一刻，空间名颤一下
    if (!open && !absorbed && sy.x < 0.1) {
      absorbed = true;
      if (!motionOff()) peek.animate([
        { scale: "1" }, { scale: "1.09 0.9", offset: 0.3 }, { scale: "0.96 1.05", offset: 0.55 }, { scale: "1.02 0.99", offset: 0.78 }, { scale: "1" },
      ], { duration: 460, easing: "cubic-bezier(.3,.7,.3,1)" });
    }
  };

  const frame = (now: number) => {
    raf = 0;
    const dt = Math.min(0.05, (now - last) / 1000);
    last = now;
    for (const s of springs) s.step(dt);
    if (springs.every(s => s.rest)) {
      for (const s of springs) s.set(s.target);
      if (docking) { finishDock(); return; }
      render();
      return;
    }
    render();
    raf = requestAnimationFrame(frame);
  };
  // 有动画就逐帧跑；减少动态效果时直接落到终点
  const animate = () => {
    if (motionOff()) {
      cancelAnimationFrame(raf); raf = 0;
      for (const s of springs) s.set(s.target);
      if (docking) finishDock(); else render();
      return;
    }
    if (!raf) { last = performance.now(); raf = requestAnimationFrame(frame); }
  };

  /* ---------- 状态切换 ---------- */

  const clearTimer = () => { clearTimeout(timer); timer = 0; };
  const setOpen = (value: boolean) => {
    clearTimer();
    if (open === value) return;
    open = value;
    if (open) absorbed = false;
    retune();
    for (const s of [sx, sy, sr, sc]) s.target = open ? 1 : 0;
    workbench.classList.toggle("head-peek", open);
    peek.setAttribute("aria-expanded", String(open));
    inner.inert = folded && !open;
    renderToggle();
    if (folded) animate();
  };

  // 进入收起：从常态（铺满、推开内容）开始，跟取消固定是同一段收回动画；首次进入、切换空间时直接落位
  const enterFold = (animated: boolean) => {
    const height = inner.offsetHeight;
    folded = true;
    workbench.classList.add("head-folded");
    peek.hidden = false;
    placePeek();
    open = false;
    workbench.classList.remove("head-peek");
    peek.setAttribute("aria-expanded", "false");
    inner.inert = true;
    retune();
    for (const s of springs) s.set(animated ? 1 : 0);
    for (const s of springs) s.target = 0;
    push = animated ? height : 0;
    absorbed = !animated;
    // 先同步画出起始帧（铺满、推开内容），再交给逐帧动画：否则加上 .head-folded 后到第一帧之间，
    // 浮层按样式默认隐藏、占位高度为空，内容会先往上跳一帧再被推回去
    render();
    if (animated) animate();
  };
  const leaveFold = () => {
    cancelAnimationFrame(raf); raf = 0;
    folded = false; open = false; docking = false; push = 0;
    for (const s of springs) s.set(0);
    workbench.classList.remove("head-folded", "head-peek");
    peek.hidden = true;
    peek.setAttribute("aria-expanded", "false");
    peek.style.opacity = peek.style.pointerEvents = "";
    inner.inert = false;
    for (const prop of ["width", "transform", "clipPath", "opacity", "filter", "visibility"] as const) inner.style[prop] = "";
    top.style.height = "";
    drop.style.visibility = "";
    drop.classList.remove("docked");
  };
  // 固定：卡片铺满成整条顶部栏、把下方内容推开，落定后换回常态布局（位置一致，看不出切换）
  const finishDock = () => {
    docking = false;
    leaveFold();
    sync();
    const icon = toggle.querySelector("svg");
    if (icon && !motionOff()) icon.animate([{ rotate: "-45deg", scale: "0.6", opacity: 0.3 }, { rotate: "0deg", scale: "1", opacity: 1 }],
      { duration: 460, easing: "cubic-bezier(.34,1.56,.5,1)" });
  };

  const renderToggle = () => {
    toggle.classList.toggle("hidden", !eligible());
    toggle.classList.toggle("pinned", !folded);
    const key = folded ? "pin" : "unpin";
    if (key === toggleKey) return;
    toggleKey = key;
    if (folded) actionButton(toggle, "", "wb.pin", () => t("固定顶部栏"));
    else actionButton(toggle, "", "wb.unpin", () => t("取消固定"));
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

  // 当前标签页徽标：旧图标往上淡出、新图标从下面弹上来，徽标鼓一下
  const tabLabel = () => document.querySelector(`.tab[data-tab="${badge.dataset.tab}"] .t`)?.textContent?.trim() || "";
  setTip(badge, () => t("当前：{p0}", { p0: tabLabel() }));
  function swapBadge(animated: boolean) {
    const slot = TAB_ICONS[S.tab];
    if (!slot) return;
    badge.dataset.tab = S.tab;
    const next = svgIcon(slot, 14);
    if (!animated || motionOff() || !badge.firstElementChild) { badge.replaceChildren(next); return; }
    for (const old of badge.querySelectorAll("svg")) {
      old.animate([{}, { translate: "0 -11px", scale: "0.5", opacity: 0 }], { duration: 220, easing: "ease-in", fill: "forwards" })
        .finished.then(() => old.remove(), () => old.remove());
    }
    badge.append(next);
    next.animate([{ translate: "0 11px", scale: "0.5", opacity: 0 }, {}], { duration: 420, easing: "cubic-bezier(.34,1.56,.5,1)" });
    badge.animate([{ scale: "1" }, { scale: "1.18", offset: 0.35 }, { scale: "0.95", offset: 0.7 }, { scale: "1" }], { duration: 420, easing: "cubic-bezier(.3,.7,.3,1)" });
  }

  // 空间名跟着当前标签页走：同一个节点挪进该页的工具栏，宿主加 .wb-peek-host 供个别布局（浏览器的网格）让位
  function placePeek() {
    const host = document.querySelector<HTMLElement>(PEEK_HOSTS[S.tab] || "");
    const previous = peek.parentElement;
    if (host && previous !== host) {
      previous?.classList.remove("wb-peek-host");
      host.prepend(peek);
    }
    peek.parentElement?.classList.toggle("wb-peek-host", folded);
  }

  function sync() {
    const want = !keep && eligible();
    const sessionKey = S.current?.id || "";
    // 首次进入、切换空间时直接落位；同一空间里从常态变成收起（发出第一条消息、从问候页切到终端、
    // 取消固定）才播收回动画
    const animated = !!synced && synced === sessionKey && !workbench.classList.contains("hidden");
    synced = sessionKey;
    if (want && !folded) enterFold(animated);
    else if (!want && folded && !docking) leaveFold();
    placePeek();
    renderPeek();
    renderToggle();
    // 浮层开着时空间名被盖住，徽标等它露出来再换（render 里），这样换的那一下看得见
    if (badge.dataset.tab !== S.tab && (!folded || !raf)) swapBadge(folded && !open);
  }

  const setKeep = (value: boolean) => {
    keep = value;
    try {
      if (value) localStorage.setItem(KEY, "off");
      else localStorage.removeItem(KEY);
    } catch {
      // 隐私模式下 localStorage 可能不可写：本次仍生效，只是记不住
    }
    hideTip();
    if (value && folded) {
      docking = true;
      if (!open) setOpen(true);
      sp.target = 1;
      animate();
      return;
    }
    sync();
  };

  /* ---------- 交互 ---------- */

  /* 浮层：悬停空间名打开，移出空间名与浮层后收回；⋯ 菜单开着时不收，菜单挂在 body 上，
   * 鼠标移进菜单就算离开了浮层，收回会让菜单悬在空处。 */
  const scheduleClose = () => {
    clearTimer();
    timer = window.setTimeout(() => {
      timer = 0;
      const focused = document.activeElement;
      if (docking || more.getAttribute("aria-expanded") === "true" || focused && top.contains(focused) && focused.matches(":focus-visible")) {
        scheduleClose();
        return;
      }
      setOpen(false);
    }, HOVER_CLOSE_MS);
  };
  // 只认鼠标的进出：触屏点一下也会发 pointerenter/leave，交给下面的 click 切换
  peek.addEventListener("pointerenter", (e) => {
    if (e.pointerType !== "mouse" || !folded) return;
    hovering = true;
    clearTimer();
    if (!open) { timer = window.setTimeout(() => setOpen(true), HOVER_OPEN_MS); render(); }
  }, options);
  for (const el of [peek, top]) {
    el.addEventListener("pointerleave", (e) => {
      if (e.pointerType !== "mouse" || !folded) return;
      if (el === peek) { hovering = false; if (!open) render(); }
      if (open || timer) scheduleClose();
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
  // 收起时（浮层里）是「固定」，常态下是「取消固定」
  toggle.addEventListener("click", () => setKeep(folded), options);

  // 点在浮层和空间名之外、按 Esc、焦点离开浮层：收回
  document.addEventListener("pointerdown", (e) => {
    if (!open || docking) return;
    const target = e.target as Node;
    if (top.contains(target) || peek.contains(target)) return;
    if (target instanceof Element && target.closest(".menu-pop, [popover]")) return;
    setOpen(false);
  }, options);
  document.addEventListener("keydown", (e) => {
    if (!open || docking || e.key !== "Escape" || e.defaultPrevented) return;
    if (more.getAttribute("aria-expanded") === "true") return; // 先让菜单自己收
    setOpen(false);
    peek.focus();
  }, options);
  top.addEventListener("focusout", (e) => {
    const next = e.relatedTarget as Node | null;
    if (open && !docking && next && !top.contains(next) && next !== peek && !(next instanceof Element && next.closest(".menu-pop"))) setOpen(false);
  }, options);
  // 浮层里点了标签：切过去后收回浮层，空间名随之挪进新标签页的工具栏
  top.addEventListener("click", (e) => {
    if (open && !docking && (e.target as Element).closest(".tab")) { hovering = false; setOpen(false); }
  }, options);

  bus.addEventListener("chat-view-updated", sync, options);
  bus.addEventListener("navigation-changed", sync, options);
  bus.addEventListener("data-updated", sync, options);
  guide.addEventListener("toggle", sync, options);
  wide.addEventListener("change", sync, options);
  window.addEventListener("resize", () => { if (folded && !raf) render(); }, options);
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
    leaveFold();
    peek.parentElement?.classList.remove("wb-peek-host");
    toggle.classList.add("hidden");
  };
}
