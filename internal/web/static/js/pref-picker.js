import { setAttrRender, setTextRender, t as i18nText } from "./i18n.js";
/* pref-picker：用户弹层里语言、风格各占一行——左边名称，右边「当前选择 + 向右箭头」。
 * 箭头展开时转向上方，全部选项在这一行上方弹出，当前项打勾。
 *
 * 菜单是这一行的子节点：鼠标移进菜单不算离开用户弹层，选完弹层还在。
 * - 鼠标：悬停右侧展开，离开这一行与菜单稍后收起；点击也能开关。
 * - 触屏没有悬停：点右侧开关，点选项或点别处收起。
 * - 键盘：Enter/Space/↑↓ 展开并聚焦，菜单内 ↑↓/Home/End 移动，Esc 只收起菜单
 *   （不连带关闭用户弹层），Tab 回到触发按钮继续。
 * 上方放不下（窗口很矮）时改从下方弹出。同一时刻只开一个菜单。
 * 选项可以是函数（风格菜单里的自定义主题随登录用户变化），refresh() 重建菜单；
 * 带 group 的选项在组变化处插一行组名；action 是菜单末尾的一条操作（「管理主题…」），
 * 不是可选值，不参与选中态。 */
"use strict";
import { svgIcon } from "./icons.js";
const hoverMQ = window.matchMedia("(hover: hover) and (pointer: fine)");
let nextID = 0;
/** 正开着的那个菜单；打开另一个时先把它收掉，免得两个叠在一起 */
let openPicker;
function swatchFor(color) {
    const swatch = document.createElement("span");
    swatch.className = "pref-swatch";
    swatch.setAttribute("aria-hidden", "true");
    swatch.style.setProperty("--swatch", color);
    return swatch;
}
export function mountPrefPicker(root, cfg) {
    const id = `pref-picker-${++nextID}`;
    root.classList.add("pref-picker");
    const title = document.createElement("span");
    title.id = id + "-title";
    title.className = "pref-picker-title";
    setTextRender(title, cfg.title);
    const trigger = document.createElement("button");
    trigger.type = "button";
    trigger.className = "pref-trigger";
    trigger.setAttribute("aria-haspopup", "menu");
    trigger.setAttribute("aria-expanded", "false");
    trigger.setAttribute("aria-controls", id + "-menu");
    const swatch = swatchFor("");
    swatch.hidden = true;
    const current = document.createElement("span");
    current.className = "pref-current";
    trigger.append(swatch, current, svgIcon("chevron-right", 14));
    const menu = document.createElement("div");
    menu.id = id + "-menu";
    menu.className = "pref-menu";
    menu.setAttribute("role", "menu");
    menu.setAttribute("aria-labelledby", title.id);
    menu.hidden = true;
    const optionList = () => typeof cfg.options === "function" ? cfg.options() : cfg.options;
    function renderMenu() {
        const nodes = [];
        let group;
        for (const option of optionList()) {
            const name = option.group?.();
            if (name !== undefined && name !== group) {
                const heading = document.createElement("div");
                heading.className = "pref-menu-group";
                heading.setAttribute("role", "presentation");
                setTextRender(heading, option.group);
                nodes.push(heading);
            }
            group = name;
            const item = document.createElement("button");
            item.type = "button";
            item.className = "pref-menu-item";
            item.setAttribute("role", "menuitemradio");
            item.tabIndex = -1;
            item.dataset.value = option.value;
            if (option.swatch)
                item.append(swatchFor(option.swatch));
            const label = document.createElement("span");
            label.className = "pref-menu-label";
            setTextRender(label, option.label);
            const check = svgIcon("check", 14);
            check.classList.add("pref-menu-check");
            item.append(label, check);
            nodes.push(item);
        }
        if (cfg.action) {
            const separator = document.createElement("div");
            separator.className = "pref-menu-sep";
            separator.setAttribute("role", "separator");
            const item = document.createElement("button");
            item.type = "button";
            item.className = "pref-menu-item pref-menu-action";
            item.setAttribute("role", "menuitem");
            item.tabIndex = -1;
            item.dataset.action = "true";
            const label = document.createElement("span");
            label.className = "pref-menu-label";
            setTextRender(label, cfg.action.label);
            item.append(svgIcon(cfg.action.icon, 14), label);
            nodes.push(separator, item);
        }
        menu.replaceChildren(...nodes);
    }
    renderMenu();
    root.replaceChildren(title, trigger, menu);
    const items = () => [...menu.querySelectorAll(".pref-menu-item")];
    const chosen = () => optionList().find(o => o.value === cfg.value());
    let closeTimer;
    let pointer = "";
    /** 由悬停展开：紧接着的那次点击是「点开」而不是「关上」 */
    let hoverOpened = false;
    function sync() {
        const value = cfg.value();
        for (const item of items()) {
            const on = item.dataset.value === value;
            item.classList.toggle("active", on);
            item.setAttribute("aria-checked", String(on));
        }
        const option = chosen();
        swatch.hidden = !option?.swatch;
        if (option?.swatch)
            swatch.style.setProperty("--swatch", option.swatch);
        trigger.dataset.value = value;
        setTextRender(current, () => chosen()?.label() ?? cfg.value());
        setAttrRender(trigger, "aria-label", () => i18nText("{p0}，当前：{p1}", { p0: cfg.title(), p1: chosen()?.label() ?? cfg.value() }));
    }
    function setOpen(open) {
        clearTimeout(closeTimer);
        closeTimer = undefined;
        if (!open)
            hoverOpened = false;
        if (menu.hidden === !open)
            return;
        if (open && openPicker && openPicker !== picker)
            openPicker.close();
        openPicker = open ? picker : openPicker === picker ? undefined : openPicker;
        menu.hidden = !open;
        root.classList.toggle("open", open);
        trigger.setAttribute("aria-expanded", String(open));
        if (!open)
            return;
        // 默认在这一行上方；顶到窗口上沿就改到下方
        root.classList.remove("below");
        if (menu.getBoundingClientRect().top < 8)
            root.classList.add("below");
    }
    const close = () => setOpen(false);
    const refresh = () => { renderMenu(); sync(); };
    const picker = { sync, close, isOpen: () => !menu.hidden, refresh };
    function focusItem(which) {
        const list = items();
        const target = which === "last" ? list.at(-1) : which === "first" ? list[0] : list.find(i => i.classList.contains("active")) || list[0];
        target?.focus();
    }
    menu.addEventListener("click", e => {
        const item = e.target.closest(".pref-menu-item");
        if (!item)
            return;
        const fromKeyboard = e.detail === 0;
        close();
        if (item.dataset.action) {
            cfg.action?.run();
            return;
        }
        cfg.select(item.dataset.value);
        sync();
        // 键盘选完回到触发按钮；鼠标点中的那项已随菜单隐藏，别让焦点停在看不见的按钮上
        if (fromKeyboard)
            trigger.focus();
        else if (menu.contains(document.activeElement))
            document.activeElement.blur();
    });
    trigger.addEventListener("pointerdown", e => { pointer = e.pointerType; });
    trigger.addEventListener("click", e => {
        const keyboard = e.detail === 0; // Enter / Space 触发的 click 没有点击次数
        const how = keyboard ? "" : pointer;
        pointer = "";
        if (keyboard || how === "") {
            // 键盘展开后把焦点放进菜单；已经展开就只挪焦点
            setOpen(true);
            focusItem("selected");
            return;
        }
        if (!menu.hidden && !hoverOpened) {
            close();
            return;
        }
        // 鼠标点开与悬停相同：移出即收
        setOpen(true);
        hoverOpened = false;
    });
    trigger.addEventListener("keydown", e => {
        if (e.key !== "ArrowDown" && e.key !== "ArrowUp")
            return;
        e.preventDefault();
        setOpen(true);
        focusItem(e.key === "ArrowUp" ? "last" : "first");
    });
    trigger.addEventListener("pointerenter", e => {
        if (e.pointerType !== "mouse" || !hoverMQ.matches || !menu.hidden)
            return;
        setOpen(true);
        hoverOpened = true;
    });
    root.addEventListener("pointerenter", () => { clearTimeout(closeTimer); closeTimer = undefined; });
    root.addEventListener("pointerleave", e => {
        if (e.pointerType !== "mouse" || menu.hidden || menu.contains(document.activeElement))
            return;
        clearTimeout(closeTimer);
        // 留一点时间跨过这一行与菜单之间的空隙
        closeTimer = setTimeout(close, 180);
    });
    menu.addEventListener("keydown", e => {
        const list = items();
        const index = list.indexOf(document.activeElement);
        let next;
        if (e.key === "ArrowDown")
            next = (index + 1) % list.length;
        else if (e.key === "ArrowUp")
            next = (index - 1 + list.length) % list.length;
        else if (e.key === "Home")
            next = 0;
        else if (e.key === "End")
            next = list.length - 1;
        else if (e.key === "Tab") {
            // 菜单项不在 Tab 顺序里：回到触发按钮，由默认行为从那里继续前后移动
            trigger.focus();
            close();
            return;
        }
        else
            return;
        e.preventDefault();
        list[next]?.focus();
    });
    root.addEventListener("focusout", e => {
        if (!root.contains(e.relatedTarget))
            close();
    });
    // 点在这一行以外（含用户弹层里的其他位置）只收起菜单；弹层自己的去留由 shell 决定
    document.addEventListener("pointerdown", e => {
        if (!menu.hidden && !root.contains(e.target))
            close();
    });
    // 捕获阶段先处理：第一次 Esc 只收起菜单，用户弹层还在
    document.addEventListener("keydown", e => {
        if (e.key !== "Escape" || menu.hidden)
            return;
        e.preventDefault();
        e.stopPropagation();
        const focused = menu.contains(document.activeElement);
        close();
        if (focused)
            trigger.focus();
    }, true);
    sync();
    return picker;
}
