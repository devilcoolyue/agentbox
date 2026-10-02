/* Shared mobile navigation and overflow actions. Move the existing controls so
 * listeners, disabled states and form values remain authoritative on both layouts. */
import { S, bus } from "./state.js";
import { actionButton, decorateIcons } from "./icons.js";

const mobile = matchMedia("(max-width: 760px)");
const byId = (id: string) => document.getElementById(id)!;

/* always=true：桌面端也收起（一行超过三个操作时，次要与破坏性动作统一进 ⋯）。 */
function overflow(host: HTMLElement, id: string, nodes: HTMLElement[], title: string, always = false) {
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
  panel.setAttribute("aria-label", title);
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
  ["fv-auto-wrap", "fv-newtab", "fv-download", "fv-meta"].map(byId), "文件更多操作");
overflow(document.querySelector<HTMLElement>(".changes-actions")!, "changes-more",
  ["btn-changes-clone", "btn-changes-profile", "btn-changes-discard-all"].map(byId), "仓库更多操作", true);

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
  const buttons = [...nav.querySelectorAll<HTMLButtonElement>("button")];
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
  currentNav()?.querySelectorAll<HTMLButtonElement>("button")[Number(item.dataset.index)]?.click();
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
    summary.textContent = "说明";
    summary.setAttribute("aria-label", heading.textContent + "说明");
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


