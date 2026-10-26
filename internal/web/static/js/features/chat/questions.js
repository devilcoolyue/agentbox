/* 对话里 Agent 的提问（Claude Code 的 AskUserQuestion）：回合停下等用户选，
 * 选完同一回合接着跑。服务端先落一条 pending（带问题与选项），结束时再落一条
 * 结果（answered / skipped / expired / cancelled），按 ID 合并成一张卡片。
 * 只有最新回合仍在运行时 pending 卡片可作答；之后来了用户消息就永久封存。
 * 回答只认第一份：其他页面先答了，这里提交会得到 question_closed。 */
import { api } from "../../api.js";
import { buttonLabel, svgIcon } from "../../icons.js";
import { trackInk } from "../../ink.js";
import { setAttr, setText, setTextRender, t } from "../../i18n.js";
import { APIError } from "../../problems.js";
import { S } from "../../state.js";
import { toast } from "../../toast.js";
const cards = new Map();
const FINAL = new Set(["answered", "skipped", "expired", "cancelled"]);
let changed = () => { };
let ticker = 0;
export function initQuestions(hooks) { changed = hooks.changed; }
const answerable = (c) => !FINAL.has(c.view.state) && !c.sealed && S.chatState === "running";
/** 有卡片正等着用户作答：执行指示与输入框占位据此提示。 */
export function questionsWaiting() {
    for (const c of cards.values())
        if (answerable(c))
            return true;
    return false;
}
/** 已组好的回答：多选按 Claude Code 自己的格式用 ", " 连接，「其他」写在最后。 */
function answerOf(item, p) {
    const parts = (item.options || []).map(o => o.label).filter(label => p.picked.has(label));
    if (p.otherOn && p.other.trim())
        parts.push(p.other.trim());
    return parts.join(", ");
}
function remaining(view) {
    const left = view.expires_at ? Date.parse(view.expires_at) - Date.now() : NaN;
    if (!Number.isFinite(left))
        return "";
    const minutes = Math.ceil(left / 60000);
    return minutes <= 1 ? t("不到 1 分钟后自动跳过") : t("约 {n} 分钟后自动跳过", { n: String(minutes) });
}
function stateText(c) {
    switch (c.view.state) {
        case "answered": return t("已回答");
        case "skipped": return t("已跳过");
        case "expired": return t("超时未回答，已自动跳过");
        case "cancelled": return t("提问已取消");
    }
    return answerable(c) ? remaining(c.view) : t("回合已结束，未回答");
}
function head(c) {
    const box = document.createElement("div");
    box.className = "qa-head";
    const badge = document.createElement("span");
    badge.className = "qa-badge";
    buttonLabel(badge, () => answerable(c) ? t("需要你回答") : t("Agent 的提问"), "help");
    const state = c.state = document.createElement("span");
    state.className = "qa-state";
    setTextRender(state, () => stateText(c));
    box.append(badge, state);
    return box;
}
function questionText(item, withHeader = true) {
    const q = document.createElement("p");
    q.className = "qa-question";
    if (withHeader && item.header)
        q.append(Object.assign(document.createElement("span"), { className: "qa-header", textContent: item.header }));
    q.append(document.createTextNode(item.question));
    return q;
}
const emptyPick = () => ({ picked: new Set(), other: "", otherOn: false });
/** 一道题：单选点了选项就算答完（调用方据此跳到下一题），多选与「其他」不自动跳。 */
function group(c, item, i, withHeader, sync, answered) {
    const p = c.picks[i];
    const box = document.createElement("fieldset");
    box.className = "qa-group";
    const legend = document.createElement("legend");
    legend.append(questionText(item, withHeader));
    box.append(legend);
    const name = `qa-${c.view.id}-${i}`, multi = !!item.multi_select;
    // 只有指针点选才跳题：键盘在单选组里用方向键移动也会触发 change，不能一按就翻走
    let pointer = false;
    box.addEventListener("pointerdown", () => { pointer = true; });
    box.addEventListener("keydown", () => { pointer = false; });
    const choose = (label, on) => {
        if (!multi) {
            p.picked.clear();
            p.otherOn = false;
        }
        if (on)
            p.picked.add(label);
        else
            p.picked.delete(label);
        sync();
        if (!multi && on && pointer)
            answered();
        pointer = false;
    };
    for (const o of item.options || []) {
        const row = document.createElement("label");
        row.className = "qa-option";
        const input = Object.assign(document.createElement("input"), { type: multi ? "checkbox" : "radio", name, checked: p.picked.has(o.label), disabled: c.busy });
        input.addEventListener("change", () => choose(o.label, input.checked));
        const text = document.createElement("span");
        text.className = "qa-option-text";
        text.append(Object.assign(document.createElement("span"), { className: "qa-label", textContent: o.label }));
        if (o.description)
            text.append(Object.assign(document.createElement("span"), { className: "qa-desc", textContent: o.description }));
        row.append(input, text);
        box.append(row);
    }
    // 「其他」：选项之外的自由回答，与在终端里输入自定义答案一样
    const row = document.createElement("label");
    row.className = "qa-option qa-other";
    const toggle = Object.assign(document.createElement("input"), { type: multi ? "checkbox" : "radio", name, checked: p.otherOn, disabled: c.busy });
    const field = Object.assign(document.createElement("input"), { type: "text", className: "qa-other-input", value: p.other, maxLength: 4000, disabled: c.busy });
    setText(row.appendChild(document.createElement("span")), "其他");
    setAttr(field, "aria-label", "其他");
    setAttr(field, "placeholder", "自己写一个回答");
    const otherOn = (on) => {
        if (on && !multi)
            p.picked.clear();
        p.otherOn = on;
        pointer = false;
        sync();
    };
    toggle.addEventListener("change", () => { otherOn(toggle.checked); if (toggle.checked)
        field.focus(); });
    field.addEventListener("input", () => {
        p.other = field.value;
        if (field.value.trim() && !toggle.checked) {
            toggle.checked = true;
            otherOn(true);
            return;
        }
        sync();
    });
    row.prepend(toggle);
    row.append(field);
    box.append(row);
    return box;
}
/** 多道题时和 CLI 一样上方一排标签、一次只显示一题，答过的标签打勾，随时可以点回去改。 */
function form(c) {
    const body = document.createElement("div");
    body.className = "qa-form";
    const items = c.view.questions || [], many = items.length > 1;
    c.tab = Math.max(0, Math.min(c.tab, items.length - 1));
    const done = (i) => !!answerOf(items[i], c.picks[i]);
    const tabs = [];
    const panel = document.createElement("div");
    panel.className = "qa-panel";
    const submit = document.createElement("button");
    const progress = document.createElement("span");
    progress.className = "qa-progress";
    const sync = () => {
        submit.disabled = c.busy || items.some((_, i) => !done(i));
        tabs.forEach((tab, i) => tab.classList.toggle("done", done(i)));
        if (many)
            setTextRender(progress, () => t("已答 {n}/{total}", { n: String(items.filter((_, i) => done(i)).length), total: String(items.length) }));
    };
    const show = (i, focus = false) => {
        c.tab = i;
        tabs.forEach((tab, j) => {
            tab.classList.toggle("active", j === i);
            tab.setAttribute("aria-selected", String(j === i));
            tab.tabIndex = j === i ? 0 : -1;
        });
        if (many)
            panel.setAttribute("aria-labelledby", `qa-${c.view.id}-tab-${i}`);
        panel.replaceChildren(group(c, items[i], i, !many, sync, () => {
            // 单选答完跳到后面第一道没答的题；全答完就停在原地，方便核对后提交
            const next = items.findIndex((_, j) => j > i && !done(j));
            const target = next >= 0 ? next : items.findIndex((_, j) => !done(j));
            if (target >= 0)
                show(target);
        }));
        if (focus)
            tabs[i]?.focus();
    };
    if (many) {
        const bar = document.createElement("div");
        bar.className = "scope-switch qa-tabs";
        bar.setAttribute("role", "tablist");
        setAttr(bar, "aria-label", "问题");
        panel.setAttribute("role", "tabpanel");
        panel.id = `qa-${c.view.id}-panel`;
        items.forEach((item, i) => {
            const tab = document.createElement("button");
            tab.type = "button";
            tab.className = "scope-btn qa-tab";
            tab.id = `qa-${c.view.id}-tab-${i}`;
            tab.setAttribute("role", "tab");
            tab.setAttribute("aria-controls", panel.id);
            const mark = document.createElement("span");
            mark.className = "qa-tab-mark";
            mark.append(svgIcon("check", 12));
            const label = document.createElement("span");
            if (item.header)
                label.textContent = item.header;
            else
                setTextRender(label, () => t("问题 {n}", { n: String(i + 1) }));
            tab.append(mark, label);
            tab.addEventListener("click", () => show(i));
            tab.addEventListener("keydown", e => {
                const k = e.key === "ArrowRight" ? i + 1 : e.key === "ArrowLeft" ? i - 1 : e.key === "Home" ? 0 : e.key === "End" ? items.length - 1 : -2;
                if (k === -2)
                    return;
                e.preventDefault();
                show((k + items.length) % items.length, true);
            });
            tabs.push(tab);
            bar.append(tab);
        });
        trackInk(bar);
        body.append(bar);
    }
    body.append(panel);
    show(c.tab);
    if (c.error)
        body.append(Object.assign(document.createElement("p"), { className: "qa-error", textContent: c.error }));
    const actions = document.createElement("div");
    actions.className = "qa-actions";
    const skip = document.createElement("button");
    skip.type = "button";
    skip.className = "btn btn-sm btn-ghost qa-skip";
    skip.disabled = c.busy;
    setText(skip, "跳过");
    skip.addEventListener("click", () => void send(c, true));
    submit.type = "button";
    submit.className = "btn btn-sm btn-primary qa-submit";
    buttonLabel(submit, () => c.busy ? t("提交中…") : t("提交回答"), "send");
    submit.addEventListener("click", () => void send(c, false));
    sync();
    actions.append(progress, skip, submit);
    body.append(actions);
    return body;
}
/** 结束后的摘要：每题一行「标签 问题：回答」，窄屏自动折行。 */
function summary(c) {
    const body = document.createElement("div");
    body.className = "qa-summary";
    for (const item of c.view.questions || []) {
        const line = document.createElement("p");
        line.className = "qa-line";
        if (item.header)
            line.append(Object.assign(document.createElement("span"), { className: "qa-header", textContent: item.header }));
        line.append(Object.assign(document.createElement("span"), { className: "qa-q", textContent: item.question }));
        const answer = c.view.answers?.[item.question];
        if (answer)
            line.append(Object.assign(document.createElement("span"), { className: "qa-answer", textContent: answer }));
        body.append(line);
    }
    if (c.view.state === "skipped" || c.view.state === "expired") {
        const note = document.createElement("p");
        note.className = "qa-note";
        setText(note, "Agent 会按自己的判断继续，并说明它做了哪些假设。");
        body.append(note);
    }
    return body;
}
function fill(c) {
    const mode = answerable(c) ? "form" : "summary";
    c.el.dataset.state = FINAL.has(c.view.state) ? c.view.state : answerable(c) ? "pending" : "stale";
    c.el.replaceChildren(head(c), mode === "form" ? form(c) : summary(c));
    c.mode = mode;
}
async function send(c, skip) {
    const session = S.current?.id, items = c.view.questions || [];
    if (!session || c.busy || !answerable(c))
        return;
    const answers = {};
    items.forEach((item, i) => { answers[item.question] = answerOf(item, c.picks[i]); });
    c.busy = true;
    c.error = "";
    c.dirty = true;
    update();
    try {
        await api(`/sessions/${encodeURIComponent(session)}/chat/questions/${encodeURIComponent(c.view.id)}`, {
            method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(skip ? { skip: true } : { answers }),
        });
        // 服务端已收下这一份，结果记录随后到达；先按已知结果收起卡片
        if (!FINAL.has(c.view.state))
            c.view = { ...c.view, state: skip ? "skipped" : "answered", answers: skip ? undefined : answers };
    }
    catch (e) {
        if (e instanceof APIError && e.problem.code === "question_closed") {
            c.sealed = true;
            toast(e.message, true);
        }
        else
            c.error = e.message || t("提交失败");
    }
    c.busy = false;
    c.dirty = true;
    update();
}
/** 新建或原地更新一张卡片；已在页面上的卡片更新后返回 null，调用方不必再放置。 */
export function upsertQuestionCard(view) {
    if (!view?.id)
        return null;
    const known = cards.get(view.id);
    if (known) {
        // 结果记录不带问题本身；已经结束的卡片不被迟到的 pending 盖回去。较早一页
        // 先放了结果、再补到 pending 时，也只是给同一张卡片补上问题。
        if (FINAL.has(known.view.state) && !FINAL.has(view.state))
            view = { ...known.view, questions: view.questions || known.view.questions };
        known.view = { ...known.view, ...view, questions: view.questions || known.view.questions };
        known.picks = (known.view.questions || []).map((_, i) => known.picks[i] || emptyPick());
        known.dirty = true;
        update();
        return null;
    }
    const el = document.createElement("section");
    el.className = "question-card";
    el.dataset.question = view.id;
    const picks = (view.questions || []).map(emptyPick);
    cards.set(view.id, { el, view, sealed: false, busy: false, error: "", mode: "", dirty: true, picks, tab: 0 });
    update();
    return el;
}
/** 之后来了新的用户消息：这些卡片所在的回合已经过去，不再可答。不给 ID 时封存全部。 */
export function sealQuestions(ids) {
    for (const [id, c] of cards)
        if (!ids || ids.includes(id))
            c.sealed = true;
    update();
}
/** 回合状态或卡片变化后重画：只有内容或可答与否变了才重建，免得打断正在输入的
 * 「其他」；其余时候只刷新倒计时。 */
export function update() {
    let waiting = false;
    for (const c of cards.values()) {
        const mode = answerable(c) ? "form" : "summary";
        if (mode !== c.mode || c.dirty) {
            c.dirty = false;
            fill(c);
        }
        else if (c.state)
            setTextRender(c.state, () => stateText(c));
        if (mode === "form")
            waiting = true;
    }
    if (waiting && !ticker)
        ticker = window.setInterval(() => update(), 15000);
    if (!waiting && ticker) {
        clearInterval(ticker);
        ticker = 0;
    }
    changed();
}
/** 切换空间或重载对话时清空登记，旧节点随对话流一起丢弃。 */
/** 一段内容整体移出页面（历史窗口收起）：忘掉其中的卡片，之后对话流加载到同一段时重新画出 */
export function forgetQuestionCards(root) {
    for (const [id, c] of cards)
        if (root.contains(c.el))
            cards.delete(id);
}
export function resetQuestionCards() {
    cards.clear();
    if (ticker) {
        clearInterval(ticker);
        ticker = 0;
    }
}
