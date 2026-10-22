import { setText, setTextRender, setAttrRender, t as i18nText } from "../../i18n.js";
import { svgIcon } from "../../icons.js";
const REFRESH_MS = 250;
const TOOLS_KEY = "agentbox_chat_task_tools";
/* Claude Code 2.1.296 起只对它列出的旧模型（Opus/Sonnet 4.x 等）默认开任务工具，Claude 5
 * 要另加 CLAUDE_CODE_ENABLE_TODO_TOOLS。开了每次请求多带几个工具定义、模型也会多几次调用
 * 去建、改任务，多花 token，所以是此浏览器的偏好，默认关，从下一条消息起生效。 */
export function taskToolsEnabled() { try {
    return localStorage.getItem(TOOLS_KEY) === "on";
}
catch {
    return false;
} }
export function setTaskToolsEnabled(on) {
    try {
        if (on)
            localStorage.setItem(TOOLS_KEY, "on");
        else
            localStorage.removeItem(TOOLS_KEY);
    }
    catch { /* 存储不可用时只影响本页 */ }
}
/** TodoWrite 的输入 → 列表 */
function todoWriteItems(input) {
    const todos = input?.todos;
    if (!Array.isArray(todos))
        return undefined;
    return todos.flatMap((t, i) => {
        const o = t;
        return typeof o?.content === "string" && o.content
            ? [{ id: String(i + 1), subject: o.content, activeForm: typeof o.activeForm === "string" ? o.activeForm : undefined, status: String(o.status || "pending") }]
            : [];
    });
}
/** Codex 的 todo_list 条目（exec 只有 completed，app-server 转出来的另带 status） */
function codexPlanItems(item) {
    const items = item?.items;
    if (!Array.isArray(items))
        return undefined;
    return items.flatMap((t, i) => {
        const o = t;
        if (typeof o?.text !== "string" || !o.text)
            return [];
        const status = typeof o.status === "string" && o.status ? o.status : o.completed === true ? "completed" : "pending";
        return [{ id: String(i + 1), subject: o.text, status }];
    });
}
/** 事件里改动了 Claude 任务文件的工具调用 */
function touchesTaskFiles(ev) {
    if (ev.type !== "assistant" || !Array.isArray(ev.message?.content))
        return false;
    return ev.message.content.some(b => b.type === "tool_use" && (b.name === "TaskCreate" || b.name === "TaskUpdate"));
}
export function createTaskPanel(hooks) {
    let files = []; // Claude 任务文件（服务端读）
    let todos = []; // TodoWrite / Codex 计划
    let truncated = false;
    let thread = "";
    let open = null; // 用户点过就听用户的，直到运行状态变化
    let wasRunning = false;
    let ctl = null;
    let timer = 0, queued = false;
    let lastActive = "";
    const items = () => files.length ? files : todos;
    const finished = (list) => list.length > 0 && list.every(t => t.status === "completed");
    function observe(ev, live = true) {
        if (!ev || typeof ev !== "object" || ev.parent_tool_use_id)
            return;
        if (ev.type === "assistant" && Array.isArray(ev.message?.content)) {
            for (const b of ev.message.content) {
                if (b.type !== "tool_use" || b.name !== "TodoWrite")
                    continue;
                const next = todoWriteItems(b.input);
                if (next)
                    todos = next;
            }
            if (live && touchesTaskFiles(ev))
                schedule();
        }
        else if ((ev.type === "item.started" || ev.type === "item.updated" || ev.type === "item.completed") && ev.item?.type === "todo_list") {
            const next = codexPlanItems(ev.item);
            if (next)
                todos = next;
        }
        else if (ev.type === "result") {
            if (live)
                schedule(); // 回合收尾时的最终状态
            return;
        }
        else
            return;
        render();
    }
    function schedule() {
        if (hooks.agent() !== "claude" || !thread)
            return;
        clearTimeout(timer);
        timer = window.setTimeout(refresh, REFRESH_MS);
    }
    async function refresh() {
        const session = hooks.session(), tid = thread;
        if (hooks.agent() !== "claude" || !session || !tid)
            return;
        if (ctl) {
            queued = true;
            return;
        } // 在途的读完再读一次
        const mine = ctl = new AbortController();
        try {
            const view = await hooks.load(session, tid, mine.signal);
            if (mine.signal.aborted || tid !== thread)
                return;
            files = Array.isArray(view.tasks) ? view.tasks.map(t => ({ id: t.id, subject: t.subject, activeForm: t.active_form, status: t.status, blockedBy: t.blocked_by })) : [];
            truncated = !!view.truncated;
            render();
        }
        catch {
            // 读不到就保持上一次的列表，下一次工具调用或回合结束再读
        }
        finally {
            if (ctl === mine)
                ctl = null;
            if (queued && !mine.signal.aborted) {
                queued = false;
                void refresh();
            }
        }
    }
    function render() {
        const el = hooks.el(), list = items(), running = hooks.running();
        if (running !== wasRunning) {
            wasRunning = running;
            open = null;
        }
        const done = list.filter(t => t.status === "completed").length;
        const show = list.length > 0 && (running || done < list.length);
        el.classList.toggle("hidden", !show);
        const active = show ? activeLabel() : "";
        if (active !== lastActive) {
            lastActive = active;
            hooks.changed();
        }
        if (!show)
            return;
        const expanded = open ?? running;
        const toggle = el.querySelector(".tasks-toggle");
        toggle.setAttribute("aria-expanded", String(expanded));
        el.dataset.tone = running && done < list.length ? "busy" : done === list.length ? "done" : "idle";
        setTextRender(el.querySelector(".tasks-count"), () => i18nText("任务 {p0}/{p1}", { p0: String(done), p1: String(list.length) }));
        const current = list.find(t => t.status === "in_progress"), next = list.find(t => t.status !== "completed");
        setTextRender(el.querySelector(".tasks-current"), () => current ? current.activeForm || current.subject
            : !next ? i18nText("全部完成") : i18nText("下一项：{p0}", { p0: next.subject }));
        const ol = el.querySelector(".tasks-list");
        ol.classList.toggle("hidden", !expanded);
        if (!expanded)
            return;
        ol.replaceChildren(...list.map(task => {
            const li = document.createElement("li");
            li.className = "task-item";
            li.dataset.status = task.status;
            const mark = document.createElement("span");
            mark.className = "task-mark";
            if (task.status === "completed")
                mark.append(svgIcon("chat.task-done", 12));
            setAttrRender(mark, "aria-label", () => task.status === "completed" ? i18nText("已完成") : task.status === "in_progress" ? i18nText("进行中") : i18nText("待办"));
            mark.setAttribute("role", "img");
            const text = document.createElement("span");
            text.className = "task-text";
            text.textContent = task.subject; // 模型写的内容，只当文本
            li.append(mark, text);
            const blocked = task.status !== "completed" ? (task.blockedBy || []).filter(Boolean) : [];
            if (blocked.length) {
                const wait = document.createElement("span");
                wait.className = "task-wait";
                setTextRender(wait, () => i18nText("等待 {p0}", { p0: blocked.map(id => "#" + id).join(" ") }));
                li.append(wait);
            }
            return li;
        }));
        if (truncated) {
            const more = document.createElement("li");
            more.className = "task-more";
            setText(more, "任务太多，只显示前 200 项");
            ol.append(more);
        }
    }
    /** 执行指示用的文案：进行中那一项的进行时描述 */
    function activeLabel() {
        if (!hooks.running())
            return "";
        const current = items().find(t => t.status === "in_progress");
        return current ? current.activeForm || current.subject : "";
    }
    return {
        observe,
        activeLabel,
        /** 按历史记录还原（最新一页），并读一次服务端的任务文件 */
        replay(entries, threadID) {
            thread = threadID || "";
            for (const e of entries) {
                // Codex 的计划只管本回合，Claude 做完的列表也到此为止：新一轮开始就清掉
                if (e.kind === "user" && (hooks.agent() === "codex" || finished(todos)))
                    todos = [];
                else if (e.kind === "event")
                    observe(e.event, false);
            }
            render();
            void refresh();
        },
        /** 新一轮开始（实时）：Codex 的上一份计划、做完了的列表作废 */
        turnStarted() {
            if (hooks.agent() === "codex" || finished(todos))
                todos = [];
            if (finished(files)) {
                // 在途的读取可能是新回合之前的样子；作废它，按新回合重读
                ctl?.abort();
                ctl = null;
                queued = false;
                files = [];
                truncated = false;
                schedule();
            }
            render();
        },
        toggle() {
            const running = hooks.running();
            open = !(open ?? running);
            render();
        },
        render,
        reset() {
            clearTimeout(timer);
            ctl?.abort();
            ctl = null;
            queued = false;
            files = [];
            todos = [];
            truncated = false;
            thread = "";
            open = null;
            render();
        },
    };
}
