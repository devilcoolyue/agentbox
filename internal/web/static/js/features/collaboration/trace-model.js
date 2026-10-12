import { decisionReason } from "./decision.js";
import { activeRoles, isDirect } from "./strategy.js";
const object = (v) => !!v && typeof v === "object" && !Array.isArray(v);
const string = (v) => typeof v === "string" ? v : "";
const positive = (v) => typeof v === "number" && Number.isSafeInteger(v) && v > 0;
const role = (v) => v === "coordinator" || v === "executor";
const lane = (v) => role(v) || v === "user" || v === "decision" || v === "platform";
const stepID = (v) => typeof v === "string" && /^[a-f0-9]{12}$/.test(v);
const identity = (v) => typeof v === "string" && /^[a-f0-9]{32}$/.test(v);
const at = (e) => typeof e.created_at === "string" && Number.isFinite(Date.parse(e.created_at)) ? e.created_at : "";
const text = (v) => string(v).slice(0, 8192);
const summary = (v) => text(v.summary) || text(v.text);
const stepLabels = {
    plan: ["派发规划", "开始规划", "规划回合结束"],
    execute: ["派发执行", "开始执行", "执行回合结束"],
    repair: ["派发返工", "开始返工", "返工回合结束"],
    clarify: ["派发答疑", "开始答疑", "答疑回合结束"],
    review: ["派发评审", "开始评审", "评审回合结束"],
    handoff_repair: ["派发交接修复", "开始修复交接", "交接修复结束"],
};
const waitLabels = {
    user_paused: "用户已暂停", server_restarted: "服务重启后待核对", instance_restored: "恢复副本需重新配置",
    execution_failed: "Agent 运行环境启动失败", verification_failed: "平台验证失败", handoff_invalid: "交接内容需要修复",
    runtime_limit: "累计执行时间已用完", budget_limit: "费用预算已用完", step_limit: "已达步骤上限", repair_limit: "已达返工上限",
    usage_incomplete: "费用尚不完整", decision_persistence_failed: "决策结果待核对", decision_failed: "决策未完成",
    authorization_revoked: "账号授权已变化", wait_expired: "等待已到期", artifacts_missing: "项目文件不可用",
    result_reviewed: "结果已核对", context_limit: "上下文已达上限",
};
/** A projection of persisted events, not a predicted workflow or execution DAG.
 * Cross-agent arrows come only from the trusted message envelope. A queued
 * step is dispatched by the platform; it is not a model's receipt/acceptance.
 * Provider decision latency and confidence are not persisted and are never
 * synthesized here. The caller may show current routing activity from View.
 */
export function buildTrace(view, events) {
    const rows = [], seenSeq = new Set();
    for (const event of [...events].sort((a, b) => a.seq - b.seq)) {
        if (!positive(event.seq) || seenSeq.has(event.seq) || !object(event.body))
            continue;
        seenSeq.add(event.seq);
        const body = event.body;
        if (view && typeof body.run_id === "string" && body.run_id !== view.run.id)
            continue;
        if (view && event.type === "state" && body.id !== view.run.id)
            continue;
        rows.push({ event, body });
    }
    const out = [], steps = new Map(), bindings = new Map();
    const emitted = new Set();
    const push = (row, suffix, from, to, label, kind, tone = "normal", detail = "", which, durationMS) => {
        const id = `${view?.run.id || "trace"}:${suffix}`;
        if (emitted.has(id))
            return;
        emitted.add(id);
        out.push({ id, seq: row.event.seq, at: at(row.event), from, to, label, detail, kind, tone, ...(which ? { stepID: which } : {}), ...(durationMS !== undefined ? { durationMS } : {}) });
    };
    // Configuration changes are historical boundaries. Never label an old
    // execution with the model currently selected after a reconfiguration.
    for (const row of rows) {
        const { event, body } = row;
        if (event.type === "created" && object(body.roles))
            bindings.set(1, body.roles);
        if (event.type === "decision" && object(body.roles))
            bindings.set(1, body.roles);
        if (event.type === "configuration" && positive(body.version) && object(body.configuration) && object(body.configuration.roles))
            bindings.set(body.version, body.configuration.roles);
    }
    if (view && !bindings.has(view.run.config_version) && view.configuration?.roles)
        bindings.set(view.run.config_version, view.configuration.roles);
    const model = (body, owner) => {
        const cfg = positive(body.config_version) ? bindings.get(body.config_version) : undefined;
        const binding = cfg?.[owner];
        return object(binding) ? string(binding.model) : "";
    };
    let lastState = "";
    for (const row of rows) {
        const { event, body } = row;
        if (event.type === "created") {
            push(row, "created", "user", "platform", "提交任务", "dispatch", "normal", text(body.text));
        }
        else if (event.type === "decision" && object(body.decisions)) {
            for (const owner of activeRoles(view || {})) {
                const d = body.decisions[owner];
                if (!object(d) || !object(d.binding) || typeof d.binding.model !== "string" || typeof d.reason !== "string" || !["manual", "builtin", "jev", "fallback"].includes(string(d.source)))
                    continue;
                const label = { manual: "指定模型", builtin: "规则选模", jev: "Jev 选模", fallback: "回退模型" }[d.source];
                const provider = d.source === "jev" || d.source === "fallback" ? string(body.provider_model) : "";
                const detail = [d.binding.model, decisionReason(d), provider].filter(Boolean).join(" · ");
                push(row, `decision:${event.seq}:${owner}`, "decision", owner, label, "decision", d.source === "fallback" ? "warning" : "normal", detail);
            }
        }
        else if (event.type === "step" && stepID(body.id) && role(body.role) && stepLabels[string(body.kind)]) {
            const entries = steps.get(body.id) || [];
            entries.push(row);
            steps.set(body.id, entries);
        }
        else if (event.type === "message" && identity(body.id) && lane(body.sender_role) && object(body.body)) {
            // Supplements intentionally have no recipient; keep them on the user lane.
            if (!lane(body.recipient_role)) {
                if (body.sender_role === "user" && body.type === "system")
                    push(row, `message:${body.id}`, "user", "user", "补充要求", "handoff", "normal", summary(body.body));
                continue;
            }
            const from = body.sender_role, to = body.recipient_role, payload = body.body;
            let label = "", kind = "handoff", tone = "normal";
            switch (body.type) {
                case "plan":
                    label = "交接计划";
                    break;
                case "question":
                    label = to === "user" ? "向你提问" : "请求澄清";
                    tone = "warning";
                    break;
                case "answer":
                    label = from === "user" ? "用户答复" : "答复澄清";
                    break;
                case "result":
                    label = "提交执行结果";
                    kind = "result";
                    break;
                case "review":
                    if (payload.decision === "revise") {
                        label = "退回修改";
                        tone = "warning";
                    }
                    else if (payload.decision === "approve") {
                        label = "评审通过";
                        tone = "success";
                    }
                    else if (payload.decision === "needs_user") {
                        label = "请求人工确认";
                        tone = "warning";
                    }
                    break;
            }
            if (label)
                push(row, `message:${body.id}`, from, to, label, kind, tone, summary(payload), stepID(body.step_id) ? body.step_id : undefined);
        }
        else if (event.type === "state") {
            const state = string(body.state), reason = string(body.wait_reason), key = [state, reason, body.config_version].join(":");
            if (key === lastState)
                continue;
            lastState = key;
            const suffix = `state:${event.seq}`;
            if (state === "preparing")
                push(row, suffix, "platform", "platform", "准备工作副本", "system");
            else if (state === "awaiting_plan")
                push(row, suffix, "platform", "user", "等待确认计划", "wait", "warning");
            else if (state === "waiting_user")
                push(row, suffix, "platform", "user", "等待你的答复", "wait", "warning");
            else if (state === "paused" && reason === "verification_required")
                push(row, suffix, "platform", "platform", "安排平台验证", "system");
            else if (state === "paused" && reason === "delivery_required")
                push(row, suffix, "platform", "platform", "准备交付结果", "system");
            else if (state === "paused")
                push(row, suffix, "platform", "platform", waitLabels[reason] || "任务已暂停", "wait", "warning", waitLabels[reason] ? "" : reason);
            else if (state === "completed")
                push(row, suffix, "platform", "user", "交付结果已生成", "result", "success", isDirect(view || {}) ? "未做独立模型评审" : "");
            else if (state === "uncertain")
                push(row, suffix, "platform", "user", "执行结果待核对", "wait", "warning", waitLabels[reason] || reason);
            else if (state === "failed")
                push(row, suffix, "platform", "user", "任务失败", "system", "error", waitLabels[reason] || reason);
            else if (state === "cancelling")
                push(row, suffix, "platform", "platform", "正在取消任务", "system", "warning");
            else if (state === "cancelled")
                push(row, suffix, "platform", "user", "任务已取消", "system");
            else if (state === "deleted")
                push(row, suffix, "platform", "user", "任务已删除", "system");
        }
        else if (event.type === "verification" || event.type === "verification_stopped") {
            const state = string(body.state);
            const key = identity(body.helper_id) ? body.helper_id : identity(body.snapshot_id) && positive(body.config_version) ? `${body.snapshot_id}:${body.config_version}` : "";
            if (!key)
                continue;
            if (event.type === "verification_stopped") {
                // Recovery proves that the helper has stopped, not whether its command
                // passed or failed. Keep this distinct from an observed test failure.
                if (state === "failed" && body.reason === "outcome_unknown")
                    push(row, `verification-stopped:${key}`, "platform", "platform", "验证已停止，结果未知", "system", "warning");
                continue;
            }
            let label = "", tone = "normal";
            if (state === "running")
                label = "开始平台验证";
            else if (state === "completed" && body.exit_code === 0) {
                label = "平台验证通过";
                tone = "success";
            }
            else if (state === "failed") {
                label = "平台验证失败";
                tone = "error";
            }
            else if (state === "not_requested")
                label = "未配置平台验证";
            else if (state === "uncertain") {
                label = "验证结果待核对";
                tone = "warning";
            }
            if (label)
                push(row, `verification:${key}:${state}`, "platform", "platform", label, "system", tone, text(body.command));
        }
        else if (event.type === "application" && identity(body.action_id)) {
            const state = string(body.state);
            const label = { switching: "正在应用结果", completed: "结果已应用到项目", failed: "应用结果失败", uncertain: "应用结果待核对" };
            if (label[state])
                push(row, `application:${body.action_id}:${state}`, "platform", state === "switching" ? "platform" : "user", label[state], "result", state === "completed" ? "success" : state === "failed" ? "error" : state === "uncertain" ? "warning" : "normal");
        }
        else if (event.type === "configuration") {
            push(row, `configuration:${event.seq}`, "platform", "platform", "协作配置已更新", "system");
        }
    }
    for (const [id, history] of steps) {
        const first = history[0], last = history[history.length - 1], owner = first.body.role;
        // Startup/running/flushing are one start record. Completion remains a later
        // event, so replay cannot project future completion or duration backwards.
        // Separate attempts have new IDs and therefore distinct start/end records.
        const accepted = history.find(row => row.body.state === "accepted");
        const labels = stepLabels[string(first.body.kind)];
        if (accepted)
            push(accepted, `step:${id}:dispatch`, "platform", owner, labels[0], "dispatch", "normal", model(first.body, owner), id);
        const started = history.find(row => ["starting", "running", "finalizing"].includes(string(row.body.state)));
        const endings = history.filter(row => ["completed", "failed", "uncertain", "cancelled", "reviewed"].includes(string(row.body.state)));
        if (!started && !endings.length)
            continue;
        const state = string(last.body.state);
        const current = !view || view.step?.id === id && !["paused", "uncertain", "completed", "cancelled", "failed", "deleted"].includes(view.run.state);
        const live = current && ["starting", "running", "finalizing"].includes(state);
        if (started)
            push(started, `step:${id}:start`, owner, owner, labels[1], "work", live ? "active" : "normal", model(first.body, owner), id);
        for (const ending of endings) {
            const state = string(ending.body.state);
            if (state === "reviewed") {
                // A user's acknowledgement neither repairs nor erases the earlier
                // uncertain result. Its waiting time is not execution duration.
                push(ending, `step:${id}:reviewed`, "platform", "platform", "回合结果已核对", "system", "normal", model(first.body, owner), id);
                continue;
            }
            let label = labels[2], tone = "normal";
            if (state === "completed") {
                label = labels[2];
                tone = "success";
            }
            else if (state === "failed") {
                label = "回合执行失败";
                tone = "error";
            }
            else if (state === "uncertain") {
                label = "回合结果待核对";
                tone = "warning";
            }
            else if (state === "cancelled")
                label = "回合已取消";
            const startMS = started ? Date.parse(at(started.event)) : NaN, endMS = Date.parse(at(ending.event));
            const elapsed = started && Number.isFinite(startMS) && Number.isFinite(endMS) && endMS >= startMS ? `${((endMS - startMS) / 1000).toFixed(1)}s` : "";
            push(ending, `step:${id}:end:${state}`, owner, owner, label, "work", tone, [model(first.body, owner), elapsed, string(ending.body.error_code)].filter(Boolean).join(" · "), id, elapsed ? endMS - startMS : undefined);
        }
    }
    return out.sort((a, b) => a.seq - b.seq);
}
/** Reduce transport chatter while retaining branches and their actual order.
 * Only successfully paired handoffs are folded into their finished step.
 * Full persisted events remain available; this projection never drives work. */
export function compactTrace(entries) {
    const hidden = new Set(), replacement = new Map();
    const groups = new Map();
    for (const e of entries)
        if (e.stepID) {
            const group = groups.get(e.stepID) || [];
            group.push(e);
            groups.set(e.stepID, group);
        }
    for (const [id, group] of groups) {
        const dispatch = group.find(e => e.id.endsWith(`step:${id}:dispatch`));
        const start = group.find(e => e.id.endsWith(`step:${id}:start`));
        const end = group.find(e => e.id.includes(`step:${id}:end:`));
        const anchor = end || start || dispatch;
        if (!anchor)
            continue;
        const branches = group.some(e => ["请求澄清", "向你提问", "答复澄清", "退回修改", "请求人工确认"].includes(e.label));
        if (dispatch && dispatch !== anchor)
            hidden.add(dispatch.id);
        if (start && end && !branches)
            hidden.add(start.id);
        const handoff = end?.tone === "success" ? group.find(e => ["交接计划", "提交执行结果", "评审通过"].includes(e.label)) : undefined;
        if (handoff)
            hidden.add(handoff.id);
        const label = handoff?.label || anchor.label;
        replacement.set(anchor.id, { ...anchor, label, from: handoff?.from || (!end ? dispatch?.from : undefined) || anchor.from, to: handoff?.to || anchor.to,
            kind: handoff?.kind || anchor.kind, detail: [anchor.detail, handoff?.detail].filter(Boolean).join("\n") });
    }
    // Scheduling notices add no new outcome; retain the actual helper result.
    const redundant = new Set(["准备工作副本", "安排平台验证", "准备交付结果"]);
    const verificationFinished = entries.some(e => ["平台验证通过", "平台验证失败", "验证结果待核对", "验证已停止，结果未知"].includes(e.label));
    return entries.filter(e => !hidden.has(e.id) && !redundant.has(e.label) && !(e.label === "开始平台验证" && verificationFinished)).map(e => replacement.get(e.id) || e);
}
