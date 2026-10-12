// Validate control-relevant identities and counters before exposing actions.
// Unknown protocol/state values fail closed rather than becoming legacy data.
import { cleanDecision } from "./decision.js";
import { cleanWorkflow } from "./strategy.js";
const object = (v) => !!v && typeof v === "object" && !Array.isArray(v);
const integer = (v, min = 0) => typeof v === "number" && Number.isSafeInteger(v) && v >= min;
const id = (v) => typeof v === "string" && /^[a-f0-9]{32}$/.test(v);
const states = new Set(["accepted", "preparing", "planning", "awaiting_plan", "executing", "waiting_answer", "waiting_user", "reviewing", "paused", "cancelling", "cancelled", "completed", "failed", "uncertain", "deleted"]);
const decisionStates = new Set(["pending", "claimed", "frozen", "interrupted", "deleted"]);
function binding(v) { return object(v) && (v.agent_type === "claude" || v.agent_type === "codex") && typeof v.account_id === "string" && v.account_id.length > 0 && typeof v.model === "string" && v.model.length > 0 && typeof v.effort === "string" && ["", "effort", "budget"].includes(String(v.effort_control)); }
function sameBinding(a, b) { return object(a) && object(b) && ["agent_type", "account_id", "model", "effort", "effort_control"].every(key => a[key] === b[key]); }
function stepUsage(value, run) {
    if (!Array.isArray(value) || value.length > 10000)
        return false;
    const steps = new Set(), turns = new Set();
    for (const row of value) {
        if (!object(row) || row.run_id !== run || typeof row.step_id !== "string" || !row.step_id || steps.has(row.step_id) || typeof row.turn_id !== "string" || !row.turn_id || turns.has(row.turn_id))
            return false;
        steps.add(row.step_id);
        turns.add(row.turn_id);
        if (!["coordinator", "executor"].includes(String(row.role)) || !["plan", "execute", "clarify", "review", "repair", "verify", "handoff_repair"].includes(String(row.kind)) || !["accepted", "starting", "running", "finalizing", "completed", "failed", "interrupted", "uncertain", "reviewed", "cancelled"].includes(String(row.state)) || !["pending", "settled", "partial", "unavailable"].includes(String(row.status)))
            return false;
        if (!Array.isArray(row.models) || !row.models.every(v => typeof v === "string"))
            return false;
        const tokenKeys = ["input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "total_tokens"];
        if ([...tokenKeys, "cost_micro_usd"].some(key => row[key] !== null && !integer(row[key])))
            return false;
        if (row.status === "pending" || row.status === "unavailable") {
            if ([...tokenKeys, "cost_micro_usd"].some(key => row[key] !== null))
                return false;
        }
        else {
            if (tokenKeys.some(key => !integer(row[key])))
                return false;
            const total = tokenKeys.slice(0, 4).reduce((n, key) => n + row[key], 0);
            if (!Number.isSafeInteger(total) || row.total_tokens !== total)
                return false;
            if (row.status === "settled" && (row.state !== "completed" || !integer(row.cost_micro_usd)))
                return false;
        }
    }
    return true;
}
export function validView(v, requested, scope) {
    if (!object(v) || v.version !== 1 || v.scope !== scope || !object(v.run) || !object(v.cost))
        return false;
    const r = v.run;
    if (!id(r.id) || !id(r.request_id) || (requested !== r.id && requested !== r.request_id) || typeof r.state !== "string" || !states.has(r.state) || !integer(r.revision, 1) || !integer(r.config_version, 1) || !integer(r.plan_version) || !integer(v.cost.cost_micro_usd) || typeof v.cost.partial !== "boolean")
        return false;
    for (const key of ["decision_cost_micro_usd", "decision_reserved_micro_usd", "decision_calls"])
        if (v.cost[key] !== undefined && !integer(v.cost[key]))
            return false;
    if (v.cost.decision_partial !== undefined && typeof v.cost.decision_partial !== "boolean")
        return false;
    if (v.step_usage !== undefined && !stepUsage(v.step_usage, String(r.id)))
        return false;
    if (v.decision !== undefined && v.decision !== null) {
        const d = v.decision;
        if (!object(d) || typeof d.state !== "string" || !decisionStates.has(d.state) || !integer(d.revision, 1))
            return false;
        if (d.state === "frozen" && (d.result === undefined || d.result === null))
            return false;
        if (d.attempt !== undefined && d.attempt !== null && (!object(d.attempt) || d.attempt.error_code !== undefined && typeof d.attempt.error_code !== "string" || d.attempt.can_reconcile !== undefined && typeof d.attempt.can_reconcile !== "boolean"))
            return false;
        if (object(d.attempt)) {
            const a = d.attempt;
            if (a.run_id !== undefined && a.run_id !== r.id || a.tokens_known !== undefined && typeof a.tokens_known !== "boolean")
                return false;
            for (const key of ["input_tokens", "output_tokens", "cost_micro_usd", "reserved_micro_usd"])
                if (a[key] !== undefined && !integer(a[key]))
                    return false;
            if (a.tokens_known === true && (!integer(a.input_tokens) || !integer(a.output_tokens)))
                return false;
            if (a.state !== undefined && !["dispatched", "settled", "unknown", "not_dispatched"].includes(String(a.state)))
                return false;
            for (const key of ["id", "provider", "model", "actual_model"])
                if (a[key] !== undefined && typeof a[key] !== "string")
                    return false;
        }
        if (d.result !== undefined && d.result !== null) {
            const result = d.result;
            if (!object(result) || !object(result.roles) || !object(result.decisions))
                return false;
            for (const role of ["coordinator", "executor"]) {
                const row = result.decisions[role];
                if (!binding(result.roles[role]) || !object(row) || !binding(row.binding) || !sameBinding(result.roles[role], row.binding) || !["manual", "builtin", "jev", "fallback"].includes(String(row.source)) || typeof row.reason !== "string")
                    return false;
            }
        }
    }
    if (r.pending_question !== undefined && !id(r.pending_question))
        return false;
    if (v.step !== null && (!object(v.step) || typeof v.step.id !== "string" || !integer(v.step.revision, 1) || !["coordinator", "executor"].includes(String(v.step.role))))
        return false;
    if (object(r.request) && r.request.decision !== undefined && (!cleanDecision(r.request.decision) || !object(v.decision)))
        return false;
    if (object(r.request) && r.request.workflow !== undefined && !cleanWorkflow(r.request.workflow))
        return false;
    if (r.state === "deleted")
        return true;
    const c = v.configuration;
    if (!object(c) || !object(c.roles) || !binding(c.roles.coordinator) || !binding(c.roles.executor) || !integer(c.max_steps, 1) || !integer(c.max_repairs) || !integer(c.budget_micro_usd) || typeof c.auto_execute !== "boolean")
        return false;
    if (c.workflow !== undefined && !cleanWorkflow(c.workflow))
        return false;
    if (object(r.request) && JSON.stringify(cleanWorkflow(c.workflow)) !== JSON.stringify(cleanWorkflow(r.request.workflow)))
        return false;
    return r.request === undefined || object(r.request) && typeof r.request.text === "string" && typeof r.request.source_session_id === "string" && (r.request.decision === undefined || !!cleanDecision(r.request.decision));
}
