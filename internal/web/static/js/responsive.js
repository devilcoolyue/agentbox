/* Shared mobile navigation and overflow actions. Move the existing controls so
 * listeners, disabled states and form values remain authoritative on both layouts. */
import { S, bus } from "./state.js";
import { actionButton, decorateIcons } from "./icons.js";
import { enhanceSelects, setSelectValue } from "./select.js";
const mobile = matchMedia("(max-width: 760px)");
const byId = (id) => document.getElementById(id);
/* always=true：桌面端也收起（一行超过三个操作时，次要与破坏性动作统一进 ⋯）。 */
function overflow(host, id, nodes, title, always = false) {
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
    const close = () => { if (panel.matches(":popover-open"))
        panel.hidePopover(); };
    panel.addEventListener("beforetoggle", event => {
        const open = event.newState === "open";
        trigger.setAttribute("aria-expanded", String(open));
        if (open)
            requestAnimationFrame(() => {
                const rect = trigger.getBoundingClientRect();
                panel.style.left = Math.max(8, Math.min(rect.right - panel.offsetWidth, innerWidth - panel.offsetWidth - 8)) + "px";
                panel.style.top = Math.max(8, Math.min(rect.bottom + 6, innerHeight - panel.offsetHeight - 8)) + "px";
            });
    });
    panel.addEventListener("click", event => {
        if (event.target.closest("button, a"))
            close();
    });
    panel.addEventListener("keydown", event => {
        if (event.key === "Escape") {
            event.preventDefault();
            event.stopPropagation();
            close();
            trigger.focus();
        }
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
overflow(document.querySelector(".fv-actions"), "fv-more", ["fv-auto-wrap", "fv-newtab", "fv-download", "fv-meta"].map(byId), "文件更多操作");
overflow(document.querySelector(".changes-actions"), "changes-more", ["btn-changes-clone", "btn-changes-profile", "btn-changes-discard-all"].map(byId), "仓库更多操作", true);
// The same navigation buttons drive both desktop and mobile. Their labels also
// carry live counts, avoiding a second list that can drift as accounts change.
const picker = byId("mobile-section-select");
const wrapper = byId("mobile-section");
function syncNavigation() {
    const nav = S.view === "settings" ? byId("set-nav") : S.view === "git" ? byId("git-nav") : null;
    wrapper.classList.toggle("hidden", !nav);
    if (!nav)
        return;
    const buttons = [...nav.querySelectorAll("button")];
    picker.replaceChildren(...buttons.map((button, index) => {
        const option = new Option(button.textContent.replace(/\s+/g, " ").trim(), String(index));
        option.disabled = button.disabled;
        return option;
    }));
    setSelectValue(picker, String(buttons.findIndex(button => button.classList.contains("active"))));
}
picker.addEventListener("change", () => {
    const nav = byId(S.view === "settings" ? "set-nav" : "git-nav");
    nav.querySelectorAll("button")[Number(picker.value)]?.click();
});
enhanceSelects(wrapper);
bus.addEventListener("navigation-changed", () => queueMicrotask(syncNavigation));
for (const id of ["set-nav", "git-nav"]) {
    new MutationObserver(syncNavigation).observe(byId(id), { subtree: true, childList: true, characterData: true, attributes: true, attributeFilter: ["class"] });
}
syncNavigation();
// Keep introductory copy available on demand without repeating the section name.
// Actual warnings, field help and operation results are outside these intros.
function compactIntros(root) {
    for (const intro of root.querySelectorAll(".sec-intro > div:first-child")) {
        const heading = intro.querySelector("h2");
        const copy = intro.querySelector(":scope > p");
        if (!heading || !copy)
            continue;
        // 窄屏默认收起，把首屏留给列表；实现细节的「了解更多」也一起收进来
        const details = document.createElement("details");
        details.className = "section-help";
        details.open = !mobile.matches;
        const summary = document.createElement("summary");
        summary.textContent = "说明";
        summary.setAttribute("aria-label", heading.textContent + "说明");
        heading.after(details); // 紧跟标题
        details.append(summary, copy, ...intro.querySelectorAll(":scope > .learn-more"));
        intro.classList.add("compact-intro");
    }
}
compactIntros(document);
new MutationObserver(() => compactIntros(byId("git-content"))).observe(byId("git-content"), { childList: true, subtree: true });
decorateIcons();
mobile.addEventListener("change", () => {
    for (const details of document.querySelectorAll(".section-help"))
        details.open = !mobile.matches;
});
