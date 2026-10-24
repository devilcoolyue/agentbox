import { setAttrRender, setText, t as i18nText } from "./i18n.js";
/* Shared mobile navigation and overflow actions. Move the existing controls so
 * listeners, disabled states and form values remain authoritative on both layouts. */
import { S, bus } from "./state.js";
import { actionButton, decorateIcons } from "./icons.js";

const mobile = matchMedia("(max-width: 760px)");
const byId = (id: string) => document.getElementById(id)!;

/* always=true：桌面端也收起（一行超过三个操作时，次要与破坏性动作统一进 ⋯）。 */
function overflow(host: HTMLElement, id: string, nodes: HTMLElement[], title: string | (() => string), always = false) {
  const anchors = nodes.map(node => {
    const anchor = document.createComment("responsive action");
    node.before(anchor);
    return anchor;
  });
  const trigger = actionButton(document.createElement("button"), "", "more", title);
  trigger.id = id;
  trigger.type = "button";
  trigger.className = "btn btn-sm overflow-trigger" + (always ? "" : " mobile-only");
  trigger.setAttribute("aria-expanded", "false");
  const panel = document.createElement("div");
  panel.id = id + "-panel";
  panel.className = "action-popover";
  panel.setAttribute("popover", "auto");
  if (typeof title === "function") setAttrRender(panel, "aria-label", title);
  else panel.setAttribute("aria-label", title);
  trigger.setAttribute("aria-controls", panel.id);
  trigger.popoverTargetElement = panel;
  host.append(trigger, panel);
  const close = () => { if (panel.matches(":popover-open")) panel.hidePopover(); };
  panel.addEventListener("beforetoggle", event => {
    const open = (event as ToggleEvent).newState === "open";
    trigger.setAttribute("aria-expanded", String(open));
    if (open) requestAnimationFrame(() => {
      const rect = trigger.getBoundingClientRect();
      panel.style.left = Math.max(8, Math.min(rect.right - panel.offsetWidth, innerWidth - panel.offsetWidth - 8)) + "px";
      panel.style.top = Math.max(8, Math.min(rect.bottom + 6, innerHeight - panel.offsetHeight - 8)) + "px";
    });
  });
  panel.addEventListener("click", event => {
    if ((event.target as Element).closest("button, a")) close();
  });
  panel.addEventListener("keydown", event => {
    if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); close(); trigger.focus(); }
  });
  const sync = () => {
    close();
    nodes.forEach((node, index) => always || mobile.matches ? panel.append(node) : anchors[index].after(node));
  };
  mobile.addEventListener("change", sync);
  window.addEventListener("resize", close);
  bus.addEventListener("navigation-changed", close);
  host.closest("dialog")?.addEventListener("close", close);
  sync();
}

overflow(document.querySelector<HTMLElement>(".fv-actions")!, "fv-more",
  ["fv-auto-wrap", "fv-newtab", "fv-download", "fv-meta"].map(byId), () => i18nText("文件更多操作"));
overflow(document.querySelector<HTMLElement>(".changes-actions")!, "changes-more",
  ["btn-changes-clone", "btn-changes-profile", "btn-changes-discard-all"].map(byId), () => i18nText("仓库更多操作"), true);

// The same navigation buttons drive both desktop and mobile. Their labels also
// carry live counts, avoiding a second list that can drift as accounts change.
// 窄屏用菜单而不是通用下拉：分区一共就十来个，竖屏一屏放得下，不需要搜索框（一获焦就弹
// 键盘、把列表挤成一小截），也不用在弹层里滑动；条目照搬桌面导航按钮的图标、文字与计数。
const wrapper = byId("mobile-section");
const sectionButton = byId("mobile-section-btn");
const sectionLabel = byId("mobile-section-label");
const sectionMenu = byId("mobile-section-menu");
const currentNav = () => S.view === "settings" ? byId("set-nav") : S.view === "git" ? byId("git-nav") : null;
const menuItems = () => [...sectionMenu.querySelectorAll<HTMLButtonElement>("[role=menuitemradio]:not(:disabled)")];
function syncNavigation() {
  const nav = currentNav();
  wrapper.classList.toggle("hidden", !nav);
  if (!nav) { if (sectionMenu.matches(":popover-open")) sectionMenu.hidePopover(); return; }
  const buttons = [...nav.querySelectorAll<HTMLButtonElement>("button:not(.hidden)")];
  const focused = sectionMenu.contains(document.activeElement) ? menuItems().indexOf(document.activeElement as HTMLButtonElement) : -1;
  sectionMenu.replaceChildren(...buttons.map((button, index) => {
    const item = document.createElement("button");
    item.type = "button";
    item.className = "section-menu-item";
    item.setAttribute("role", "menuitemradio");
    item.setAttribute("aria-checked", String(button.classList.contains("active")));
    item.disabled = button.disabled;
    item.dataset.index = String(index);
    for (const node of button.childNodes) {
      const copy = node.cloneNode(true);
      if (copy instanceof Element) { copy.removeAttribute("id"); copy.querySelectorAll("[id]").forEach(el => el.removeAttribute("id")); }
      item.append(copy);
    }
    return item;
  }));
  const active = buttons.find(button => button.classList.contains("active"));
  sectionLabel.textContent = (active?.querySelector(".action-label") || active)?.textContent?.trim() || "";
  if (focused >= 0) menuItems()[Math.min(focused, menuItems().length - 1)]?.focus();
}
function placeSectionMenu() {
  const rect = sectionButton.getBoundingClientRect();
  const viewport = window.visualViewport;
  const width = viewport?.width || innerWidth, height = viewport?.height || innerHeight;
  const top = rect.bottom + 6;
  sectionMenu.style.top = top + "px";
  sectionMenu.style.maxHeight = Math.max(160, height - top - 8) + "px";
  sectionMenu.style.left = Math.max(8, Math.min(rect.right - sectionMenu.offsetWidth, width - sectionMenu.offsetWidth - 8)) + "px";
}
sectionMenu.addEventListener("beforetoggle", event => {
  const open = (event as ToggleEvent).newState === "open";
  sectionButton.setAttribute("aria-expanded", String(open));
  if (!open) return;
  syncNavigation();
  requestAnimationFrame(() => {
    placeSectionMenu();
    (sectionMenu.querySelector<HTMLButtonElement>("[aria-checked=true]:not(:disabled)") || menuItems()[0])?.focus({ preventScroll: true });
  });
});
sectionMenu.addEventListener("click", event => {
  const item = (event.target as Element).closest<HTMLButtonElement>("[role=menuitemradio]");
  if (!item || item.disabled) return;
  sectionMenu.hidePopover();
  sectionButton.focus({ preventScroll: true });
  currentNav()?.querySelectorAll<HTMLButtonElement>("button:not(.hidden)")[Number(item.dataset.index)]?.click();
});
sectionMenu.addEventListener("keydown", event => {
  const items = menuItems();
  let pos = items.indexOf(document.activeElement as HTMLButtonElement);
  if (event.key === "ArrowDown") pos = (pos + 1) % items.length;
  else if (event.key === "ArrowUp") pos = (pos - 1 + items.length) % items.length;
  else if (event.key === "Home") pos = 0;
  else if (event.key === "End") pos = items.length - 1;
  else if (event.key === "Escape") { event.preventDefault(); sectionMenu.hidePopover(); sectionButton.focus(); return; }
  else return;
  event.preventDefault();
  items[pos]?.focus();
});
sectionButton.addEventListener("keydown", event => {
  if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
  event.preventDefault();
  sectionMenu.showPopover();
});
// iOS 收起/展开地址栏也会触发 resize，跟着重新定位而不是直接关掉
for (const target of [window, window.visualViewport]) target?.addEventListener("resize", () => { if (sectionMenu.matches(":popover-open")) placeSectionMenu(); });
bus.addEventListener("navigation-changed", () => queueMicrotask(syncNavigation));
for (const id of ["set-nav", "git-nav"]) {
  new MutationObserver(syncNavigation).observe(byId(id), { subtree: true, childList: true, characterData: true, attributes: true, attributeFilter: ["class"] });
}
syncNavigation();

// Keep introductory copy available on demand without repeating the section name.
// Actual warnings, field help and operation results are outside these intros.
function compactIntros(root: ParentNode) {
  for (const intro of root.querySelectorAll<HTMLElement>(".sec-intro > div:first-child")) {
    const heading = intro.querySelector("h2");
    const copy = intro.querySelector<HTMLParagraphElement>(":scope > p");
    if (!heading || !copy) continue;
    // 窄屏默认收起，把首屏留给列表；实现细节的「了解更多」也一起收进来
    const details = document.createElement("details");
    details.className = "section-help";
    details.open = !mobile.matches;
    const summary = document.createElement("summary");
    setText(summary, "说明");
    setAttrRender(summary, "aria-label", () => heading.textContent + i18nText("说明"));
    heading.after(details); // 紧跟标题
    details.append(summary, copy, ...intro.querySelectorAll<HTMLElement>(":scope > .learn-more"));
    intro.classList.add("compact-intro");
  }
}
compactIntros(document);
new MutationObserver(() => compactIntros(byId("git-content"))).observe(byId("git-content"), {childList: true, subtree: true});
decorateIcons();

mobile.addEventListener("change", () => {
  for (const details of document.querySelectorAll<HTMLDetailsElement>(".section-help")) details.open = !mobile.matches;
});



/* 键盘弹出时 iOS 与 Android Chrome 只把可视区域往上挪，不缩小布局视口：外壳连同顶栏被推出
 * 屏幕。触屏或窄屏上把外壳一直钉在可视视口上（.visual-viewport，见 shell.css），顶栏留在
 * 原处、输入框贴着键盘；不猜键盘是否打开（iOS 各版本 innerHeight 表现不一，判断会漏）。
 * 页面被放大时不接管，保留浏览器的平移缩放。可视区域明显变高（键盘收起）时，iOS 有时把
 * 布局视口留在挪开的位置，顺手滚回顶部。 */
const touchLayout = matchMedia("(max-width: 760px), (pointer: coarse)");
const KEYBOARD_MIN_PX = 120;
let lastHeight = 0;
function fitVisualViewport() {
  const viewport = window.visualViewport;
  const app = byId("app");
  const active = !!viewport && touchLayout.matches && Math.abs(viewport.scale - 1) < 0.01;
  app.classList.toggle("visual-viewport", active);
  if (!viewport) return;
  if (active) {
    app.style.setProperty("--visual-viewport-height", `${viewport.height}px`);
    app.style.setProperty("--visual-viewport-top", `${viewport.offsetTop}px`);
  }
  if (lastHeight && viewport.height - lastHeight > KEYBOARD_MIN_PX && (scrollX || scrollY)) scrollTo(0, 0);
  lastHeight = viewport.height;
}
window.visualViewport?.addEventListener("resize", fitVisualViewport);
window.visualViewport?.addEventListener("scroll", fitVisualViewport);
touchLayout.addEventListener("change", fitVisualViewport);
fitVisualViewport();

/* 区域内容不满一屏时浏览器不回弹（只有真能滚的容器才有 iOS 回弹 / Android 拉伸），用户往下
 * 拉毫无反应，像是卡住了。这种情况下由这里补一个同样手感的拉动：内容跟手位移、越拉越沉，
 * 松手弹回。内容超出一屏时交给浏览器原生效果（shell.css 的 overscroll-behavior: contain）；
 * 起点在输入框、或在一个自己能竖向滚动的子元素里时不接管。区域列表与 shell.css 保持一致。 */
const PULL_REGIONS = ".chat-log, .chat-hero, .tp-list, .empty, .set-content, .ext-body, .tun-body, .files-list, "
  + ".changes-list, .changes-diff, .skills-list, .skills-detail, #mcp-content";
/* 阻尼曲线照 iOS 原生回弹：位移 = h·(1 − 1/(d·0.55/h + 1))，h 是区域高度。拉 100px 走约 50px、
 * 拉 300px 走约 130px，越拉越沉但没有硬顶，和内容超出一屏时的原生回弹手感一致。
 * （2026-10 第一版用 110px 封顶、系数 0.6，真机上嫌拉着太重。） */
const PULL_RESIST = 0.55;
const PULL_MIN_SPAN = 300;
const reducedMotion = matchMedia("(prefers-reduced-motion: reduce)");
const scrollsY = (el: Element) => el.scrollHeight > el.clientHeight + 1 && /auto|scroll/.test(getComputedStyle(el).overflowY);
let pull: { region: HTMLElement; x: number; y: number; active: boolean } | null = null;

function pullTarget(target: Element): HTMLElement | null {
  if (!touchLayout.matches || reducedMotion.matches || target.closest("input, textarea, select, [contenteditable]")) return null;
  const region = target.closest<HTMLElement>(PULL_REGIONS);
  if (!region || region.scrollHeight > region.clientHeight + 1) return null;
  for (let el: Element | null = target; el && el !== region; el = el.parentElement) if (scrollsY(el)) return null;
  return region;
}

function releasePull() {
  const region = pull?.active ? pull.region : null;
  pull = null;
  if (!region) return;
  region.classList.add("pull-settle");
  region.style.setProperty("--pull-y", "0px");
  const done = () => { if (!pull || pull.region !== region) region.classList.remove("pull-bounce", "pull-settle"); };
  setTimeout(done, 400);
}

document.addEventListener("touchstart", event => {
  releasePull();
  const touch = event.touches[0];
  const region = event.touches.length === 1 ? pullTarget(event.target as Element) : null;
  if (region) pull = { region, x: touch.clientX, y: touch.clientY, active: false };
}, { passive: true });
document.addEventListener("touchmove", event => {
  if (!pull) return;
  if (event.touches.length !== 1) { releasePull(); return; }
  const dx = event.touches[0].clientX - pull.x, dy = event.touches[0].clientY - pull.y;
  if (!pull.active) {
    if (Math.abs(dx) > Math.abs(dy)) { pull = null; return; } // 横向滑动（代码块、标签条）不管
    if (Math.abs(dy) < 6) return;
    pull.active = true;
    pull.region.classList.remove("pull-settle");
    pull.region.classList.add("pull-bounce");
  }
  const span = Math.max(pull.region.clientHeight, PULL_MIN_SPAN);
  const offset = Math.sign(dy) * span * (1 - 1 / (Math.abs(dy) * PULL_RESIST / span + 1));
  pull.region.style.setProperty("--pull-y", `${offset.toFixed(1)}px`);
}, { passive: true });
document.addEventListener("touchend", event => { if (!event.touches.length) releasePull(); }, { passive: true });
document.addEventListener("touchcancel", releasePull, { passive: true });
