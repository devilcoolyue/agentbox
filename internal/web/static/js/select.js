/* 统一单选下拉。原 select 保留为表单数据源，用户选择仍派发 input/change。
 * 选项、disabled、hidden 等 DOM 变化自动同步；代码赋值请用 setSelectValue，
 * 因为浏览器的 select.value / selectedIndex 属性赋值不会触发 DOM mutation。
 * 新增控件可调用 enhanceSelects(root)，无需手写按钮、菜单或键盘处理。 */
const controls = new WeakMap();
let nextID = 0;
class SelectControl {
    select;
    trigger = document.createElement("button");
    caption = document.createElement("span");
    panel = document.createElement("div");
    search = document.createElement("input");
    list = document.createElement("div");
    empty = document.createElement("div");
    rows = [];
    active = -1;
    typeahead = "";
    typedAt = 0;
    labels;
    tabIndex;
    constructor(select) {
        this.select = select;
        const id = `select-${++nextID}`;
        this.labels = [...select.labels || []];
        this.tabIndex = select.tabIndex;
        this.trigger.type = "button";
        this.trigger.id = id;
        this.trigger.setAttribute("role", "combobox");
        this.trigger.setAttribute("aria-haspopup", "listbox");
        this.trigger.setAttribute("aria-expanded", "false");
        this.trigger.setAttribute("aria-controls", id + "-list");
        this.trigger.setAttribute("popovertarget", id + "-panel");
        this.caption.className = "select-caption";
        this.trigger.append(this.caption);
        const caret = document.createElement("span");
        caret.className = "select-caret";
        caret.setAttribute("aria-hidden", "true");
        this.trigger.append(caret);
        this.panel.id = id + "-panel";
        this.panel.className = "select-panel";
        this.panel.setAttribute("popover", "auto");
        this.search.type = "search";
        this.search.className = "select-search";
        this.search.placeholder = "搜索选项…";
        this.search.autocomplete = "off";
        this.search.spellcheck = false;
        this.search.setAttribute("role", "combobox");
        this.search.setAttribute("aria-autocomplete", "list");
        this.search.setAttribute("aria-controls", id + "-list");
        this.list.id = id + "-list";
        this.list.className = "select-options";
        this.list.setAttribute("role", "listbox");
        this.empty.className = "select-empty";
        this.empty.setAttribute("role", "status");
        this.panel.append(this.search, this.list, this.empty);
        // 必须留在 dialog 内，否则顶层弹窗会把 body 上的菜单视为 inert。
        (select.closest("dialog, .git-surface") || document.body).append(this.panel);
        select.after(this.trigger);
        select.classList.add("select-native");
        select.tabIndex = -1;
        select.setAttribute("aria-hidden", "true");
        for (const label of this.labels)
            label.htmlFor = id;
        this.panel.addEventListener("beforetoggle", e => {
            const open = e.newState === "open";
            this.trigger.setAttribute("aria-expanded", String(open));
            this.search.setAttribute("aria-expanded", String(open));
            if (open) {
                this.search.value = "";
                this.typeahead = "";
                this.active = -1;
                this.sync();
                // 等原生展开动作完成，在下一帧绘制前定位（microtask 可能早于原生动作）。
                requestAnimationFrame(() => {
                    if (!this.isOpen())
                        return;
                    this.position();
                    this.setActive(this.active);
                    this.focusActive();
                    if (!this.search.hidden)
                        this.search.focus({ preventScroll: true });
                });
            }
            else {
                this.trigger.removeAttribute("aria-activedescendant");
                this.search.removeAttribute("aria-activedescendant");
            }
        });
        this.search.addEventListener("input", () => { this.render(); this.position(); });
        this.trigger.addEventListener("keydown", e => this.keydown(e));
        this.panel.addEventListener("keydown", e => this.keydown(e));
        // 鼠标点选项时别让列表抢走搜索框的焦点。触摸不能这样拦：Safari 26.5 起在 pointerdown
        // 上 preventDefault 会连这次触摸的滚动一起取消，选项一多手机上就滑不动了。
        this.list.addEventListener("pointerdown", e => { if (e.pointerType === "mouse")
            e.preventDefault(); });
        this.list.addEventListener("pointermove", e => {
            const row = e.target.closest("[data-index]");
            if (row && row.getAttribute("aria-disabled") !== "true")
                this.setActive(Number(row.dataset.index));
        });
        this.list.addEventListener("click", e => {
            const row = e.target.closest("[data-index]");
            if (row)
                this.choose(Number(row.dataset.index));
        });
        select.addEventListener("change", () => this.sync());
        select.addEventListener("input", () => this.sync());
        select.addEventListener("invalid", e => {
            e.preventDefault();
            this.trigger.setAttribute("aria-invalid", "true");
            this.trigger.focus();
        });
        select.form?.addEventListener("reset", () => queueMicrotask(() => this.sync()));
        select.closest("dialog, .git-surface")?.addEventListener("close", () => this.close(false));
        new MutationObserver(() => this.sync()).observe(select, {
            childList: true, subtree: true, characterData: true, attributes: true,
            attributeFilter: ["class", "hidden", "disabled", "selected", "label", "value", "required", "title", "aria-label", "aria-describedby", "data-tip"],
        });
        // 只在面板打开时监听位置变化，避免每个控件常驻一套滚动回调。
        this.panel.addEventListener("toggle", () => {
            const method = this.isOpen() ? "addEventListener" : "removeEventListener";
            window[method]("resize", this.reposition);
            window[method]("scroll", this.reposition, true);
            window.visualViewport?.[method]("resize", this.reposition);
            window.visualViewport?.[method]("scroll", this.reposition);
        });
        this.sync();
    }
    sync() {
        this.trigger.className = "select-trigger " + [...this.select.classList].filter(c => c !== "select-native").join(" ");
        this.trigger.hidden = this.select.hidden;
        this.trigger.disabled = this.select.matches(":disabled");
        this.trigger.tabIndex = this.tabIndex;
        this.caption.textContent = this.select.selectedOptions[0]?.label || "请选择";
        if (this.select.title)
            this.trigger.title = this.select.title;
        else
            this.trigger.removeAttribute("title");
        this.trigger.setAttribute("aria-required", String(this.select.required));
        if (this.select.validity.valid)
            this.trigger.removeAttribute("aria-invalid");
        const labelText = this.labels.map(label => {
            const copy = label.cloneNode(true);
            copy.querySelectorAll("select, button, .hint, .tag, .field-hint").forEach(el => el.remove());
            return copy.textContent?.trim();
        }).filter(Boolean).join("，");
        const name = this.select.getAttribute("aria-label") || labelText || this.select.dataset.tip || "选择选项";
        this.trigger.setAttribute("aria-label", name);
        this.list.setAttribute("aria-label", name);
        this.search.setAttribute("aria-label", `搜索${name}`);
        for (const attr of ["aria-describedby", "data-tip"]) {
            const value = this.select.getAttribute(attr);
            if (value)
                this.trigger.setAttribute(attr, value);
            else
                this.trigger.removeAttribute(attr);
        }
        this.search.hidden = this.select.options.length < 8;
        if (this.isOpen() && (this.trigger.disabled || !this.trigger.getClientRects().length))
            this.close(false);
        this.render();
        if (this.isOpen())
            this.position();
    }
    isOpen() { return this.panel.matches(":popover-open"); }
    disabled(option) {
        return option.disabled || (option.parentElement instanceof HTMLOptGroupElement && option.parentElement.disabled);
    }
    render() {
        const previous = this.rows.find(r => r.index === this.active)?.option;
        this.rows = [];
        this.list.replaceChildren();
        const query = this.search.hidden ? "" : this.search.value.trim().toLocaleLowerCase();
        let group = null;
        [...this.select.options].forEach((option, index) => {
            if (option.hidden || option.parentElement?.hidden || !option.label.toLocaleLowerCase().includes(query))
                return;
            if (option.parentElement instanceof HTMLOptGroupElement && option.parentElement !== group) {
                group = option.parentElement;
                const heading = document.createElement("div");
                heading.className = "select-group";
                heading.textContent = group.label;
                this.list.append(heading);
            }
            const row = document.createElement("div");
            row.className = "select-option";
            row.id = this.list.id + "-" + index;
            row.dataset.index = String(index);
            row.setAttribute("role", "option");
            row.setAttribute("aria-selected", String(option.selected));
            row.setAttribute("aria-disabled", String(this.disabled(option)));
            row.textContent = option.label;
            this.list.append(row);
            this.rows.push({ option, element: row, index });
        });
        this.empty.hidden = this.rows.length > 0;
        this.empty.textContent = query ? "没有匹配的选项" : "暂无可选项";
        const enabled = this.rows.filter(r => !this.disabled(r.option));
        const active = enabled.find(r => r.option === previous) || enabled.find(r => r.option.selected) || enabled[0];
        this.setActive(active?.index ?? -1);
    }
    setActive(index) {
        this.active = index;
        for (const row of this.rows)
            row.element.classList.toggle("active", row.index === index);
        for (const el of [this.trigger, this.search]) {
            if (index >= 0 && this.isOpen())
                el.setAttribute("aria-activedescendant", this.list.id + "-" + index);
            else
                el.removeAttribute("aria-activedescendant");
        }
    }
    focusActive() {
        const row = this.rows.find(r => r.index === this.active)?.element;
        if (!row)
            return;
        const rect = row.getBoundingClientRect();
        const list = this.list.getBoundingClientRect();
        // 只滚选项列表，不让 scrollIntoView 连带滚动页面或外层 dialog。
        if (rect.top < list.top)
            this.list.scrollTop += rect.top - list.top;
        else if (rect.bottom > list.bottom)
            this.list.scrollTop += rect.bottom - list.bottom;
    }
    choose(index) {
        const option = this.select.options[index];
        if (!option || this.disabled(option) || this.trigger.disabled)
            return;
        const changed = this.select.selectedIndex !== index;
        this.select.selectedIndex = index;
        this.sync();
        this.close(true);
        if (changed) {
            this.select.dispatchEvent(new Event("input", { bubbles: true }));
            this.select.dispatchEvent(new Event("change", { bubbles: true }));
        }
    }
    close(focus) {
        if (this.isOpen())
            this.panel.hidePopover();
        if (focus)
            this.trigger.focus({ preventScroll: true });
    }
    keydown(e) {
        if (e.isComposing || this.trigger.disabled)
            return;
        const open = this.isOpen();
        const searching = e.target === this.search;
        if (e.key === "Escape" && open) {
            e.preventDefault();
            e.stopPropagation(); // 第一次 Esc 只关闭下拉，不关闭外层 dialog。
            this.close(true);
        }
        else if (e.key === "Tab" && open) {
            // 搜索框在顶层面板里，将 Tab 顺序接回原表单。
            this.close(searching);
        }
        else if (["ArrowDown", "ArrowUp", "Home", "End"].includes(e.key) && !(searching && ["Home", "End"].includes(e.key))) {
            e.preventDefault();
            if (!open)
                this.panel.showPopover();
            const enabled = this.rows.filter(r => !this.disabled(r.option));
            let pos = enabled.findIndex(r => r.index === this.active);
            if (e.key === "Home")
                pos = 0;
            else if (e.key === "End")
                pos = enabled.length - 1;
            else if (open)
                pos += e.key === "ArrowDown" ? 1 : -1;
            this.setActive(enabled[Math.max(0, Math.min(pos, enabled.length - 1))]?.index ?? -1);
            this.focusActive();
        }
        else if (e.key === "Enter" || (e.key === " " && !searching)) {
            e.preventDefault();
            if (open)
                this.choose(this.active);
            else
                this.panel.showPopover();
        }
        else if (!searching && e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
            e.preventDefault();
            if (!open)
                this.panel.showPopover();
            if (!this.search.hidden) {
                this.search.value += e.key;
                this.render();
                this.search.focus();
            }
            else {
                const now = Date.now();
                this.typeahead = (now - this.typedAt > 700 ? "" : this.typeahead) + e.key.toLocaleLowerCase();
                this.typedAt = now;
                const match = this.rows.find(r => !this.disabled(r.option) && r.option.label.toLocaleLowerCase().startsWith(this.typeahead));
                if (match)
                    this.setActive(match.index);
            }
            this.focusActive();
        }
    }
    reposition = (e) => {
        if (!(e.target instanceof Node) || !this.panel.contains(e.target))
            this.position();
    };
    position() {
        if (!this.trigger.getClientRects().length) {
            this.close(false);
            return;
        }
        const rect = this.trigger.getBoundingClientRect();
        const viewport = window.visualViewport;
        const x = viewport?.offsetLeft || 0;
        const y = viewport?.offsetTop || 0;
        const width = viewport?.width || window.innerWidth;
        const height = viewport?.height || window.innerHeight;
        const gap = 6, margin = 8;
        const below = Math.max(0, y + height - rect.bottom - gap - margin);
        const above = Math.max(0, rect.top - y - gap - margin);
        const up = below < 260 && above > below;
        const available = up ? above : below;
        this.panel.style.width = `${Math.min(Math.max(rect.width, 200), width - margin * 2)}px`;
        this.panel.style.maxHeight = `${available}px`;
        this.panel.style.left = `${Math.max(x + margin, Math.min(rect.left, x + width - this.panel.offsetWidth - margin))}px`;
        this.panel.style.top = `${up ? rect.top - this.panel.offsetHeight - gap : rect.bottom + gap}px`;
        this.panel.dataset.side = up ? "top" : "bottom";
    }
}
export function enhanceSelects(root = document) {
    for (const select of root.querySelectorAll("select")) {
        if (!controls.has(select) && !select.multiple && select.size <= 1 && "showPopover" in HTMLElement.prototype) {
            controls.set(select, new SelectControl(select));
        }
    }
}
/** 同步代码赋值，不模拟用户 change（避免加载设置时触发保存或请求）。 */
export function setSelectValue(select, value) {
    select.value = value;
    controls.get(select)?.sync();
}
