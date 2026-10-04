import { t as i18nText, setTextRender } from "./i18n.js";
import { S, bus } from "./state.js";
import { $ } from "./util.js";
import { decorateIcons, buttonLabel } from "./icons.js";
import { enhanceSelects } from "./select.js";
const panels = new Set();
let clearing = false;
export function createGitSurface(title, markup) {
    const inline = S.view === "git";
    const host = inline ? $("git-content") : document.body;
    const previous = inline ? [...host.children].filter(el => !el.classList.contains("git-covered")) : [];
    const focused = document.activeElement;
    const scroll = host.scrollTop;
    const d = document.createElement(inline ? "section" : "dialog");
    d.className = inline ? "git-surface" : "dlg-git-connections";
    d.innerHTML = `<div class="dlg-head"><h2></h2><button type="button" class="btn btn-sm btn-ghost" data-close data-icon="${inline ? "arrow-left" : "close"}">${inline ? i18nText("返回") : i18nText("关闭")}</button></div>` + markup;
    if (typeof title === "function")
        setTextRender(d.querySelector("h2"), title);
    else
        d.querySelector("h2").textContent = title;
    buttonLabel(d.querySelector("[data-close]"), () => inline ? i18nText("返回") : i18nText("关闭"), inline ? "arrow-left" : "close");
    if (inline) {
        Object.defineProperty(d, "open", { get: () => d.isConnected });
        d.close = () => {
            if (!d.isConnected)
                return;
            d.remove();
            d.dispatchEvent(new Event("close"));
            if (!clearing) {
                previous.forEach(el => el.classList.remove("git-covered"));
                host.scrollTop = scroll;
                if (focused?.isConnected)
                    focused.focus({ preventScroll: true });
            }
        };
        previous.forEach(el => el.classList.add("git-covered"));
    }
    d.querySelector("[data-close]").addEventListener("click", () => d.close());
    d.addEventListener("close", () => { panels.delete(d); d.remove(); });
    host.append(d);
    panels.add(d);
    decorateIcons(d);
    enhanceSelects(d);
    if (inline) {
        host.scrollTop = 0;
        d.tabIndex = -1;
        d.focus({ preventScroll: true });
    }
    else
        d.showModal();
    return d;
}
bus.addEventListener("git-page-cleared", () => {
    clearing = true;
    for (const d of [...panels].reverse())
        if (d.classList.contains("git-surface"))
            d.close();
    clearing = false;
});
bus.addEventListener("signed-out", () => { for (const d of [...panels].reverse())
    d.close(); });
