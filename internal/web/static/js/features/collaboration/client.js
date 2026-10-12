import { api } from "../../api.js";
import { validView } from "./protocol.js";
import { t } from "../../i18n.js";
export const requestID = () => crypto.randomUUID().replaceAll("-", "");
export const terminal = (state) => ["completed", "cancelled", "failed", "deleted"].includes(state);
export const stateLabels = { accepted: "已接收", preparing: "准备项目", planning: "规划中", awaiting_plan: "等待确认计划", executing: "执行中", waiting_answer: "正在答疑", waiting_user: "等待你的答复", reviewing: "评审中", paused: "已暂停", cancelling: "正在取消", cancelled: "已取消", completed: "评审通过，结果待应用", failed: "任务失败", uncertain: "结果待核对", deleted: "已删除" };
export function messageText(value) {
    if (!value || typeof value !== "object")
        return "";
    const b = value, lines = [];
    for (const key of ["summary", "text"])
        if (typeof b[key] === "string")
            lines.push(b[key]);
    for (const key of ["items", "constraints", "completed", "remaining", "tests", "changes", "options"])
        if (Array.isArray(b[key]))
            for (const item of b[key]) {
                if (typeof item === "string")
                    lines.push("• " + item);
                else if (item && typeof item === "object" && typeof item.text === "string")
                    lines.push("• " + item.text);
            }
    if (Array.isArray(b.code_map))
        for (const entry of b.code_map) {
            if (!entry || typeof entry !== "object" || typeof entry.path !== "string" || typeof entry.purpose !== "string")
                continue;
            lines.push("\n" + entry.path + (Array.isArray(entry.symbols) ? " · " + entry.symbols.filter((s) => typeof s === "string").join(", ") : ""), entry.purpose);
            if (Array.isArray(entry.constraints))
                for (const constraint of entry.constraints)
                    if (typeof constraint === "string")
                        lines.push("• " + constraint);
            if (typeof entry.excerpt === "string")
                lines.push(entry.excerpt);
        }
    return lines.join("\n");
}
export function collaborationClient(scope, signal) {
    const check = (v) => { if (v.version !== 1 || v.scope !== scope)
        throw new Error(t("登录状态已变化")); return v; };
    return {
        async list(session, thread) { return check(await api(`/collaboration/runs?session=${encodeURIComponent(session)}&thread=${encodeURIComponent(thread)}`, { signal })).runs; },
        async view(id) { const v = await api(`/collaboration/runs/${id}`, { signal }); if (!validView(v, id, scope))
            throw new Error(t("无法确认任务状态")); return v; },
        async events(id, after) { return check(await api(`/collaboration/runs/${id}/events?after=${after}`, { signal })).events; },
        async context(session, thread, include) { return check(await api(`/sessions/${session}/collaboration/context?thread=${encodeURIComponent(thread)}&include=${include ? 1 : 0}`, { signal })).context; },
        async preflight(source_session_id, repository, configuration) { return check(await api("/collaboration/preflight", { method: "POST", body: JSON.stringify({ scope, source_session_id, repository, roles: configuration.roles, ...(configuration.workflow ? { workflow: configuration.workflow } : {}) }), signal })).baseline; },
        put(path, input) { return api(path, { method: "PUT", body: JSON.stringify(input), signal }); },
        query(path) { return api(path, { signal }); },
    };
}
