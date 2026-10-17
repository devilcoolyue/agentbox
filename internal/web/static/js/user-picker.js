/* 用户多选：账号使用范围、智能决策等「指定用户」处共用，勾选代替手打用户名。
 *
 * - 列表来自 /users（仅管理员）。管理员本来就始终可用：列出但锁成已选，不写进名单；
 * - 名单里已经不存在的名字（删用户不会清这份名单）单列出来标明，保存时去掉——
 *   服务端会拒绝不存在的用户，留着只会让整次保存失败；
 * - 列表没读到（读取中、失败）时 value() 原样返回载入的名单，不会因此丢人。
 *
 * 系统设置的保存条按控件 id 比较取值，所以可以给一个隐藏 input 做镜像（field）：
 * 每次勾选把规范化后的名单写进去并派发 change。程序改写镜像要走 userPickerFor(field).set，
 * 和 setSelectValue 一个道理。 */
import { api } from "./api.js";
import { actionButton } from "./icons.js";
import { setAttrRender, setText, setTextRender, t as i18nText } from "./i18n.js";
import { skelRows } from "./skeleton.js";
const pickers = new WeakMap();
export const userPickerFor = (field) => pickers.get(field);
/** 镜像字段的取值：去重排序后逐行，勾选顺序不同但名单相同时不算改动 */
export const userListValue = (names) => [...new Set(names)].sort().join("\n");
const byName = (a, b) => a.localeCompare(b);
export function mountUserPicker(root, opts = {}) {
    root.classList.add("user-picker");
    root.replaceChildren();
    let stored = [];
    const selected = new Set();
    let users = null;
    let loading = null;
    let stale = true;
    let disabled = false;
    let failure = "";
    const bar = document.createElement("div");
    bar.className = "up-bar";
    const search = document.createElement("input");
    search.type = "search";
    search.className = "up-search";
    search.autocomplete = "off";
    search.spellcheck = false;
    setAttrRender(search, "placeholder", () => i18nText("搜索用户"));
    setAttrRender(search, "aria-label", () => i18nText("搜索用户"));
    const allLabel = document.createElement("label");
    allLabel.className = "check up-all";
    const all = document.createElement("input");
    all.type = "checkbox";
    const allText = document.createElement("span");
    setText(allText, "全选");
    allLabel.append(all, allText);
    const count = document.createElement("span");
    count.className = "up-count";
    count.setAttribute("aria-live", "polite");
    bar.append(search, allLabel, count);
    const list = document.createElement("div");
    list.className = "up-list";
    list.setAttribute("role", "group");
    setAttrRender(list, "aria-label", () => i18nText("选择用户"));
    const note = document.createElement("p");
    note.className = "up-note";
    const error = document.createElement("p");
    error.className = "up-error login-error";
    error.setAttribute("role", "alert");
    root.append(bar, list, note, error);
    const selectable = (u) => opts.includeAdmins || u.role !== "admin";
    const known = () => new Set((users || []).filter(selectable).map(u => u.name));
    const value = () => {
        if (!users)
            return [...selected].sort(byName);
        const names = known();
        return [...selected].filter(n => names.has(n)).sort(byName);
    };
    const boxes = () => [...list.querySelectorAll("input[data-user]")];
    const visible = () => boxes().filter(box => !box.closest(".up-row").hidden);
    const paintCount = () => {
        const n = value().length;
        setTextRender(count, () => i18nText("已选 {n} 人", { n: String(n) }));
        const shown = visible(), on = shown.filter(box => box.checked).length;
        all.checked = !!shown.length && on === shown.length;
        all.indeterminate = on > 0 && on < shown.length;
        all.disabled = disabled || !shown.length;
    };
    const commit = () => {
        error.textContent = "";
        paintCount();
        if (opts.field) {
            opts.field.value = userListValue(value());
            opts.field.dispatchEvent(new Event("change", { bubbles: true }));
        }
    };
    const applyFilter = () => {
        const q = search.value.normalize("NFKC").trim().toLowerCase();
        let matched = 0;
        for (const row of list.querySelectorAll(".up-row[data-name]")) {
            row.hidden = !!q && !row.dataset.name.toLowerCase().includes(q);
            if (!row.hidden)
                matched++;
        }
        const empty = list.querySelector(".up-empty-match");
        if (empty)
            empty.hidden = !q || matched > 0;
        paintCount();
    };
    const row = (name, kind) => {
        const label = document.createElement("label");
        label.className = "check up-row" + (kind === "user" ? "" : " up-" + kind);
        label.dataset.name = name;
        const box = document.createElement("input");
        box.type = "checkbox";
        const text = document.createElement("span");
        text.className = "up-name";
        text.textContent = name;
        label.append(box, text);
        if (kind === "user") {
            box.dataset.user = name;
            box.checked = selected.has(name);
            box.disabled = disabled;
        }
        else {
            // 管理员锁成已选；不存在的名字锁成未选，保存时自然去掉
            box.checked = kind === "admin";
            box.disabled = true;
            const tag = document.createElement("span");
            tag.className = "up-tag";
            setText(tag, kind === "admin" ? "管理员 · 始终可用" : "用户不存在，保存时移除");
            label.append(tag);
        }
        return label;
    };
    const render = () => {
        if (!users) {
            list.replaceChildren();
            bar.hidden = true;
            note.hidden = true;
            if (failure) {
                const msg = document.createElement("p");
                msg.className = "up-status";
                setTextRender(msg, () => i18nText("读取用户列表失败：") + failure);
                const retry = actionButton(document.createElement("button"), () => i18nText("重试"), "refresh");
                retry.type = "button";
                retry.className = "btn btn-sm";
                retry.disabled = disabled;
                retry.addEventListener("click", () => { stale = true; ensure(); });
                list.append(msg, retry);
            }
            else
                list.append(skelRows(3, i18nText("读取用户中…"), { rowClass: "up-row", lines: 0, title: [18, 20] }));
            return;
        }
        const regular = users.filter(selectable).map(u => u.name).sort(byName);
        const admins = users.filter(u => !selectable(u)).map(u => u.name).sort(byName);
        const names = new Set(users.map(u => u.name));
        const missing = stored.filter(n => !names.has(n)).sort(byName);
        const rows = [...regular.map(n => row(n, "user")), ...admins.map(n => row(n, "admin")), ...missing.map(n => row(n, "missing"))];
        const noMatch = document.createElement("p");
        noMatch.className = "up-status up-empty-match";
        setText(noMatch, "没有匹配的用户");
        list.replaceChildren(...rows, noMatch);
        // 人少时不放搜索框：一眼就能看全，窄屏上也省一行
        bar.hidden = !regular.length;
        search.hidden = rows.length <= 6;
        if (search.hidden)
            search.value = "";
        note.hidden = regular.length > 0;
        setText(note, "暂无普通用户，可在「安全与访问 → 用户管理」中添加。");
        applyFilter();
    };
    const ensure = () => {
        if (!stale || loading || opts.signal?.aborted)
            return;
        stale = false;
        failure = "";
        const ctrl = loading = new AbortController();
        const abort = () => ctrl.abort();
        opts.signal?.addEventListener("abort", abort, { once: true });
        render();
        void api("/users", { signal: ctrl.signal }).then(list => {
            if (ctrl.signal.aborted)
                return;
            users = list;
        }).catch(e => {
            if (ctrl.signal.aborted)
                return;
            stale = true;
            failure = e.message;
        }).finally(() => {
            opts.signal?.removeEventListener("abort", abort);
            if (loading !== ctrl)
                return;
            loading = null;
            if (!ctrl.signal.aborted)
                render();
        });
    };
    list.addEventListener("change", e => {
        const box = e.target;
        if (!box.dataset.user)
            return;
        if (box.checked)
            selected.add(box.dataset.user);
        else
            selected.delete(box.dataset.user);
        commit();
    });
    all.addEventListener("change", () => {
        for (const box of visible()) {
            box.checked = all.checked;
            if (all.checked)
                selected.add(box.dataset.user);
            else
                selected.delete(box.dataset.user);
        }
        commit();
    });
    search.addEventListener("input", applyFilter);
    // 回车在表单里会提交整个弹窗；搜索框里只是筛选
    search.addEventListener("keydown", e => { if (e.key === "Enter")
        e.preventDefault(); });
    const picker = {
        set(names) {
            stored = [...new Set(names)];
            selected.clear();
            for (const n of stored)
                selected.add(n);
            error.textContent = "";
            if (opts.field)
                opts.field.value = userListValue(stored);
            if (users)
                render();
            else
                paintCount();
        },
        value,
        setDisabled(on) {
            disabled = on;
            search.disabled = on;
            for (const box of boxes())
                box.disabled = on;
            for (const b of list.querySelectorAll("button"))
                b.disabled = on;
            paintCount();
        },
        ensure,
        invalidate() {
            stale = true;
            loading?.abort();
            loading = null;
        },
        setError(message) {
            error.textContent = message;
        },
        focus() {
            (visible()[0] || search).focus();
            root.scrollIntoView({ block: "nearest" });
        },
    };
    if (opts.field)
        pickers.set(opts.field, picker);
    opts.signal?.addEventListener("abort", () => {
        loading?.abort();
        if (opts.field)
            pickers.delete(opts.field);
    }, { once: true });
    paintCount();
    return picker;
}
