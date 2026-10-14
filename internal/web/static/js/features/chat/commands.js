import { setText, setTextRender, t as i18nText } from "../../i18n.js";
// init 事件之前的兜底：终端专属命令与内部命令
const TERMINAL_ONLY = ["doctor", "color", "focus", "reload-plugins", "heapdump", "workflow-launch-exec"];
const FRESH_MS = 60_000, RETRY_MS = 10_000, MAX_ROWS = 60;
// 常用内置命令的说明跟随界面语言；其余用 CLI 自己的英文说明
const BUILTIN_TEXT = {
    compact: () => i18nText("压缩上下文：把目前的对话总结成摘要以释放空间（会请求一次模型）"),
    context: () => i18nText("查看上下文用量明细"),
    init: () => i18nText("生成 CLAUDE.md，记录项目结构与约定"),
    usage: () => i18nText("查看本会话花费与套餐用量"),
    "security-review": () => i18nText("审查当前分支改动里的安全问题"),
};
/** 文本开头的斜杠命令名（与 CLI 同规则），不是命令返回 "" */
export function commandName(text) {
    return /^\/([a-zA-Z0-9_][a-zA-Z0-9:_-]*)(?:\s|$)/.exec(text.trim())?.[1] || "";
}
export function createCommandMenu(hooks) {
    let cache = null;
    let loading = null;
    const terminalOnly = new Set(TERMINAL_ONLY);
    let items = [];
    let active = 0;
    let shownQuery = "";
    let open = false;
    let dismissed = ""; // Esc 收起时的输入；内容变了才重新弹出
    let blurTimer = 0;
    /** 光标所在的命令名片段：输入以「/」开头，光标在第一个空白之前 */
    function token() {
        const input = hooks.input(), value = input.value;
        if (!value.startsWith("/") || input.selectionStart !== input.selectionEnd)
            return null;
        const space = value.search(/\s/), stop = space < 0 ? value.length : space;
        const caret = input.selectionStart ?? value.length;
        return caret >= 1 && caret <= stop ? { query: value.slice(1, stop), stop } : null;
    }
    function load(session) {
        if (session.agent !== "claude")
            return;
        if (cache?.session === session.id && Date.now() - cache.at < cache.ttl)
            return;
        if (loading?.session === session.id)
            return;
        loading?.ctl.abort();
        const ctl = new AbortController(), id = session.id;
        loading = { session: id, ctl };
        const keep = cache?.session === id ? cache.commands : [];
        hooks.load(id, ctl.signal).then(view => {
            if (loading?.ctl !== ctl)
                return;
            const commands = Array.isArray(view.commands) ? view.commands : [];
            // 容器没起来时很快会起来；读不到时沿用上一次
            cache = { session: id, at: Date.now(), ttl: view.state === "available" ? FRESH_MS : RETRY_MS, state: view.state,
                commands: view.state === "available" ? commands : keep };
        }).catch(() => {
            if (loading?.ctl === ctl)
                cache = { session: id, at: Date.now(), ttl: RETRY_MS, state: "error", commands: keep };
        }).finally(() => {
            if (loading?.ctl !== ctl)
                return;
            loading = null;
            if (open)
                update();
        });
    }
    function all(agent) {
        const out = hooks.web.map(c => ({ name: c.name, aliases: c.aliases || [], hint: "", describe: c.describe, text: "", builtin: true, web: true }));
        const taken = new Set(out.flatMap(c => [c.name, ...c.aliases]));
        if (agent !== "claude" || !cache || cache.session !== hooks.session()?.id)
            return out;
        for (const c of cache.commands) {
            if (!c?.name || taken.has(c.name) || terminalOnly.has(c.name) || c.name.startsWith("__"))
                continue;
            const own = c.builtin ? BUILTIN_TEXT[c.name] : undefined, text = c.description || "";
            out.push({ name: c.name, aliases: (c.aliases || []).filter(a => !taken.has(a)), hint: c.argument_hint || "",
                describe: own || (() => text), text, builtin: !!c.builtin, web: false });
        }
        return out;
    }
    /** CLI 的排序：名字完全相同 > 名字开头 > 名字包含 > 说明包含 */
    function match(query, agent) {
        const q = query.toLowerCase();
        const scored = [];
        all(agent).forEach((item, at) => {
            const names = [item.name, ...item.aliases].map(n => n.toLowerCase());
            const rank = !q ? 1 : names.includes(q) ? 0 : names.some(n => n.startsWith(q)) ? 1
                : names.some(n => n.includes(q)) ? 2 : (item.describe() + "\n" + item.text).toLowerCase().includes(q) ? 3 : -1;
            if (rank >= 0)
                scored.push({ item, rank, at });
        });
        return scored.sort((a, b) => a.rank - b.rank || a.at - b.at).slice(0, MAX_ROWS).map(s => s.item);
    }
    function note(session) {
        if (session.agent !== "claude")
            return null;
        if (loading && (!cache || cache.session !== loading.session || !cache.commands.length))
            return () => i18nText("正在读取 Claude Code 的命令…");
        if (cache?.state === "stopped" && !cache.commands.length)
            return () => i18nText("工作空间启动后显示 Claude Code 的全部命令");
        if (cache && cache.state !== "available" && !cache.commands.length)
            return () => i18nText("暂时读不到 Claude Code 的命令，可以直接输入");
        return null;
    }
    function update() {
        const input = hooks.input(), session = hooks.session(), t = token();
        if (!t || !session || input.disabled || document.activeElement !== input || input.value === dismissed) {
            close();
            return;
        }
        load(session);
        // 列表刷新（读完命令）时保留选中项；改了输入就回到最匹配的第一项
        const before = open && t.query === shownQuery ? items[active]?.name : undefined;
        items = match(t.query, session.agent);
        const hint = note(session);
        if (!items.length && !hint) {
            close();
            return;
        }
        const keep = items.findIndex(i => i.name === before);
        active = keep >= 0 ? keep : 0;
        shownQuery = t.query;
        render(t.query, hint);
    }
    function highlight(name, query) {
        const box = document.createElement("span");
        box.className = "cmd-name mono";
        const at = query ? name.toLowerCase().indexOf(query.toLowerCase()) : -1;
        if (at < 0) {
            box.textContent = "/" + name;
            return box;
        }
        const strong = document.createElement("b");
        strong.textContent = name.slice(at, at + query.length);
        box.append("/" + name.slice(0, at), strong, name.slice(at + query.length));
        return box;
    }
    function render(query, hint) {
        const menu = hooks.menu();
        menu.replaceChildren();
        items.forEach((item, i) => {
            const row = document.createElement("div");
            row.className = "cmd-row";
            row.id = "cmd-opt-" + i;
            row.setAttribute("role", "option");
            row.setAttribute("aria-selected", String(i === active));
            const head = document.createElement("span");
            head.className = "cmd-head";
            head.append(highlight(item.name, query));
            if (item.hint)
                head.append(Object.assign(document.createElement("span"), { className: "cmd-hint mono", textContent: item.hint }));
            if (item.web) {
                const tag = document.createElement("span");
                tag.className = "cmd-tag";
                setText(tag, "网页");
                head.append(tag);
            }
            const desc = document.createElement("span");
            desc.className = "cmd-desc";
            setTextRender(desc, item.describe);
            row.append(head, desc);
            // 鼠标按下不抢输入框焦点；触屏不拦，否则 Safari 滚不动列表
            row.addEventListener("pointerdown", e => { if (e.pointerType === "mouse")
                e.preventDefault(); });
            row.addEventListener("mousemove", () => { if (active !== i) {
                active = i;
                mark();
            } });
            row.addEventListener("click", () => { active = i; complete(false); });
            menu.append(row);
        });
        if (hint) {
            const tip = document.createElement("div");
            tip.className = "cmd-note";
            setTextRender(tip, hint);
            menu.append(tip);
        }
        menu.classList.remove("hidden");
        open = true;
        mark();
    }
    function mark() {
        const menu = hooks.menu(), input = hooks.input();
        menu.querySelectorAll(".cmd-row").forEach((row, i) => {
            row.setAttribute("aria-selected", String(i === active));
            if (i === active)
                row.scrollIntoView({ block: "nearest" });
        });
        if (items[active])
            input.setAttribute("aria-activedescendant", "cmd-opt-" + active);
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
    /** 填入选中的命令；execute 时接着发送（网页命令在发送入口里接管） */
    function complete(execute) {
        const item = items[active], t = token(), input = hooks.input();
        if (!item || !t)
            return;
        const rest = input.value.slice(t.stop);
        const space = !rest && (item.hint || !execute) ? " " : "";
        input.value = "/" + item.name + space + rest;
        const caret = item.name.length + 1 + space.length;
        input.setSelectionRange(caret, caret);
        close();
        dismissed = "";
        input.focus();
        hooks.changed();
        if (execute)
            hooks.send();
    }
    return {
        update,
        close,
        /** 第一次聚焦输入框时先读一次，等到敲「/」时多半已经读好 */
        prefetch() {
            const session = hooks.session();
            if (session && cache?.session !== session.id)
                load(session);
        },
        /** 菜单打开时接管方向键、Tab、Enter、Esc；接管了返回 true */
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
                    if (e.shiftKey || !items.length)
                        return false;
                    complete(false);
                    break;
                case "Enter":
                    if (e.shiftKey || !items.length)
                        return false;
                    complete(hooks.enterSends());
                    break;
                case "Escape":
                    dismissed = hooks.input().value;
                    close();
                    break;
                default:
                    return false;
            }
            e.preventDefault();
            e.stopPropagation();
            return true;
        },
        /** 失焦稍等再收：点菜单项时输入框先失焦 */
        blur() {
            clearTimeout(blurTimer);
            blurTimer = window.setTimeout(() => { if (document.activeElement !== hooks.input())
                close(); }, 150);
        },
        /** init 事件里 CLI 自己列出的终端专属命令 */
        observe(ev) {
            if (ev?.type !== "system" || ev.subtype !== "init" || !Array.isArray(ev.terminal_slash_commands))
                return;
            for (const name of ev.terminal_slash_commands)
                if (typeof name === "string")
                    terminalOnly.add(name);
        },
        /** 输入的是网页接管的命令（含别名）时执行它并返回 true */
        runWeb(text) {
            const name = commandName(text);
            const command = name && hooks.web.find(c => c.name === name || c.aliases?.includes(name));
            if (!command)
                return false;
            close();
            command.run();
            return true;
        },
        reset() {
            loading?.ctl.abort();
            loading = null;
            dismissed = "";
            clearTimeout(blurTimer);
            close();
        },
    };
}
