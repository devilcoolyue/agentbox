/* Shared action icons: 24px grid, 1.8px rounded stroke, currentColor.
 * Keep action semantics independent from danger/primary styling. */
const ICONS: Record<string, string> = {
  "globe": "M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM3 12h18M12 3c5 5 5 13 0 18-5-5-5-13 0-18Z",
  "branch": "M6 8v8M8 6a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM8 18a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM20 6a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM18 8v2a8 8 0 0 1-8 8H8",
  "calendar": "M5 5h14a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2ZM7 3v4m10-4v4M3 11h18",
  "user": "M16 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM4 21v-2a6 6 0 0 1 6-6h4a6 6 0 0 1 6 6v2",
  "lock": "M5 10h14v11H5ZM8 10V7a4 4 0 0 1 8 0v3M12 14v3",
  "eye-off": "m3 3 18 18M10.6 5.6 12 5.5c6.4 0 10 6.5 10 6.5a21 21 0 0 1-3 3.8M6.2 6.2A21 21 0 0 0 2 12s3.6 6.5 10 6.5a13 13 0 0 0 5.8-1.7M9.4 9.4a3.7 3.7 0 0 0 5.2 5.2",
  "tool": "M4 6l6 6-6 6m9 0h7",
  "copy": "M9 5V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2h-1M4 8h10a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V10a2 2 0 0 1 2-2Z",
  "check": "m5 12 4 4L19 6",
  "caret": "m9 6 6 6-6 6",
  "chevron": "m6 9 6 6 6-6",
  "play": "M6.5 3.5 20 12 6.5 20.5Z",
  "stop": "M7 5h10a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2Z",
  "trash": "M3 6h18M8 6V4.5A1.5 1.5 0 0 1 9.5 3h5A1.5 1.5 0 0 1 16 4.5V6M5.5 6l1 14.5h11L18.5 6",
  "move": "M3 7V5a2 2 0 0 1 2-2h5l3 4h6a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7ZM8 14h8m-3-3 3 3-3 3",
  "folder": "M3 7V5a2 2 0 0 1 2-2h5l3 4h6a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7Z",
  "rename": "M4 20h4L18.5 9.5a2.12 2.12 0 0 0-3-3L5 17zM13.5 6.5l3 3",
  "edit": "M12 5H5a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-7M16 3a2.12 2.12 0 0 1 3 3L10 15l-4 1 1-4 9-9Zm-1.5 1.5 3 3",
  "download": "M12 3.5v11m0 0 4.5-4.5M12 14.5 7.5 10M4.5 16v3.5h15V16",
  "clock": "M12 21a9 9 0 1 1 0-18 9 9 0 0 1 0 18ZM12 7.2v5l3.4 2",
  "gauge": "M3.34 19a10 10 0 1 1 17.32 0M12 14l4-4",
  "eye": "M2 12s3.6-6.5 10-6.5S22 12 22 12s-3.6 6.5-10 6.5S2 12 2 12Zm10 2.6a2.6 2.6 0 1 0 0-5.2 2.6 2.6 0 0 0 0 5.2Z",
  "plus": "M12 5v14M5 12h14",
  "close": "m6 6 12 12M6 18 18 6",
  "refresh": "M20 7v5h-5M4 17v-5h5M6.1 6.1A8 8 0 0 1 20 12M4 12a8 8 0 0 0 13.9 5.9",
  "save": "M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h12l4 4v12a2 2 0 0 1-2 2ZM7 3v6h10V3M7 21v-8h10v8",
  "upload": "M12 15V4m-4 4 4-4 4 4M4 16v4h16v-4",
  "folder-plus": "M3 7V5a2 2 0 0 1 2-2h5l3 4h6a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7ZM12 11v6m-3-3h6",
  "undo": "M9 4 4 9l5 5M4 9h10a6 6 0 0 1 0 12",
  "commit": "M3 12h5m8 0h5M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0Z",
  "code": "m8 7-5 5 5 5m8-10 5 5-5 5m-3-14-2 18",
  "diff": "M5 3h9l5 5v13H5ZM14 3v5h5M8 12h8m-4-3v6m-4 3h8",
  "box": "m12 3 9 5v9l-9 5-9-5V8ZM3 8l9 5 9-5M12 13v9M7.5 5.5l9 5",
  "users": "M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M13 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM17 4a4 4 0 0 1 0 8m2 4a4 4 0 0 1 3 4v1",
  "key": "M11.5 12.5 21 3m-4 4 3 3m-6-6 3 3M13 16a5 5 0 1 1-10 0 5 5 0 0 1 10 0Z",
  "login": "M14 3h5v18h-5M3 12h12m-5-5 5 5-5 5",
  "shield": "M12 3 3 7v5c0 5 9 10 9 10s9-5 9-10V7ZM8 12l3 3 5-6",
  "network": "M8 3h8v6H8ZM3 17h6v4H3Zm12 0h6v4h-6ZM12 9v4M6 17v-4h12v4",
  "sliders": "M4 7h7m4 0h5M4 17h11m4 0h1M11 4v6M15 14v6",
  "cpu": "M6 6h12v12H6ZM9 9h6v6H9ZM9 3v3m6-3v3m-6 12v3m6-3v3M3 9h3m-3 6h3m12-6h3m-3 6h3",
  "wallet": "M20 8V5H5a2 2 0 0 0 0 4h16v12H5a2 2 0 0 1-2-2V7m18 6h-6v4h6",
  "activity": "M3 12h4l3-9 4 18 3-9h4",
  "info": "M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM12 11v6m0-10v.01",
  "link": "m10 13 4-4M8 16l-1 1a3.5 3.5 0 0 1-5-5l4-4a3.5 3.5 0 0 1 5 0m2 8a3.5 3.5 0 0 0 5 0l4-4a3.5 3.5 0 0 0-5-5l-1 1",
  "external": "M14 3h7v7m0-7L10 14M10 3H3v18h18v-7",
  "expand": "M8 3H3v5m13-5h5v5M3 16v5h5m13-5v5h-5",
  "collapse": "M3 8h5V3m8 0v5h5M8 21v-5H3m13 5v-5h5",
  "desktop": "M3 3h18v14H3ZM12 17v4m-5 0h10",
  "tablet": "M5 2h14v20H5ZM12 18v.01",
  "phone": "M7 2h10v20H7ZM12 18v.01",
  "arrow-left": "M20 12H4m7-7-7 7 7 7",
  "arrow-right": "M4 12h16m-7-7 7 7-7 7",
  "chevron-left": "m15 6-6 6 6 6",
  "chevron-right": "m9 6 6 6-6 6",
  "more": "M5 12h.01M12 12h.01M19 12h.01",
  "sparkles": "m12 3 2.5 6.5L21 12l-6.5 2.5L12 21l-2.5-6.5L3 12l6.5-2.5Z",
  "bug": "M8 8h8v8a4 4 0 0 1-8 0ZM9 8V6a3 3 0 0 1 6 0v2M3 9l5 2m8 0 5-2M3 15h5m8 0h5M4 21l4-3m8 0 4 3M12 8v12",
  "list-check": "m3 6 1 1 2-2m-3 8 1 1 2-2m-3 8 1 1 2-2M10 6h11M10 13h11M10 20h11",
  "unlink": "m9 15-2 2a3.5 3.5 0 0 1-5-5l2-2m11-1 2-2a3.5 3.5 0 0 1 5 5l-2 2M3 3l18 18M9 3v3M3 9h3m12 0h3m-12 9v3",
  "logout": "M10 3H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h5M10 12h11m-4-4 4 4-4 4",
  "template": "M14 3H5v18h14V8l-5-5ZM14 3v5h5M8 3v9l2-1.5 2 1.5V3M8 16h7",
  "folder-shared": "M21 11V9a2 2 0 0 0-2-2h-6l-3-4H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h4m5-7h-1a3 3 0 0 0 0 6h1m4-6h1a3 3 0 0 1 0 6h-1m-5-3h6",
  "package-plus": "m12 3 9 5-9 5-9-5 9-5ZM3 8v9l9 5V13m9-5v4M7.5 5.5l9 5M18 15v6m-3-3h6",
  "store": "M3 9l2-6h14l2 6M3 9v2a3 3 0 0 0 6 0V9m0 0v2a3 3 0 0 0 6 0V9m0 0v2a3 3 0 0 0 6 0V9H3m2 5v7h14v-7M10 21v-5h4v5",
  "list": "M5 3h14v18l-3-2-4 2-4-2-3 2V3Zm3 5h8m-8 4h8m-8 4h4",
  "history": "M3 11a9 9 0 1 1 2 7M3 4v7h7m2-4v5l3 2",
  "git-fetch": "M7 5a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM5 7v10m2 2a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM17 3v12m-4-4 4 4 4-4m-8 10h8",
  "git-pull": "M7 5a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM5 7v10m2 2a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM19 3v8a8 8 0 0 1-8 8H9m3-3-3 3 3 3",
  "git-push": "M7 5a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM5 7v10m2 2a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM17 21V3m-4 4 4-4 4 4",
  "git-clone": "M3 8V5h7l3 3h8v13H3V8Zm9 2v8m-3-3 3 3 3-3",
  "git-pr": "M7 5a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM5 7v10m2 2a2 2 0 1 1-4 0 2 2 0 0 1 4 0ZM21 19a2 2 0 1 1-4 0 2 2 0 0 1 4 0Zm-2-2V9a4 4 0 0 0-4-4h-3m3-3-3 3 3 3",
  "shield-off": "M12 3 3 7v5c0 5 9 10 9 10s9-5 9-10V7L12 3Zm-3 7 6 6m0-6-6 6",
  "unlock": "M5 11h14v10H5V11Zm4 0V7a4 4 0 0 1 7.5-2M12 15v2",
  "menu": "M4 5h16M4 12h16M4 19h16",
  "panel-left": "M5 3h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2ZM9 3v18m7-13-4 4 4 4",
  "message": "M21 11a8 8 0 0 1-8 8H7l-4 3V5a2 2 0 0 1 2-2h8a8 8 0 0 1 8 8Z",
  "paperclip": "m20 11-9 9a6 6 0 0 1-8.5-8.5l9-9a4 4 0 0 1 5.7 5.7l-9 9a2 2 0 0 1-2.9-2.9l8-8",
  "mic": "M9 5a3 3 0 0 1 6 0v7a3 3 0 0 1-6 0V5ZM5 10v2a7 7 0 0 0 14 0v-2M12 19v3M8 22h8",
  "send": "m21 3-9 9m9-9-6 18-3-9-9-3 18-6Z",
  "arrow-down": "M12 4v16m-6-6 6 6 6-6",
  "chevron-up": "m6 15 6-6 6 6",
  "plug": "M8 3v5m8-5v5M6 8h12v3a6 6 0 0 1-12 0V8ZM12 17v5",
  "keyboard": "M4 4h16a2 2 0 0 1 2 2v12H2V6a2 2 0 0 1 2-2ZM6 8h.01M10 8h.01M14 8h.01M18 8h.01M6 11h.01M10 11h.01M14 11h.01M18 11h.01M7 15h10M9 21h6",
  "sun": "M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.5 1.5m11 11L19 19M5 19l1.5-1.5m11-11L19 5",
  "moon": "M21 13a9 9 0 1 1-10-10 7 7 0 0 0 10 10Z",
  "bulb": "M9 18h6m-5 4h4M9 18v-2c0-2-3-3-3-7a6 6 0 0 1 12 0c0 4-3 5-3 7v2",
  "help": "M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM9 9a3 3 0 0 1 6 0c0 2-3 2-3 4m0 4h.01",
  "settings": "M15.2 12a3.2 3.2 0 1 1-6.4 0 3.2 3.2 0 0 1 6.4 0ZM19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09a1.65 1.65 0 0 0-1-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09a1.65 1.65 0 0 0 1.51-1 1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33h.01a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51h.01a1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82v.01a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1Z",
};

export function svgIcon(name: string, size = 13) {
  const d = ICONS[name];
  if (!d) throw new Error(`Unknown action icon: ${name}`);
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("width", String(size));
  svg.setAttribute("height", String(size));
  svg.setAttribute("fill", "none");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");
  svg.classList.add("ui-icon");
  svg.dataset.iconName = name;
  const p = document.createElementNS(ns, "path");
  p.setAttribute("d", d);
  p.setAttribute("stroke", "currentColor");
  p.setAttribute("stroke-width", "1.8");
  p.setAttribute("stroke-linecap", "round");
  p.setAttribute("stroke-linejoin", "round");
  svg.appendChild(p);
  return svg;
}

/** Static controls opt in explicitly; never infer actions from user-facing text. */
export function decorateIcons(root: ParentNode = document) {
  for (const el of root.querySelectorAll<HTMLElement | SVGSVGElement>("[data-icon]")) {
    // SVG slots retain state classes and CSS hooks (theme, send/stop, sidebar).
    if (el instanceof SVGSVGElement) {
      const icon = svgIcon(el.dataset.icon!, Number(el.getAttribute("width")) || 16);
      for (const attr of [...icon.attributes]) {
        if (attr.name !== "class" && attr.name !== "width" && attr.name !== "height") el.setAttribute(attr.name, attr.value);
      }
      el.classList.add("ui-icon");
      el.replaceChildren(...icon.childNodes);
      continue;
    }
    if (!el.querySelector(":scope > .ui-icon")) el.prepend(svgIcon(el.dataset.icon!, 16));
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
        el.dataset.tip ||= label;
        if (!el.hasAttribute("aria-label")) el.setAttribute("aria-label", label);
      }
    }
  }
  // Also normalize legacy actions whose SVG is supplied by their own module.
  for (const el of root.querySelectorAll<HTMLElement>(".btn")) {
    if (!el.querySelector(":scope > svg")) continue;
    for (const node of [...el.childNodes]) {
      if (node.nodeType !== Node.TEXT_NODE) continue;
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
export function buttonLabel(el: HTMLElement, label: string, icon: string) {
  const previous = el.querySelector(".action-label")?.textContent;
  el.dataset.icon = icon;
  const caption = document.createElement("span");
  caption.className = "action-label";
  caption.textContent = label;
  el.replaceChildren(svgIcon(icon, 16), caption);
  if (el.matches(".btn")) {
    el.classList.add("action-control");
    el.classList.toggle("action-icon", !label);
    if (label) {
      if (!el.dataset.tip || el.dataset.tip === previous) el.dataset.tip = label;
      if (!el.getAttribute("aria-label") || el.getAttribute("aria-label") === previous) el.setAttribute("aria-label", label);
    }
  }
}

export function actionButton<T extends HTMLElement>(el: T, label: string, icon: string, tip = label): T {
  buttonLabel(el, label, icon);
  el.classList.toggle("action-icon", !label);
  el.setAttribute("aria-label", tip || label);
  el.removeAttribute("title");
  if (tip) el.dataset.tip = tip;
  else delete el.dataset.tip;
  return el;
}
