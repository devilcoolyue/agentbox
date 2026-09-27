/* ---- 行内小图标：stroke 线稿，颜色随 currentColor ---- */
const ICONS = {
    branch: { d: "M6 8v8M8 6a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM8 18a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM20 6a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM18 8v2a8 8 0 0 1-8 8H8", box: 24, width: 1.8 },
    calendar: { d: "M3 5h18v16H3ZM7 3v4m10-4v4M3 11h18", box: 24, width: 1.8 },
    user: { d: "M16 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM4 21v-2a6 6 0 0 1 6-6h4a6 6 0 0 1 6 6v2", box: 24, width: 1.8 },
    lock: { d: "M5 10h14v11H5ZM8 10V7a4 4 0 0 1 8 0v3M12 14v3", box: 24, width: 1.8 },
    "eye-off": { d: "m3 3 18 18M10.6 5.6 12 5.5c6.4 0 10 6.5 10 6.5a21 21 0 0 1-3 3.8M6.2 6.2A21 21 0 0 0 2 12s3.6 6.5 10 6.5a13 13 0 0 0 5.8-1.7M9.4 9.4a3.7 3.7 0 0 0 5.2 5.2", box: 24, width: 1.8 },
    tool: "M3.75 4.5 8.25 9l-4.5 4.5M9.75 13.5h4.5",
    copy: "M6.75 6V4.13c0-.62.5-1.13 1.13-1.13h6c.62 0 1.12.5 1.12 1.13v6c0 .62-.5 1.12-1.12 1.12H12M3 7.88c0-.63.5-1.13 1.13-1.13h6c.62 0 1.12.5 1.12 1.13v6c0 .62-.5 1.12-1.13 1.12h-6C3.5 15 3 14.5 3 13.88Z",
    check: "M3.75 9.75 7.5 13.5l6.75-8.25",
    caret: "M6.75 3.75 12 9l-5.25 5.25",
    chevron: "M3.75 6.75 9 12l5.25-5.25",
    /* 会话动作三件套：与侧栏（内网隧道/系统设置/退出登录）同为 24 视框、1.8 线宽，
     * 同尺寸渲染时观感才一致——18 视框的字形留白更多，会显得小一号。 */
    play: { d: "M6.5 3.5 20 12 6.5 20.5Z", box: 24, width: 1.8 },
    stop: { d: "M5.5 5.5h13v13h-13Z", box: 24, width: 1.8 },
    trash: { d: "M3 6h18M8 6V4.5A1.5 1.5 0 0 1 9.5 3h5A1.5 1.5 0 0 1 16 4.5V6M5.5 6l1 14.5h11L18.5 6", box: 24, width: 1.8 },
    move: { d: "M3 7h6l2 2h10v11H3ZM8 14h8m-3-3 3 3-3 3", box: 24, width: 1.8 },
    folder: { d: "M3 7h6l2 2h10v11H3Z", box: 24, width: 1.8 },
    rename: { d: "M4 20h4L18.5 9.5a2.12 2.12 0 0 0-3-3L5 17zM13.5 6.5l3 3", box: 24, width: 1.8 },
    download: { d: "M12 3.5v11m0 0 4.5-4.5M12 14.5 7.5 10M4.5 16v3.5h15V16", box: 24, width: 1.8 },
    /* 历史对话入口：表盘 + 指针 */
    clock: { d: "M12 21a9 9 0 1 1 0-18 9 9 0 0 1 0 18ZM12 7.2v5l3.4 2", box: 24, width: 1.8 },
    /* 账号额度：仪表盘弧 + 指针。弧要占满 2–22 / 4–19，否则挤在下半格，
     * 和同排的 play/stop/trash（都撑到 3–21）摆一起会明显小一号。 */
    gauge: { d: "M3.34 19a10 10 0 1 1 17.32 0M12 14l4-4", box: 24, width: 1.8 },
    /* HTML 渲染预览：眼睛 */
    eye: { d: "M2 12s3.6-6.5 10-6.5S22 12 22 12s-3.6 6.5-10 6.5S2 12 2 12Zm10 2.6a2.6 2.6 0 1 0 0-5.2 2.6 2.6 0 0 0 0 5.2Z", box: 24, width: 1.8 },
    "plus": { d: "M12 5v14M5 12h14", box: 24, width: 1.8 },
    "close": { d: "m6 6 12 12M6 18 18 6", box: 24, width: 1.8 },
    "refresh": { d: "M20 7v5h-5M4 17v-5h5M6.1 6.1A8 8 0 0 1 20 12M4 12a8 8 0 0 0 13.9 5.9", box: 24, width: 1.8 },
    "save": { d: "M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h12l4 4v12a2 2 0 0 1-2 2ZM7 3v6h10V3M7 21v-8h10v8", box: 24, width: 1.8 },
    "upload": { d: "M12 15V4m-4 4 4-4 4 4M4 16v4h16v-4", box: 24, width: 1.8 },
    "folder-plus": { d: "M3 7h6l2 2h10v11H3ZM12 12v5m-2.5-2.5h5", box: 24, width: 1.8 },
    "undo": { d: "M9 4 4 9l5 5M4 9h10a6 6 0 0 1 0 12", box: 24, width: 1.8 },
    "commit": { d: "M3 12h5m8 0h5M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0Z", box: 24, width: 1.8 },
    "code": { d: "m8 7-5 5 5 5m8-10 5 5-5 5m-3-14-2 18", box: 24, width: 1.8 },
    "diff": { d: "M5 3h9l5 5v13H5ZM14 3v5h5M8 12h8m-4-3v6m-4 3h8", box: 24, width: 1.8 },
    "box": { d: "m12 3 9 5v9l-9 5-9-5V8ZM3 8l9 5 9-5M12 13v9M7.5 5.5l9 5", box: 24, width: 1.8 },
    "users": { d: "M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M13 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM17 4a4 4 0 0 1 0 8m2 4a4 4 0 0 1 3 4v1", box: 24, width: 1.8 },
    "key": { d: "m15 8 6-6m-3 3 3 3M16 12a6 6 0 1 1-12 0 6 6 0 0 1 12 0Z", box: 24, width: 1.8 },
    "login": { d: "M14 3h5v18h-5M3 12h12m-5-5 5 5-5 5", box: 24, width: 1.8 },
    "shield": { d: "M12 3 3 7v5c0 5 9 10 9 10s9-5 9-10V7ZM8 12l3 3 5-6", box: 24, width: 1.8 },
    "network": { d: "M8 3h8v6H8ZM3 17h6v4H3Zm12 0h6v4h-6ZM12 9v4M6 17v-4h12v4", box: 24, width: 1.8 },
    "sliders": { d: "M4 7h7m4 0h5M4 17h11m4 0h1M11 4v6M15 14v6", box: 24, width: 1.8 },
    "cpu": { d: "M6 6h12v12H6ZM9 9h6v6H9ZM9 3v3m6-3v3m-6 12v3m6-3v3M3 9h3m-3 6h3m12-6h3m-3 6h3", box: 24, width: 1.8 },
    "wallet": { d: "M20 8V5H5a2 2 0 0 0 0 4h16v12H5a2 2 0 0 1-2-2V7m18 6h-6v4h6", box: 24, width: 1.8 },
    "activity": { d: "M3 12h4l3-9 4 18 3-9h4", box: 24, width: 1.8 },
    "info": { d: "M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM12 11v6m0-10v.01", box: 24, width: 1.8 },
    "link": { d: "M10 13a5 5 0 0 0 7 .5l3-3a5 5 0 0 0-7-7L11 5M14 11a5 5 0 0 0-7-.5l-3 3a5 5 0 0 0 7 7l2-1.5", box: 24, width: 1.8 },
    "external": { d: "M14 3h7v7m0-7L10 14M10 3H3v18h18v-7", box: 24, width: 1.8 },
    "expand": { d: "M8 3H3v5m13-5h5v5M3 16v5h5m13-5v5h-5", box: 24, width: 1.8 },
    "collapse": { d: "M3 8h5V3m8 0v5h5M8 21v-5H3m13 5v-5h5", box: 24, width: 1.8 },
    "desktop": { d: "M3 3h18v14H3ZM12 17v4m-5 0h10", box: 24, width: 1.8 },
    "tablet": { d: "M5 2h14v20H5ZM12 18v.01", box: 24, width: 1.8 },
    "phone": { d: "M7 2h10v20H7ZM12 18v.01", box: 24, width: 1.8 },
    "arrow-left": { d: "M20 12H4m7-7-7 7 7 7", box: 24, width: 1.8 },
    "arrow-right": { d: "M4 12h16m-7-7 7 7-7 7", box: 24, width: 1.8 },
    "chevron-left": { d: "m15 6-6 6 6 6", box: 24, width: 1.8 },
    "chevron-right": { d: "m9 6 6 6-6 6", box: 24, width: 1.8 },
    "more": { d: "M5 12h.01M12 12h.01M19 12h.01", box: 24, width: 1.8 },
    "sparkles": { d: "m12 3 2.5 6.5L21 12l-6.5 2.5L12 21l-2.5-6.5L3 12l6.5-2.5Z", box: 24, width: 1.8 },
    "bug": { d: "M8 8h8v8a4 4 0 0 1-8 0ZM9 8V6a3 3 0 0 1 6 0v2M3 9l5 2m8 0 5-2M3 15h5m8 0h5M4 21l4-3m8 0 4 3M12 8v12", box: 24, width: 1.8 },
    "list-check": { d: "m3 6 1 1 2-2m-3 8 1 1 2-2m-3 8 1 1 2-2M10 6h11M10 13h11M10 20h11", box: 24, width: 1.8 },
};
export function svgIcon(name, size = 13) {
    const ico = ICONS[name];
    const { d, box = 18, width = 1.5 } = typeof ico === "string" ? { d: ico } : ico;
    const ns = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", `0 0 ${box} ${box}`);
    svg.setAttribute("width", String(size));
    svg.setAttribute("height", String(size));
    svg.setAttribute("fill", "none");
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("focusable", "false");
    svg.classList.add("ui-icon");
    const p = document.createElementNS(ns, "path");
    p.setAttribute("d", d);
    p.setAttribute("stroke", "currentColor");
    p.setAttribute("stroke-width", String(width));
    p.setAttribute("stroke-linecap", "round");
    p.setAttribute("stroke-linejoin", "round");
    svg.appendChild(p);
    return svg;
}
/** Static controls opt in explicitly; never infer actions from user-facing text. */
export function decorateIcons(root = document) {
    for (const el of root.querySelectorAll("[data-icon]")) {
        if (!el.querySelector(":scope > .ui-icon"))
            el.prepend(svgIcon(el.dataset.icon, 16));
        if (el.matches("button, a")) {
            const label = el.dataset.tip || el.getAttribute("aria-label") || el.textContent?.trim();
            if (label) {
                el.dataset.tip ||= label;
                if (!el.hasAttribute("aria-label"))
                    el.setAttribute("aria-label", label);
            }
        }
    }
}
/** Update icon and label together, including state changes and dynamic controls. */
export function buttonLabel(el, label, icon) {
    el.dataset.icon = icon;
    el.replaceChildren(svgIcon(icon, 16), document.createTextNode(label));
}
export function actionButton(el, label, icon, tip = label) {
    buttonLabel(el, label, icon);
    el.classList.toggle("action-icon", !label);
    el.setAttribute("aria-label", tip || label);
    el.removeAttribute("title");
    if (tip)
        el.dataset.tip = tip;
    else
        delete el.dataset.tip;
    return el;
}
