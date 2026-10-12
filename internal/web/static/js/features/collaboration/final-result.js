import { setText, t } from "../../i18n.js";
import { selectFinalOutcome } from "./outcome.js";
const node = (tag, label) => { const element = document.createElement(tag); if (label)
    setText(element, label); return element; };
/** Present an approved, delivered result separately from the agents' progress. */
export function renderFinalResult(view, events) {
    const box = node("section");
    box.className = "collaboration-final-result";
    box.dataset.finalResult = view.run.id;
    box.hidden = view.run.state !== "completed";
    if (box.hidden)
        return box;
    const title = node("h3", "最终结果");
    box.append(title);
    const outcome = selectFinalOutcome(view, events);
    if (!outcome) {
        box.append(node("p", "正在同步本次任务的最终结果…"));
        box.dataset.resultState = "loading";
        return box;
    }
    box.dataset.resultState = "ready";
    const summary = node("p");
    summary.className = "collaboration-final-summary";
    summary.textContent = outcome.result.summary;
    box.append(summary);
    const list = (label, items) => { if (!items.length)
        return; const group = node("div"), heading = node("h4", label), values = node("ul"); for (const text of items) {
        const item = node("li");
        item.textContent = text;
        values.append(item);
    } group.append(heading, values); box.append(group); };
    list("已完成", outcome.result.completed);
    list("待处理事项", outcome.result.remaining);
    const review = node("div");
    review.className = "collaboration-final-review";
    if (outcome.review) {
        const reviewer = view.configuration.roles.coordinator, by = node("small");
        by.textContent = `${reviewer.agent_type === "claude" ? "Claude Code" : "Codex"} · ${reviewer.model}`;
        const conclusion = node("p");
        conclusion.textContent = outcome.review.summary;
        review.append(node("h4", "评审确认"), by, conclusion);
    }
    else {
        review.append(node("h4", "未做独立模型评审"));
    }
    box.append(review);
    const verified = outcome.verification, verification = node("div");
    verification.className = "collaboration-final-verification";
    verification.append(node("h4", verified.state === "completed" ? "平台验证通过" : "未设置平台验证命令"));
    if (verified.command) {
        const command = node("code");
        command.textContent = verified.command;
        verification.append(command);
    }
    if (verified.output) {
        const details = node("details"), output = node("pre");
        output.textContent = verified.output;
        details.append(node("summary", "查看验证输出"), output);
        if (verified.output_truncated)
            details.append(node("p", "验证输出已截断"));
        verification.append(details);
    }
    box.append(verification);
    if (outcome.result.tests.length) {
        const details = node("details"), output = node("pre");
        output.textContent = outcome.result.tests.join("\n");
        details.append(node("summary", "执行方报告的检查"), output);
        box.append(details);
    }
    const note = node("small");
    note.className = "collaboration-final-note";
    note.textContent = t(outcome.review ? "这是已评审的任务结果；运行输出用于查看执行过程。" : "这是直接执行的任务结果；平台验证与执行方自报检查分别列出。");
    box.append(note);
    return box;
}
export function collaborationMessageLabel(type) {
    const labels = { plan: "规划", result: "执行报告", review: "评审确认", question: "提问", answer: "答复", system: "补充要求" };
    return t(labels[type] || "协作记录");
}
