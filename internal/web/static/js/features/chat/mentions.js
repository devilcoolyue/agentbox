import { setTextRender, t as i18nText } from "../../i18n.js";
import { fileIconName, svgIcon } from "../../icons.js";
const DEBOUNCE_MS = 60, FRESH_MS = 10_000, ERROR_MS = 3_000, KEEP = 50;
/** 光标所在的 @ 片段：@ 在开头或空白之后、到光标为止没有空白；@"…" 里可以有空格 */
export function mentionToken(value, caret) {
    const m = /(?:^|\s)@("[^"\n]*|[^\s"]*)$/.exec(value.slice(0, caret));
    if (!m)
        return null;
    const quoted = m[1].startsWith('"');
    // 光标落在已有路径中间时，替换到这个词的末尾
    const tail = (quoted ? /^[^"\n]*"?/ : /^\S*/).exec(value.slice(caret))[0];
    return { start: caret - m[1].length - 1, end: caret + tail.length, query: quoted ? m[1].slice(1) : m[1] };
}
/** 填进输入框的写法；文件夹不收尾，留在 @ 里接着往下找 */
export function mentionText(item, agent) {
    const path = item.dir ? item.path + "/" : item.path;
    const quote = /\s/.test(path);
    if (item.dir)
        return "@" + (quote ? '"' : "") + path;
    const spelled = quote ? '"' + path + '"' : path;
    return agent === "codex" ? spelled : "@" + spelled;
}
export function createMentionMenu(hooks) {
    const results = new Map();
    let pending = null;
    let items = [];
    let active = 0;
    let shownQuery = "";
    let open = false;
    let dismissed = ""; // Esc 收起时的输入；内容变了才重新弹出
    let acceptLater = false; // 结果还没回来就按了 Tab/Enter：回来后填入第一项
    let timer = 0, blurTimer = 0;
    function token() {
        const input = hooks.input();
        if (input.selectionStart !== input.selectionEnd)
            return null;
        return mentionToken(input.value, input.selectionStart ?? input.value.length);
    }
    function cached(key) {
        const view = results.get(key);
        return view && Date.now() - view.at < (view.error ? ERROR_MS : FRESH_MS) ? view : undefined;
    }
    function remember(key, view) {
        results.delete(key);
        results.set(key, view);
        if (results.size > KEEP)
            results.delete(results.keys().next().value);
    }
    function search(session, query, key) {
        if (pending?.key === key)
            return;
        pending?.ctl.abort();
        clearTimeout(timer);
        const ctl = new AbortController();
        pending = { key, ctl };
        timer = window.setTimeout(() => {
            hooks.search(session, query, ctl.signal).then(view => {
                remember(key, { files: Array.isArray(view.files) ? view.files : [], truncated: !!view.truncated, error: false, at: Date.now() });
            }).catch(() => {
                if (!ctl.signal.aborted)
                    remember(key, { files: [], truncated: false, error: true, at: Date.now() });
            }).finally(() => {
                if (pending?.ctl !== ctl)
                    return;
                pending = null;
                update();
            });
        }, DEBOUNCE_MS);
    }
    function cancel() {
        acceptLater = false;
        clearTimeout(timer);
        pending?.ctl.abort();
        pending = null;
    }
    function update() {
        const input = hooks.input(), session = hooks.session(), t = token();
        if (!t || !session || input.disabled || document.activeElement !== input || input.value === dismissed) {
            cancel();
            close();
            return;
        }
        const key = session.id + "\n" + t.query, view = cached(key);
        if (!view) {
            search(session.id, t.query, key);
            // 打开着就先留着上一次的结果，免得每敲一个字闪一下
            if (!open) {
                items = [];
                render("", () => i18nText("正在搜索文件…"));
            }
            return;
        }
        const accept = acceptLater;
        cancel();
        const before = open && t.query === shownQuery ? items[active]?.path : undefined;
        items = view.files;
        const keep = items.findIndex(i => i.path === before);
        active = keep >= 0 ? keep : 0;
        shownQuery = t.query;
        render(t.query, view.error ? () => i18nText("暂时读不到工作区文件")
            : !items.length ? () => i18nText("没有匹配的文件")
                : view.truncated ? () => i18nText("工作区文件很多，只搜索了靠上的一部分") : null);
        if (accept && items.length)
            complete();
    }
    /** 路径里与输入对上的字：先找连续的一段，找不到再按顺序逐字标出；列文件夹内容时不标 */
    function marks(path, query) {
        const lower = path.toLowerCase(), q = query.toLowerCase().replace(/^\.\//, "");
        const hit = new Array(path.length).fill(false);
        if (!q || q.endsWith("/"))
            return hit;
        const base = lower.lastIndexOf("/") + 1;
        let at = lower.indexOf(q, base);
        if (at < 0)
            at = lower.indexOf(q);
        if (at >= 0) {
            hit.fill(true, at, at + q.length);
            return hit;
        }
        for (let i = 0, j = 0; i < lower.length && j < q.length; i++)
            if (lower[i] === q[j]) {
                hit[i] = true;
                j++;
            }
        return hit;
    }
    /** 「文件名 + 淡色目录」两段（同 VS Code 快速打开），对上的字加粗 */
    function pathLabel(item, query) {
        const path = item.path, base = path.lastIndexOf("/") + 1, hit = marks(path, query);
        const part = (cls, from, to, suffix = "") => {
            const box = document.createElement("span");
            box.className = cls + " mono";
            for (let i = from; i < to;) {
                let j = i + 1;
                while (j < to && hit[j] === hit[i])
                    j++;
                const run = document.createElement(hit[i] ? "b" : "span");
                run.textContent = path.slice(i, j);
                box.append(run);
                i = j;
            }
            if (suffix)
                box.append(suffix);
            return box;
        };
        const name = part("mention-name", base, path.length, item.dir ? "/" : "");
        // 目录不带末尾斜杠；顶层的没有目录
        return base > 1 ? [name, part("mention-dir", 0, base - 1)] : [name];
    }
    function render(query, note) {
        const menu = hooks.menu();
        menu.replaceChildren();
        items.forEach((item, i) => {
            const row = document.createElement("div");
            row.className = "cmd-row mention-row";
            row.id = "mention-opt-" + i;
            row.setAttribute("role", "option");
            row.setAttribute("aria-selected", String(i === active));
            const icon = svgIcon(item.dir ? "file.folder" : fileIconName(item.path.slice(item.path.lastIndexOf("/") + 1)), 14);
            icon.classList.add("mention-icon");
            row.append(icon, ...pathLabel(item, query));
            // 鼠标按下不抢输入框焦点；触屏不拦，否则 Safari 滚不动列表
            row.addEventListener("pointerdown", e => { if (e.pointerType === "mouse")
                e.preventDefault(); });
            row.addEventListener("mousemove", () => { if (active !== i) {
                active = i;
                mark();
            } });
            row.addEventListener("click", () => { active = i; complete(); });
            menu.append(row);
        });
        if (note) {
            const tip = document.createElement("div");
            tip.className = "cmd-note";
            setTextRender(tip, note);
            menu.append(tip);
        }
        menu.classList.remove("hidden");
        open = true;
        mark();
    }
    function mark() {
        const menu = hooks.menu(), input = hooks.input();
        menu.querySelectorAll(".mention-row").forEach((row, i) => {
            row.setAttribute("aria-selected", String(i === active));
            if (i === active)
                row.scrollIntoView({ block: "nearest" });
        });
        if (items[active])
            input.setAttribute("aria-activedescendant", "mention-opt-" + active);
        else
            input.removeAttribute("aria-activedescendant");
    }
    function close() {
        if (!open)
            return;
        open = false;
        hooks.menu().classList.add("hidden");
        hooks.menu().replaceChildren();
        hooks.input().removeAttribute("aria-activedescendant");
    }
    /** 替换整个 @ 片段；文件后补一个空格，文件夹留在原处接着搜 */
    function complete() {
        const item = items[active], t = token(), input = hooks.input(), session = hooks.session();
        if (!item || !t || !session)
            return;
        const after = input.value.slice(t.end);
        const text = mentionText(item, session.agent) + (item.dir || /^\s/.test(after) ? "" : " ");
        input.value = input.value.slice(0, t.start) + text + after;
        const caret = t.start + text.length + (item.dir || !/^\s/.test(after) ? 0 : 1);
        input.setSelectionRange(caret, caret);
        dismissed = "";
        input.focus();
        hooks.changed();
        if (item.dir)
            update();
        else
            close();
    }
    return {
        update,
        close,
        /** 列表打开时接管方向键、Tab、Enter、Esc；接管了返回 true。Enter 只填入，不发送 */
        keydown(e) {
            if (!open || e.isComposing || e.keyCode === 229 || e.altKey || e.ctrlKey || e.metaKey)
                return false;
            switch (e.key) {
                case "ArrowDown":
                case "ArrowUp":
                    if (!items.length)
                        return false;
                    active = (active + (e.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
                    mark();
                    break;
                case "Tab":
                case "Enter":
                    if (e.shiftKey)
                        return false;
                    // 列表还是上一个字的结果时不能填它，等这次的结果
                    if (pending)
                        acceptLater = true;
                    else if (items.length)
                        complete();
                    else
                        return false;
                    break;
                case "Escape":
                    dismissed = hooks.input().value;
                    cancel();
                    close();
                    break;
                default:
                    return false;
            }
            e.preventDefault();
            e.stopPropagation();
            return true;
        },
        /** 失焦稍等再收：点列表项时输入框先失焦 */
        blur() {
            clearTimeout(blurTimer);
            blurTimer = window.setTimeout(() => { if (document.activeElement !== hooks.input()) {
                cancel();
                close();
            } }, 150);
        },
        reset() {
            cancel();
            results.clear();
            dismissed = "";
            clearTimeout(blurTimer);
            close();
        },
    };
}
