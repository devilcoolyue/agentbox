import { setTextRender, setAttrRender, setAttr } from "./i18n.js";
import { ICON_SLOTS } from "./icon-slots.js";
import { BUILTIN_ICONS, ICON_SET_STYLES } from "./icon-builtin.js";
/* 图标统一从位置注册表取：调用方传位置 ID（icon-slots.ts），这里按「管理员覆盖 → 位置默认 → 跟随的语义位」
 * 解析成图标库引用，再用白名单元素画出来。管理员改了配置后 applyIconConfig 会重画页面上已有的图标。 */
const TAGS = {
    path: ["d"], circle: ["cx", "cy", "r"], ellipse: ["cx", "cy", "rx", "ry"],
    rect: ["x", "y", "width", "height", "rx", "ry"], line: ["x1", "y1", "x2", "y2"],
    polyline: ["points"], polygon: ["points"],
};
const PAINT = { fill: ["currentColor", "none"], stroke: ["currentColor", "none"], opacity: [".5"] };
const STROKE_WIDTH = "1.8";
const shapes = { ...BUILTIN_ICONS };
let overrides = {};
function slotDef(slot) {
    const def = ICON_SLOTS[slot];
    if (!def)
        throw new Error(`Unknown icon slot: ${slot}`);
    return def;
}
/** 未经管理员覆盖时的图标：自身默认，或沿 base 找到的语义位默认。 */
export function defaultIcon(slot) {
    for (let id = slot, hops = 0; hops < 8; hops++) {
        const def = slotDef(id);
        if (def.icon)
            return def.icon;
        id = def.base;
    }
    throw new Error(`Icon slot base chain too deep: ${slot}`);
}
/** 实际生效的图标：沿 base 链逐级看有没有管理员覆盖，最后落到默认图标。 */
export function resolveIcon(slot) {
    for (let id = slot, hops = 0; hops < 8; hops++) {
        const ref = overrides[id];
        if (ref && shapes[ref])
            return ref;
        const def = slotDef(id);
        if (def.icon)
            return def.icon;
        id = def.base;
    }
    throw new Error(`Icon slot base chain too deep: ${slot}`);
}
export function iconOverrides() { return overrides; }
export function hasIconShape(ref) { return !!shapes[ref]; }
/** 图标选择器加载整套图标库后登记形状，预览和保存后立即可用。 */
export function registerIconShapes(icons) {
    for (const [ref, nodes] of Object.entries(icons)) {
        if (ref.includes(":") && Array.isArray(nodes))
            shapes[ref] = nodes;
    }
}
/** 套用服务端的全站图标配置，并重画页面上已有的图标。 */
export function applyIconConfig(cfg) {
    if (cfg.icons)
        registerIconShapes(cfg.icons);
    overrides = { ...(cfg.overrides || {}) };
    for (const svg of document.querySelectorAll("svg.ui-icon[data-icon-slot]")) {
        paint(svg, svg.dataset.iconSlot);
    }
}
/* 全站配置缓存在本浏览器：首屏先按缓存画，避免先闪一下默认图标；随后向服务端取最新的。 */
const CACHE_KEY = "agentbox.icons";
export function loadCachedIconConfig() {
    try {
        const raw = localStorage.getItem(CACHE_KEY);
        if (raw)
            applyIconConfig(JSON.parse(raw));
    }
    catch { /* 缓存坏了或存储被禁用：用默认图标 */ }
}
/** 套用并缓存服务端返回的配置（管理员保存后也走这里）。 */
export function storeIconConfig(cfg) {
    applyIconConfig(cfg);
    try {
        if (Object.keys(cfg.overrides || {}).length)
            localStorage.setItem(CACHE_KEY, JSON.stringify({ overrides: cfg.overrides, icons: cfg.icons }));
        else
            localStorage.removeItem(CACHE_KEY);
    }
    catch { /* 存储被禁用时只影响下次首屏 */ }
}
export async function refreshIconConfig() {
    try {
        const res = await fetch("/api/icons", { cache: "no-store" });
        if (res.ok)
            storeIconConfig(await res.json());
    }
    catch { /* 离线或旧服务端：保持当前图标 */ }
}
function shapeElement(node) {
    const ns = "http://www.w3.org/2000/svg";
    const [tag, attrs] = typeof node === "string" ? ["path", { d: node }] : node;
    const allowed = TAGS[tag];
    if (!allowed)
        return null;
    const el = document.createElementNS(ns, tag);
    for (const [k, v] of Object.entries(attrs)) {
        if (allowed.includes(k) || PAINT[k]?.includes(v))
            el.setAttribute(k, v);
    }
    return el;
}
/** 按图标库引用画到给定 svg 上（选择器预览也用它）。 */
export function paintIconRef(svg, ref) {
    const nodes = shapes[ref];
    if (!nodes)
        throw new Error(`Icon not loaded: ${ref}`);
    const fill = ICON_SET_STYLES[ref.slice(0, ref.indexOf(":"))] === "fill";
    svg.setAttribute("viewBox", "0 0 24 24");
    svg.setAttribute("fill", fill ? "currentColor" : "none");
    if (fill) {
        svg.removeAttribute("stroke");
        svg.removeAttribute("stroke-width");
    }
    else {
        svg.setAttribute("stroke", "currentColor");
        svg.setAttribute("stroke-width", STROKE_WIDTH);
        svg.setAttribute("stroke-linecap", "round");
        svg.setAttribute("stroke-linejoin", "round");
    }
    svg.dataset.iconRef = ref;
    svg.replaceChildren(...nodes.map(shapeElement).filter((el) => !!el));
}
function paint(svg, slot) {
    svg.dataset.iconSlot = slot;
    svg.dataset.iconName = slot;
    paintIconRef(svg, resolveIcon(slot));
}
export function iconRefSvg(ref, size = 16) {
    const svg = blankSvg(size);
    paintIconRef(svg, ref);
    return svg;
}
function blankSvg(size) {
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("width", String(size));
    svg.setAttribute("height", String(size));
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("focusable", "false");
    svg.classList.add("ui-icon");
    return svg;
}
/* 文件类型图标：文件列表与技能文件树共用。曾经用 ◨ ▣ ⌘ · 这些字符代替，
 * 风格与其他 SVG 图标不一致，缺字体时还会显示成方块。 */
const IMAGE_EXT = ["png", "jpg", "jpeg", "gif", "svg", "webp", "ico", "bmp", "avif"];
const ARCHIVE_EXT = ["zip", "tar", "gz", "tgz", "7z", "rar", "xz", "bz2", "zst"];
const CODE_EXT = ["sh", "bash", "zsh", "py", "js", "mjs", "cjs", "ts", "tsx", "jsx", "go", "rs", "c", "h", "cc", "cpp", "hpp",
    "java", "kt", "rb", "php", "pl", "swift", "lua", "sql", "css", "scss", "html", "htm", "vue", "svelte"];
export function fileIconName(name) {
    const ext = (name.split(".").pop() || "").toLowerCase();
    if (name === "SKILL.md")
        return "file.skill";
    if (IMAGE_EXT.includes(ext))
        return "file.image";
    if (ARCHIVE_EXT.includes(ext))
        return "file.archive";
    if (CODE_EXT.includes(ext))
        return "file.code";
    return "file.generic";
}
export function svgIcon(slot, size = 13) {
    const svg = blankSvg(size);
    paint(svg, slot);
    return svg;
}
/** Static controls opt in explicitly; never infer actions from user-facing text. */
export function decorateIcons(root = document) {
    for (const el of root.querySelectorAll("[data-icon]")) {
        // SVG slots retain state classes and CSS hooks (theme, send/stop, sidebar).
        const slot = el.dataset.icon;
        if (el instanceof SVGSVGElement) {
            el.setAttribute("aria-hidden", "true");
            el.setAttribute("focusable", "false");
            el.classList.add("ui-icon");
            paint(el, slot);
            continue;
        }
        if (!el.querySelector(":scope > .ui-icon"))
            el.prepend(svgIcon(slot, 16));
        for (const node of [...el.childNodes]) {
            if (node.nodeType === Node.TEXT_NODE && node.textContent?.trim()) {
                const label = document.createElement("span");
                label.className = "action-label";
                node.replaceWith(label);
                label.append(node);
            }
        }
        if (el.matches("button, a")) {
            const label = el.dataset.tip || el.getAttribute("aria-label") || el.textContent?.trim();
            if (label) {
                const source = el.querySelector(":scope > [data-i18n]")?.dataset.i18n;
                if (!el.dataset.tip) {
                    if (source)
                        setAttr(el, "data-tip", source);
                    else
                        el.dataset.tip = label;
                }
                if (!el.hasAttribute("aria-label")) {
                    if (source)
                        setAttr(el, "aria-label", source);
                    else
                        el.setAttribute("aria-label", label);
                }
            }
        }
    }
    // Also normalize legacy actions whose SVG is supplied by their own module.
    for (const el of root.querySelectorAll(".btn")) {
        if (!el.querySelector(":scope > svg"))
            continue;
        for (const node of [...el.childNodes]) {
            // 标签之间的换行缩进别包：包成 span 就成了 flex 项，每个都多吃一份 gap，把图标挤偏
            if (node.nodeType !== Node.TEXT_NODE || !node.textContent?.trim())
                continue;
            const caption = document.createElement("span");
            caption.className = "action-label";
            node.replaceWith(caption);
            caption.append(node);
        }
        const label = el.getAttribute("aria-label") || el.dataset.tip || el.textContent?.trim();
        if (label) {
            el.setAttribute("aria-label", label);
            el.dataset.tip ||= label;
        }
        el.classList.add("action-control");
        el.classList.toggle("action-icon", !el.textContent?.trim());
    }
}
/** Update icon and label together, including state changes and dynamic controls. */
export function buttonLabel(el, label, icon) {
    const previous = el.querySelector(".action-label")?.textContent;
    const value = typeof label === "function" ? label() : label;
    el.dataset.icon = icon;
    const caption = document.createElement("span");
    caption.className = "action-label";
    if (typeof label === "function")
        setTextRender(caption, label);
    else
        caption.textContent = label;
    el.replaceChildren(svgIcon(icon, 16), caption);
    if (el.matches(".btn")) {
        el.classList.add("action-control");
        el.classList.toggle("action-icon", !value);
        if (value) {
            if (!el.dataset.tip || el.dataset.tip === previous) {
                if (typeof label === "function")
                    setAttrRender(el, "data-tip", label);
                else
                    el.dataset.tip = value;
            }
            if (!el.getAttribute("aria-label") || el.getAttribute("aria-label") === previous) {
                if (typeof label === "function")
                    setAttrRender(el, "aria-label", label);
                else
                    el.setAttribute("aria-label", value);
            }
        }
    }
}
export function actionButton(el, label, icon, tip = label) {
    buttonLabel(el, label, icon);
    const value = typeof label === "function" ? label() : label;
    const tipValue = typeof tip === "function" ? tip() : tip;
    el.classList.toggle("action-icon", !value);
    if (typeof tip === "function")
        setAttrRender(el, "aria-label", tip);
    else if (!tipValue && typeof label === "function")
        setAttrRender(el, "aria-label", label);
    else
        el.setAttribute("aria-label", tipValue || value);
    el.removeAttribute("title");
    if (typeof tip === "function")
        setAttrRender(el, "data-tip", tip);
    else if (tipValue)
        el.dataset.tip = tipValue;
    else
        delete el.dataset.tip;
    return el;
}
