import { api } from "../../api.js";
import { agentIcon, agentName } from "../../brand.js";
import { formatText } from "../../chat-render.js";
import { S } from "../../state.js";
import { setText, t } from "../../i18n.js";
import { actionButton } from "../../icons.js";
import { enhanceSelects, setSelectValue } from "../../select.js";
import { askConfirm } from "../../util.js";
import { roleSelector, workflowSelector } from "./roles.js";
import { collaborationClient, requestID, stateLabels, messageText, terminal } from "./client.js";
import { CollaborationPending } from "./pending.js";
import { createWorkflow } from "./workflow.js";
import { createEvidence } from "./evidence.js";
import { createLiveOutput } from "./live.js";
import { renderFinalResult, collaborationMessageLabel } from "./final-result.js";
import { automaticRequestDigest, matchesAutomaticReceipt, decisionReason } from "./decision.js";
import { isDirect, activeRoles, completionLabel } from "./strategy.js";
function el(tag, text) { const n = document.createElement(tag); if (text)
    setText(n, text); return n; }
function button(text, icon, action) { const b = el("button"); b.type = "button"; b.className = "btn btn-sm"; actionButton(b, () => t(text), icon); b.onclick = action; return b; }
function field(text, input) { const l = el("label"); l.className = "collaboration-field"; l.append(el("span", text), input); return l; }
export function initCollaborationComposer(h) {
    const life = new AbortController(), owner = S.token;
    const mode = document.getElementById("chat-execution-mode");
    const setup = document.getElementById("chat-collaboration-setup");
    const bar = mode.parentElement;
    const host = el("section");
    host.className = "collaboration-composer";
    bar.before(host);
    const notice = el("p");
    notice.className = "collaboration-composer-notice";
    notice.setAttribute("role", "status");
    const recovery = el("div");
    recovery.className = "collaboration-actions";
    const settings = el("details"), summary = el("summary", "协作设置"), roles = el("div");
    roles.className = "collaboration-roles";
    settings.append(summary);
    const repository = el("input");
    repository.value = ".";
    repository.maxLength = 1024;
    const repositories = el("datalist");
    repositories.id = "collaboration-repositories";
    repository.setAttribute("list", repositories.id);
    settings.append(repositories);
    const include = el("input");
    include.type = "checkbox";
    include.checked = true;
    const auto = el("input");
    auto.type = "checkbox";
    const verify = el("input");
    verify.maxLength = 8192;
    const contextPreview = el("pre");
    contextPreview.hidden = true;
    const contextToggle = button("查看携带的上下文", "eye", () => void work(async () => { const s = session, th = thread, e = epoch; const c = await client.context(s, th, include.checked); if (!valid(s, th, e))
        return; contextPreview.textContent = (c.truncated ? t("上下文较长，仅携带最近的可见消息。") + "\n\n" : "") + (c.text || t("仅使用本次需求")); contextPreview.hidden = false; }));
    const budget = el("input"), maxSteps = el("input");
    budget.type = maxSteps.type = "number";
    budget.value = "0";
    budget.min = "0";
    budget.step = "0.01";
    maxSteps.value = "20";
    maxSteps.min = "1";
    maxSteps.max = "100";
    const decisionControls = el("div");
    decisionControls.className = "collaboration-decision-settings";
    decisionControls.hidden = true;
    const useJev = el("input");
    useJev.type = "checkbox";
    useJev.dataset.useJev = "";
    const decisionHint = el("small");
    decisionHint.className = "collaboration-hint";
    const inheritProvider = button("沿用空间默认", "undo", () => { providerChoice = "inherit"; saveSettings(); });
    const saveDefault = button("设为此空间默认", "save", () => void work(async () => {
        const s = session, th = thread, e = epoch, provider = effectiveProvider();
        if (!s)
            return;
        const result = await api(`/sessions/${s}/collaboration/decision-default`, { method: "PUT", body: JSON.stringify({ scope, provider }), signal: life.signal });
        if (!valid(s, th, e))
            return;
        if (result.version !== 1 || result.scope !== scope || result.provider !== provider)
            throw new Error(t("无法确认任务状态"));
        workspaceProvider = result.provider;
        providerChoice = "inherit";
        saveSettings();
        notice.textContent = t("已保存此空间的决策偏好");
    }));
    decisionControls.append(field("使用 Jev 辅助判断", useJev), decisionHint, inheritProvider, saveDefault);
    const advanced = el("details");
    advanced.append(el("summary", "更多设置"), field("验证命令（可选）", verify), field("费用预算 USD（0 不限制）", budget), field("最多模型步骤", maxSteps));
    const strategy = workflowSelector(), autoField = field("规划后自动执行", auto);
    const swap = button("交换主辅", "switch", () => { try {
        const a = main.read(), b = worker.read(), am = main.mode(), bm = worker.mode();
        main.set(b);
        worker.set(a);
        main.setMode(bm);
        worker.setMode(am);
        saveSettings();
    }
    catch (e) {
        report(e);
    } });
    settings.append(strategy.box, roles, swap, decisionControls, field("仓库相对路径", repository), field("携带当前对话上下文", include), contextToggle, contextPreview, autoField, advanced);
    const targetLine = el("div");
    targetLine.className = "collaboration-reply-target";
    host.append(targetLine, notice, recovery, settings);
    let ready = false, scope = "", choices = [], client;
    let main, worker;
    let session = "", thread = "", epoch = 0, busy = false, polling = false, timer;
    let workflowSupported = false, automaticSupported = false, jevAvailable = false, serverProvider = "builtin", workspaceProvider = "inherit", providerChoice = "inherit", configuringRun = "";
    const effectiveProvider = () => providerChoice !== "inherit" ? providerChoice : workspaceProvider !== "inherit" ? workspaceProvider : serverProvider;
    let draft, target, prior;
    let store;
    const views = new Map(), logs = new Map(), applications = new Map();
    const owns = () => !life.signal.aborted && S.token === owner;
    const valid = (s = session, th = thread, e = epoch) => owns() && session === s && thread === th && epoch === e;
    let detailRun = "", detailTab = "overview";
    const detail = el("aside");
    detail.className = "collaboration-detail collaboration-workspace";
    detail.hidden = true;
    detail.setAttribute("aria-label", t("协作过程"));
    const detailHead = el("div");
    detailHead.className = "collaboration-detail-head";
    const detailTitle = el("strong", "协作过程"), detailStatus = el("span");
    detailStatus.className = "collaboration-workspace-status";
    function closeDetail() { const previous = detailRun; detail.hidden = true; detailRun = ""; live.close(); workflow.close(); evidence.clear(); document.querySelector(`[data-collaboration-run="${previous}"] [data-show-process]`)?.focus({ preventScroll: true }); }
    detailHead.append(detailTitle, detailStatus, button("关闭", "close", closeDetail));
    const detailTabs = el("nav");
    detailTabs.className = "collaboration-workspace-tabs";
    detailTabs.setAttribute("role", "tablist");
    detailTabs.setAttribute("aria-label", t("协作过程"));
    const detailBody = el("div");
    detailBody.className = "collaboration-detail-body collaboration-workspace-body";
    const detailPages = new Map(), detailButtons = new Map();
    function setDetailTab(tab) { detailTab = tab; if (tab !== "overview")
        workflow.close(); for (const [key, page] of detailPages)
        page.hidden = key !== tab; for (const [key, b] of detailButtons) {
        b.setAttribute("aria-selected", String(key === tab));
        b.tabIndex = key === tab ? 0 : -1;
    } if (tab === "overview")
        workflow.resume(); }
    for (const [key, label, icon] of [["overview", "过程", "activity"], ["output", "步骤输出", "tool"], ["result", "最终结果", "check"], ["records", "文件与记录", "list"]]) {
        const page = el("section");
        page.className = "collaboration-workspace-pane";
        page.dataset.pane = key;
        page.id = `collaboration-detail-${key}`;
        page.setAttribute("role", "tabpanel");
        page.setAttribute("aria-labelledby", `${page.id}-tab`);
        const b = button(label, icon, () => setDetailTab(key));
        b.id = `${page.id}-tab`;
        b.dataset.tab = key;
        b.setAttribute("role", "tab");
        b.setAttribute("aria-controls", page.id);
        detailPages.set(key, page);
        detailButtons.set(key, b);
        detailTabs.append(b);
        detailBody.append(page);
    }
    detailTabs.addEventListener("keydown", event => { const keys = [...detailButtons.keys()], current = keys.indexOf(detailTab); let next = current; if (event.key === "ArrowRight")
        next = (current + 1) % keys.length;
    else if (event.key === "ArrowLeft")
        next = (current + keys.length - 1) % keys.length;
    else if (event.key === "Home")
        next = 0;
    else if (event.key === "End")
        next = keys.length - 1;
    else
        return; event.preventDefault(); setDetailTab(keys[next]); detailButtons.get(keys[next]).focus(); }, { signal: life.signal });
    const finalResult = el("div"), finalEmpty = el("p", "最终结果将在任务完成后显示。");
    finalResult.hidden = true;
    finalEmpty.className = "collaboration-workspace-empty";
    const workflow = createWorkflow(entry => { if (entry.stepID) {
        setDetailTab("output");
        live.select(entry.stepID);
    } }), live = createLiveOutput(owner, owns), evidence = createEvidence(owner, life.signal, () => scope, owns);
    const exchange = el("div");
    exchange.className = "collaboration-exchange";
    const exchangeDetails = el("details");
    exchangeDetails.className = "collaboration-exchange-details";
    exchangeDetails.open = true;
    exchangeDetails.append(el("summary", "交接记录"), exchange);
    const fileActions = el("div");
    fileActions.className = "collaboration-workspace-file-actions";
    fileActions.append(button("查看文件变更", "diff", () => void work(async () => { if (detailRun)
        await evidence.showDiff(detailRun); })));
    detailPages.get("overview").append(workflow.box);
    detailPages.get("output").append(live.box);
    detailPages.get("result").append(finalResult, finalEmpty);
    detailPages.get("records").append(fileActions, evidence.box, exchangeDetails);
    detail.append(detailHead, detailTabs, detailBody);
    document.getElementById("tab-chat").append(detail);
    setDetailTab("overview");
    const report = (e) => { if (owns()) {
        notice.textContent = e instanceof Error ? e.message : String(e);
        notice.hidden = false;
    } };
    const selection = () => mode.value === "collaboration";
    const pending = () => store?.list().filter(p => p.session === session && p.thread === thread) || [];
    const render = () => {
        const active = selection();
        host.hidden = !active && !target && !notice.textContent && !pending().length;
        settings.hidden = !active || !!target;
        setup.hidden = !active || !!target;
        setup.classList.toggle("hidden", setup.hidden);
        document.getElementById("picker").hidden = active || !!target;
        const hint = document.getElementById("chat-collaboration-hint");
        hint.hidden = true;
        mode.hidden = !ready;
        mode.disabled = busy || !!target;
        if (main && worker) {
            main.active(!strategy.direct());
            setText(worker.box.querySelector("legend"), strategy.direct() ? "执行 Agent" : "辅 Agent：执行与测试");
        }
        swap.hidden = autoField.hidden = strategy.direct();
        roles.dataset.direct = String(strategy.direct());
        decisionControls.hidden = !automaticSupported || !!configuringRun;
        useJev.checked = effectiveProvider() === "jev";
        useJev.disabled = busy || (!jevAvailable && !useJev.checked);
        inheritProvider.disabled = busy || providerChoice === "inherit";
        saveDefault.disabled = busy || !session;
        setText(decisionHint, !jevAvailable ? "Jev 当前不可用，自动选模将使用内置规则或回退模型。" : providerChoice === "inherit" ? "沿用此空间的决策偏好；只有自动角色可能调用 Jev。" : "本次任务的决策偏好；固定模型不会调用 Jev。");
        targetLine.replaceChildren();
        targetLine.hidden = !target && !draft?.reply;
        if (!target && draft?.reply) {
            targetLine.append(el("span", "回复此任务"), button("返回草稿", "arrow-left", leaveTarget));
        }
        if (target) {
            targetLine.append(el("span", target.kind === "answer" ? "回复此任务" : "补充给此任务"), button("返回草稿", "arrow-left", leaveTarget));
        }
        notice.hidden = !notice.textContent;
        if (main && worker)
            try {
                const b = worker.read(), a = strategy.direct() ? b : main.read();
                setup.textContent = strategy.direct() ? `${agentName(b.agent_type)} · ${t("直接执行")} · ${t("设置")}` : `${a.agent_type === "claude" ? "Claude Code" : "Codex"} → ${b.agent_type === "claude" ? "Claude Code" : "Codex"} · ${t("设置")}`;
            }
            catch {
                setText(setup, "设置团队");
            }
        const pendingRows = pending();
        const pendingKey = JSON.stringify(pendingRows.map(p => [p.id, p.kind])) + t("查询待确认操作");
        if (recovery.dataset.signature !== pendingKey) {
            recovery.dataset.signature = pendingKey;
            recovery.replaceChildren();
            for (const p of pendingRows) {
                recovery.append(el("span", "有操作尚未确认，请先查询结果。"), button("查询待确认操作", "refresh", () => void work(() => queryPending(p))), button(p.kind === "application" ? "核对应用状态" : "放弃未接收操作", p.kind === "application" ? "refresh" : "close", () => void work(async () => { if (p.kind === "application") {
                    const run = p.path.split("/")[3];
                    await api(`/collaboration/runs/${run}/application/recover`, { method: "POST", body: JSON.stringify({ scope, action_id: p.id, snapshot_id: p.input?.snapshot_id }), signal: life.signal });
                    store.remove(p.id);
                    render();
                    return;
                } const path = p.kind === "create" ? `/collaboration/requests/${p.id}/abandon` : p.path + "/abandon"; await client.put(path, { scope }); store.remove(p.id); render(); })));
            }
        }
        for (const b of document.querySelectorAll(".collaboration-thread-body .collaboration-actions button"))
            b.disabled = busy;
        h.changed();
    };
    const config = () => {
        if (!main || !worker)
            throw new Error(t("请选择可用账号和模型"));
        const executor = worker.read(), workflow = strategy.read();
        const c = { ...(draft?.configuration || {}), roles: { coordinator: strategy.direct() ? executor : main.read(), executor }, ...(workflow ? { workflow } : {}), auto_execute: strategy.direct() || auto.checked, verification_command: verify.value, max_repairs: draft?.configuration?.max_repairs ?? 2, max_steps: Number(maxSteps.value), budget_micro_usd: Math.round(Number(budget.value) * 1e6), step_timeout_seconds: draft?.configuration?.step_timeout_seconds || 1800, total_runtime_seconds: draft?.configuration?.total_runtime_seconds || 21600, wait_timeout_seconds: draft?.configuration?.wait_timeout_seconds || 604800 };
        if (!Number.isInteger(c.max_steps) || c.max_steps < 1 || c.max_steps > 100 || !Number.isSafeInteger(c.budget_micro_usd) || c.budget_micro_usd < 0)
            throw new Error(t("请检查任务限额"));
        return c;
    };
    const decisionIntent = () => ({ version: 1, provider: providerChoice, roles: { coordinator: { mode: strategy.direct() ? "fixed" : main?.mode() || "fixed" }, executor: { mode: worker?.mode() || "fixed" } } });
    function saveSettings() { try {
        const configuration = config();
        if (strategy.direct()) {
            configuration.auto_execute = auto.checked;
            try {
                configuration.roles.coordinator = main.read();
            }
            catch {
                configuration.roles.coordinator = draft?.configuration?.roles.coordinator || configuration.roles.executor;
            }
        }
        draft = { mode: "collaboration", repository: repository.value, include_context: include.checked, configuration, ...(automaticSupported && !configuringRun ? { decision: { ...decisionIntent(), roles: { coordinator: { mode: main?.mode() || "fixed" }, executor: { mode: worker?.mode() || "fixed" } } } } : {}) };
    }
    catch (e) {
        report(e);
    } contextPreview.hidden = true; render(); h.save(); }
    function applySettings() {
        repository.value = draft?.repository || ".";
        include.checked = draft?.include_context ?? true;
        const c = draft?.configuration;
        strategy.configure(c?.workflow, workflowSupported, !!c, !!configuringRun);
        if (c && main && worker) {
            main.set(c.roles.coordinator);
            worker.set(c.roles.executor);
            auto.checked = c.auto_execute;
            verify.value = c.verification_command || "";
            budget.value = String(c.budget_micro_usd / 1e6);
            maxSteps.value = String(c.max_steps);
        }
        else if (main && worker) {
            const first = choices.find(c => c.id === S.current?.account_id) || choices[0], other = choices.find(c => c.type !== first?.type) || first;
            if (first && other) {
                main.set({ agent_type: first.type, account_id: first.id, model: first.id === S.current?.account_id ? S.pick.model || first.default_model : first.default_model, effort: "", effort_control: "" });
                worker.set({ agent_type: other.type, account_id: other.id, model: other.default_model, effort: "", effort_control: "" });
            }
            auto.checked = false;
            verify.value = "";
            budget.value = "0";
            maxSteps.value = "20";
        }
        providerChoice = draft?.decision?.provider || "inherit";
        main?.allowAutomatic(automaticSupported && !configuringRun);
        worker?.allowAutomatic(automaticSupported && !configuringRun);
        main?.setMode(draft?.decision?.roles.coordinator.mode || "fixed");
        worker?.setMode(draft?.decision?.roles.executor.mode || "fixed");
    }
    async function work(fn) { if (busy || !owns())
        return; busy = true; notice.textContent = ""; render(); try {
        await fn();
    }
    catch (e) {
        report(e);
    }
    finally {
        busy = false;
        render();
    } }
    async function queryPending(p) {
        const s = session, th = thread, e = epoch;
        const result = p.kind === "create" ? await client.view(p.id) : await client.query(p.query);
        if (!valid(s, th, e))
            return;
        if (p.kind === "create" && (p.decision_version === 1 || p.workflow_version === 1)) {
            const v = result;
            if (!v.run.request || v.run.request_id !== p.id || await automaticRequestDigest(v.run.request) !== p.request_fingerprint || !matchesAutomaticReceipt(v, p.input, p.id))
                throw new Error(t("自动任务回执与原请求不一致，请保留待确认记录。"));
            if (!valid(s, th, e))
                return;
        }
        if (p.kind === "action") {
            const receipt = result;
            if (receipt.version !== 1 || receipt.scope !== scope || receipt.action?.action_id !== p.id)
                throw new Error(t("无法确认任务状态"));
        }
        if (p.kind === "application") {
            const a = result.application;
            if (!a || a.action_id !== p.id)
                throw new Error(t("应用尚未被服务端确认，请核对任务记录。"));
            if (!["completed", "failed"].includes(a.state))
                throw new Error(t("应用需要核对，请使用任务中的应用恢复入口。"));
        }
        if (p.kind === "create" && p.draft && JSON.stringify(h.read()) === JSON.stringify(p.draft)) {
            h.apply({ text: "", attachments: [], collaboration: draft });
            h.save();
        }
        store.remove(p.id);
        notice.textContent = t("操作已确认");
        await h.reload();
        await poll();
    }
    async function action(v, kind, payload) {
        if (pending().length)
            throw new Error(t("请先查询待确认操作"));
        const id = requestID(), path = `/collaboration/runs/${v.run.id}/actions/${id}`, input = { scope, expected_revision: v.run.revision, action: kind, ...(payload ? { payload } : {}) };
        const p = { id, session, thread, path, query: path, input, created: Date.now(), kind: "action", text: "" };
        store.save(p);
        render();
        await client.put(path, input);
        if (!owns())
            return;
        store.remove(id);
        await poll();
    }
    function leaveTarget() { target = undefined; const value = prior; prior = undefined; if (value)
        h.apply(value);
    else
        h.apply({ text: "", attachments: [] }); render(); }
    function reply(v, kind) {
        if (target)
            leaveTarget();
        prior = structuredClone(h.read());
        target = { view: v, question: v.run.pending_question || "", kind };
        h.apply({ text: "", attachments: [], collaboration: { mode: "collaboration", repository: v.run.request?.repository || ".", include_context: true, configuration: v.configuration, reply: { run: v.run.id, question: v.run.pending_question || "", kind } } });
        render();
        document.getElementById("chat-input").focus();
    }
    function showDetail(v, tab = "overview") { if (detailRun !== v.run.id) {
        live.close();
        workflow.render(undefined);
        for (const page of detailPages.values())
            page.scrollTop = 0;
    } detailRun = v.run.id; detail.hidden = false; evidence.clear(); workflow.render(v, logs.get(v.run.id) || []); renderExchange(v); live.show(v, logs.get(v.run.id) || []); setDetailTab(tab); detailButtons.get(tab)?.focus({ preventScroll: true }); }
    function renderExchange(v) {
        detailStatus.textContent = t(v.run.state === "paused" && v.run.wait_reason === "verification_required" ? "等待验证" : v.run.state === "paused" && v.run.wait_reason === "delivery_required" ? "正在交付" : (v.run.state === "completed" ? completionLabel(v) : stateLabels[v.run.state]) || "未知状态");
        detailStatus.dataset.state = v.run.state;
        finalEmpty.hidden = v.run.state === "completed";
        detailButtons.get("result").dataset.ready = String(v.run.state === "completed");
        finalResult.hidden = v.run.state !== "completed";
        const events = logs.get(v.run.id) || [];
        const finalSignature = JSON.stringify([v.run.id, v.run.revision, events.at(-1)?.seq || 0, t("最终结果")]);
        if (finalResult.dataset.signature !== finalSignature) {
            finalResult.dataset.signature = finalSignature;
            finalResult.replaceChildren(renderFinalResult(v, events));
        }
        exchange.replaceChildren();
        for (const event of events) {
            if (event.type !== "message" || !event.body || typeof event.body !== "object")
                continue;
            const m = event.body;
            const entry = el("article"), label = el("strong"), text = el("pre");
            const b = m.sender_role === "executor" ? v.configuration.roles.executor : v.configuration.roles.coordinator;
            label.textContent = (m.sender_role === "user" ? t("用户") : `${b.agent_type === "claude" ? "Claude Code" : "Codex"} · ${collaborationMessageLabel(m.type)}`);
            text.textContent = messageText(m.body);
            entry.dataset.kind = m.type;
            entry.append(label, text);
            exchange.append(entry);
        }
    }
    function renderRun(v) {
        const root = document.querySelector(`[data-collaboration-run="${v.run.id}"] .collaboration-thread-body`);
        if (!root)
            return;
        const signature = `${v.run.revision}:${logs.get(v.run.id)?.at(-1)?.seq || 0}:${v.decision?.revision || 0}:${v.decision?.state || ""}:${v.decision?.attempt?.error_code || ""}:${!!v.decision?.attempt?.can_reconcile}:${v.cost.decision_calls || 0}:${v.cost.decision_cost_micro_usd || 0}:${v.cost.decision_reserved_micro_usd || 0}:${!!v.cost.decision_partial}:${applications.get(v.run.id) || ""}`;
        if (root.dataset.revision === signature)
            return;
        root.dataset.revision = signature;
        root.dataset.configVersion = String(v.run.config_version);
        root.dataset.planVersion = String(v.run.plan_version);
        root.replaceChildren();
        const title = el("span", applications.get(v.run.id) === "completed" ? "已应用到当前项目" : (v.run.state === "completed" ? completionLabel(v) : stateLabels[v.run.state]) || "未知状态"), team = el("div"), content = el("div"), actions = el("div");
        actions.className = "collaboration-actions";
        content.className = "msg agent collaboration-reply-content";
        const c = v.configuration.roles, head = el("header"), identity = el("strong", "协作回复"), brands = el("span");
        head.className = "collaboration-reply-head";
        title.className = "collaboration-reply-status";
        title.dataset.state = v.run.state;
        brands.className = "collaboration-reply-brands";
        for (const role of activeRoles(v))
            brands.append(agentIcon(c[role].agent_type, 20));
        head.append(brands, identity, title);
        team.className = "collaboration-reply-team";
        for (const role of activeRoles(v)) {
            const binding = c[role], member = el("span"), label = el("span", role === "coordinator" ? "主 Agent" : isDirect(v) ? "执行 Agent" : "辅 Agent"), model = el("span");
            label.className = "collaboration-reply-role";
            model.textContent = `${agentName(binding.agent_type)} · ${binding.model}`;
            member.append(label, model);
            team.append(member);
        }
        const routing = el("div");
        routing.className = "collaboration-routing";
        if (v.decision) {
            if (v.decision.result) {
                for (const role of activeRoles(v)) {
                    const row = v.decision.result.decisions[role], line = el("p");
                    line.textContent = `${t(role === "coordinator" ? "主 Agent" : isDirect(v) ? "执行 Agent" : "辅 Agent")} · ${row.binding.model} · ${t(decisionReason(row))}`;
                    routing.append(line);
                }
            }
            else
                routing.append(el("p", "正在确认自动选模结果…"));
            if (v.cost.decision_calls && ((v.cost.decision_cost_micro_usd || 0) > 0 || !v.cost.decision_reserved_micro_usd && !v.cost.decision_partial)) {
                const cost = el("small");
                cost.textContent = t("决策费用 ${cost}", { cost: ((v.cost.decision_cost_micro_usd || 0) / 1e6).toFixed(6) });
                routing.append(cost);
            }
            if (v.cost.decision_reserved_micro_usd) {
                const reserved = el("small");
                reserved.textContent = t("待核对决策费用，预算占用 ${cost}", { cost: (v.cost.decision_reserved_micro_usd / 1e6).toFixed(6) });
                routing.append(reserved);
            }
            else if (v.cost.decision_partial)
                routing.append(el("small", "决策调用用量尚待核对。"));
            if (v.decision.attempt?.error_code === "decision_persistence_failed")
                routing.append(el("p", v.decision.attempt.can_reconcile ? "决策费用写入失败，请核对后继续。" : "决策费用尚无确定凭据，请联系管理员核对。"));
        }
        else
            routing.hidden = true;
        const messages = (logs.get(v.run.id) || []).filter(e => e.type === "message").map(e => e.body);
        const m = v.run.state === "waiting_user" ? messages.find(m => m.id === v.run.pending_question) : [...messages].reverse().find(m => v.run.state !== "awaiting_plan" || m.type === "plan");
        if (m)
            content.append(formatText(messageText(m.body)));
        content.hidden = v.run.state === "completed";
        const add = (name, icon, fn) => actions.append(button(name, icon, () => void work(fn)));
        if (v.decision?.attempt?.error_code === "decision_persistence_failed" && v.decision.attempt.can_reconcile)
            add("核对决策费用", "wallet", async () => { await api(`/collaboration/runs/${v.run.id}/decision/reconcile`, { method: "POST", body: JSON.stringify({ scope, expected_revision: v.run.revision }), signal: life.signal }); if (owns())
                await poll(); });
        const process = button("查看过程", "activity", () => showDetail(v));
        process.dataset.showProcess = "";
        actions.append(process);
        if (v.run.state === "completed")
            actions.append(button("最终结果", "check", () => showDetail(v, "result")));
        if (v.run.state === "awaiting_plan")
            add("确认计划并执行", "play", () => action(v, "confirm_plan", { plan_version: v.run.plan_version }));
        if (v.run.state === "waiting_user")
            actions.append(button("答复", "message", () => reply(v, "answer")));
        if (["preparing", "planning", "executing", "reviewing", "waiting_answer"].includes(v.run.state))
            add("本步结束后暂停", "clock", () => action(v, "pause"));
        if (["awaiting_plan", "waiting_user", "planning", "executing", "reviewing", "paused"].includes(v.run.state))
            actions.append(button("补充要求", "plus", () => reply(v, "supplement")));
        if (v.run.state === "paused") {
            const blocked = ["execution_failed", "verification_required", "verification_failed", "delivery_required", "runtime_limit", "repair_limit", "budget_limit", "usage_incomplete", "context_limit", "artifacts_missing", "handoff_invalid", "instance_restored", "result_reviewed", "decision_persistence_failed"];
            if (v.decision?.attempt?.error_code !== "decision_persistence_failed" && !blocked.includes(v.run.wait_reason || ""))
                add("继续任务", "play", () => action(v, "resume"));
            if (v.run.wait_reason === "execution_failed")
                add("重新准备任务", "refresh", () => action(v, "configure", v.configuration));
        }
        if (["paused", "awaiting_plan", "waiting_user"].includes(v.run.state))
            actions.append(button("调整团队", "users", () => { configuringRun = v.run.id; draft = { mode: "collaboration", repository: v.run.request?.repository || ".", include_context: true, configuration: v.configuration }; applySettings(); setSelectValue(mode, "collaboration"); settings.open = true; render(); const apply = button("应用主辅配置", "check", () => void work(() => action(v, "configure", config()))); apply.dataset.configure = v.run.id; settings.querySelector('[data-configure]')?.remove(); settings.append(apply); }));
        if (v.run.state === "completed") {
            add("查看文件变更", "diff", async () => { showDetail(v, "records"); await evidence.showDiff(v.run.id); });
            add("应用到当前项目", "check", async () => {
                const prior = await api(`/collaboration/runs/${v.run.id}/application`, { signal: life.signal });
                if (prior.application && prior.application.state !== "failed") {
                    if (prior.application.state === "completed") {
                        notice.textContent = t("已应用到当前项目");
                        return;
                    }
                    if (prior.application.state === "failed") {
                        notice.textContent = t("应用未完成，原项目已保留。请查看变更或下载结果。");
                        return;
                    }
                    if (await askConfirm(t("恢复应用前的项目，保留待核对副本，不重新执行任务。"), { title: t("核对应用状态") })) {
                        await api(`/collaboration/runs/${v.run.id}/application/recover`, { method: "POST", body: JSON.stringify({ scope, action_id: prior.application.action_id }), signal: life.signal });
                        notice.textContent = t("原项目已恢复，待核对副本已保留。");
                    }
                    return;
                }
                showDetail(v, "records");
                await evidence.showDiff(v.run.id);
                const diff = await api(`/collaboration/runs/${v.run.id}/diff`, { signal: life.signal });
                if (!await askConfirm(t("请先停止当前工作空间。应用会保留 Git 信息和原项目副本，不自动提交、推送或部署。"), { title: t("应用到当前项目") }))
                    return;
                const id = requestID(), path = `/collaboration/runs/${v.run.id}/application/${id}`, input = { scope, expected_revision: v.run.revision, snapshot_id: diff.snapshot.id };
                store.save({ id, session, thread, path, query: `/collaboration/runs/${v.run.id}/application?action=${id}`, input, created: Date.now(), kind: "application", text: "" });
                render();
                await client.put(path, input);
                store.remove(id);
                notice.textContent = t("已应用到当前项目");
            });
            add("下载协作结果", "download", async () => { const response = await fetch(`/api/collaboration/runs/${v.run.id}/download`, { headers: { Authorization: `Bearer ${owner}` }, signal: life.signal }); if (!response.ok)
                throw new Error(t("结果下载失败")); const blob = await response.blob(); if (!owns())
                return; const url = URL.createObjectURL(blob), a = el("a"); a.href = url; a.download = `collaboration-${v.run.id}.zip`; a.click(); setTimeout(() => URL.revokeObjectURL(url), 1000); });
        }
        if (!terminal(v.run.state))
            add("取消任务", "close", async () => { if (await askConfirm(t("已产生的文件和费用不会回滚。"), { title: t("取消任务"), danger: true }))
                await action(v, "cancel"); });
        if (["uncertain", "failed", "paused"].includes(v.run.state))
            actions.append(button("更多操作", "more", () => window.dispatchEvent(new CustomEvent("collaboration-open", { detail: v.run.id }))));
        const cost = el("small");
        cost.textContent = `${t("费用")} $${(v.cost.cost_micro_usd / 1e6).toFixed(6)}${v.cost.partial ? " · " + t("费用尚不完整") : ""}`;
        const reason = el("p");
        if (v.run.state === "paused") {
            const reasons = { execution_failed: "Agent 运行环境启动失败", verification_failed: "验证失败", usage_incomplete: "费用尚不完整", budget_limit: "费用预算已用完", runtime_limit: "累计执行时间已用完", server_restarted: "服务重启后待核对", user_paused: "用户已暂停" };
            setText(reason, reasons[v.run.wait_reason || ""] || "请检查任务记录");
        }
        else
            reason.hidden = true;
        const footer = el("footer");
        footer.className = "collaboration-reply-footer";
        footer.append(actions, cost);
        root.append(head, team, routing, reason);
        if (v.run.state === "completed")
            root.append(renderFinalResult(v, logs.get(v.run.id) || []));
        root.append(content, footer);
        for (const b of actions.querySelectorAll("button"))
            b.disabled = busy;
    }
    async function loadDecisionDefault() {
        const s = session, th = thread, e = epoch;
        if (!s || !automaticSupported)
            return;
        try {
            const result = await api(`/sessions/${s}/collaboration/decision-default`, { signal: life.signal });
            if (!valid(s, th, e))
                return;
            if (result.version !== 1 || result.scope !== scope || !["inherit", "builtin", "jev"].includes(result.provider))
                throw new Error(t("无法确认任务状态"));
            workspaceProvider = result.provider;
            render();
        }
        catch (error) {
            if (valid(s, th, e))
                report(error);
        }
    }
    async function suggestRepository() {
        const s = session, th = thread, e = epoch;
        if (!s || !ready)
            return;
        try {
            const result = await api(`/sessions/${s}/collaboration/repositories`, { signal: life.signal });
            if (!valid(s, th, e) || result.version !== 1 || result.scope !== scope)
                return;
            repositories.replaceChildren(...result.repositories.map(p => new Option(p, p)));
            if (!draft && result.repositories.length === 1)
                repository.value = result.repositories[0];
        }
        catch { /* manual path remains available */ }
    }
    async function poll() {
        if (polling || !ready || !session || !thread || !owns())
            return;
        const s = session, th = thread, e = epoch;
        polling = true;
        try {
            const runs = await client.list(s, th);
            if (!valid(s, th, e))
                return;
            for (const anchor of document.querySelectorAll("[data-collaboration-run]")) {
                if (!runs.some(r => r.id === anchor.dataset.collaborationRun)) {
                    anchor.remove();
                }
            }
            for (const run of runs) {
                if (!valid(s, th, e))
                    return;
                const v = await client.view(run.id);
                if (!valid(s, th, e))
                    return;
                if (v.run.state === "completed") {
                    const application = await api(`/collaboration/runs/${run.id}/application`, { signal: life.signal });
                    if (!valid(s, th, e))
                        return;
                    applications.set(run.id, application.application?.state || "");
                }
                const rows = logs.get(run.id) || [];
                let after = rows.at(-1)?.seq || 0;
                for (let page = 0; page < 100; page++) {
                    const events = await client.events(run.id, after);
                    if (!valid(s, th, e))
                        return;
                    if (!events.length)
                        break;
                    for (const event of events) {
                        if (!Number.isSafeInteger(event.seq) || event.seq <= after)
                            throw new Error(t("无法确认任务状态"));
                        rows.push(event);
                        after = event.seq;
                    }
                    if (events.length < 100)
                        break;
                }
                if ((views.get(run.id)?.run.revision || 0) > v.run.revision)
                    continue;
                logs.set(run.id, rows);
                views.set(run.id, v);
                renderRun(v);
                if (terminal(v.run.state) && !h.read().text && !target && selection()) {
                    draft = undefined;
                    setSelectValue(mode, "chat");
                    render();
                    h.save();
                }
                if (detailRun === run.id) {
                    workflow.render(v, logs.get(v.run.id) || []);
                    renderExchange(v);
                    live.show(v, logs.get(v.run.id) || []);
                }
            }
        }
        catch (error) {
            if (valid(s, th, e))
                report(error);
        }
        finally {
            polling = false;
        }
    }
    mode.addEventListener("change", () => { notice.textContent = ""; if (selection()) {
        configuringRun = "";
        settings.querySelector('[data-configure]')?.remove();
        const suggested = repository.value;
        applySettings();
        if (!draft)
            repository.value = suggested;
        saveSettings();
    }
    else {
        draft = undefined;
        configuringRun = "";
        settings.open = false;
        h.save();
    } render(); }, { signal: life.signal });
    useJev.addEventListener("input", () => { providerChoice = useJev.checked ? "jev" : "builtin"; }, { signal: life.signal });
    setup.onclick = () => { settings.open = !settings.open; };
    settings.addEventListener("input", saveSettings, { signal: life.signal });
    settings.addEventListener("change", saveSettings, { signal: life.signal });
    window.addEventListener("chat-local-saving-changed", event => { const enabled = event.detail.enabled; if (store) {
        if (enabled)
            store.enabled = true;
        else
            store.disable();
    } }, { signal: life.signal });
    void (async () => {
        try {
            const o = await api("/collaboration/options", { signal: life.signal });
            if (!owns() || o.version !== 1 || o.thread_mode !== 1 || !/^[a-f0-9]{64}$/.test(o.scope))
                return;
            choices = o.accounts;
            scope = o.scope;
            workflowSupported = o.workflow_version === 1;
            automaticSupported = o.automatic_intent_version === 1;
            jevAvailable = !!o.decision?.jev_available;
            serverProvider = o.decision?.default_provider || "builtin";
            client = collaborationClient(scope, life.signal);
            store = new CollaborationPending(localStorage, S.draftScope);
            try {
                store.enabled = localStorage.getItem(`agentbox.chat-draft.v1.${S.draftScope}.enabled`) !== "off";
            }
            catch { /* saving fails closed until the user disables local saving */ }
            main = roleSelector("主 Agent：规划与评审", choices, life.signal, automaticSupported);
            worker = roleSelector("辅 Agent：执行与测试", choices, life.signal, automaticSupported);
            roles.append(main.box, worker.box);
            ready = true;
            applySettings();
            enhanceSelects(host);
            enhanceSelects(bar);
            render();
            void loadDecisionDefault();
            void suggestRepository();
            void poll();
        }
        catch (e) {
            if (selection())
                report(e);
        }
    })();
    window.addEventListener("agentbox-language-change", () => { for (const root of document.querySelectorAll(".collaboration-thread-body"))
        delete root.dataset.revision; for (const view of views.values())
        renderRun(view); render(); }, { signal: life.signal });
    timer = window.setInterval(() => { if (!document.hidden)
        void poll(); }, 2500);
    render();
    return {
        busy: () => busy,
        active: () => selection() || !!target,
        draft: () => selection() || target ? draft : undefined,
        apply(value) { draft = value; setSelectValue(mode, value ? "collaboration" : "chat"); applySettings(); render(); },
        enter(s, th) { if (s === session && th === thread) {
            void poll();
            return;
        } ++epoch; session = s; thread = th; workspaceProvider = "inherit"; configuringRun = ""; views.clear(); logs.clear(); applications.clear(); detail.hidden = true; detailRun = ""; live.close(); workflow.close(); evidence.clear(); target = undefined; prior = undefined; notice.textContent = ""; settings.open = false; settings.querySelector('[data-configure]')?.remove(); render(); void loadDecisionDefault(); void suggestRepository(); void poll(); },
        leave() { ++epoch; session = thread = ""; target = undefined; prior = undefined; detail.hidden = true; detailRun = ""; live.close(); workflow.close(); evidence.clear(); },
        refresh: poll,
        async send() {
            await work(async () => {
                if (!ready || !client || !session || !thread)
                    throw new Error(t("协作功能暂不可用，请刷新页面后重试"));
                if (!h.allowed())
                    return;
                if (pending().length)
                    throw new Error(t("请先查询待确认操作"));
                const value = structuredClone(h.read()), text = value.text.trim(), s = session, th = thread, e = epoch, selectionAtSend = mode.value;
                if (!text)
                    return;
                if (value.attachments.length)
                    throw new Error(t("协作模式暂不支持附件，请先移除附件或使用普通对话"));
                const unchanged = () => valid(s, th, e) && mode.value === selectionAtSend && JSON.stringify(h.read()) === JSON.stringify(value);
                if (!target && draft?.reply) {
                    const restored = await client.view(draft.reply.run);
                    if (!unchanged())
                        return;
                    target = { view: restored, question: draft.reply.question, kind: draft.reply.kind };
                }
                if (target) {
                    const dest = target;
                    const fresh = await client.view(dest.view.run.id);
                    if (!unchanged())
                        return;
                    if (dest.kind === "answer" && (fresh.run.state !== "waiting_user" || fresh.run.pending_question !== dest.question))
                        throw new Error(t("任务问题已变化，请重新选择答复目标。"));
                    await action(fresh, dest.kind, dest.kind === "answer" ? { question_id: dest.question, text } : { text });
                    if (unchanged())
                        leaveTarget();
                    if (dest.kind === "supplement")
                        notice.textContent = t(isDirect(fresh) ? "补充要求已保存，将由执行者在下一步处理。" : "补充要求已保存，将在当前步骤结束后重新规划。");
                    return;
                }
                const configuration = config(), repositoryValue = repository.value.trim() || ".";
                if (draft?.decision && !automaticSupported && [draft.decision.roles.coordinator.mode, draft.decision.roles.executor.mode].includes("auto"))
                    throw new Error(t("当前服务端不支持自动选模，请改为固定模型后发送。"));
                if (draft?.configuration?.workflow && !workflowSupported)
                    throw new Error(t("当前服务端不支持此执行方式，请刷新后重试。"));
                const requestedDecision = automaticSupported ? decisionIntent() : undefined, decision = requestedDecision && [requestedDecision.roles.coordinator.mode, requestedDecision.roles.executor.mode].includes("auto") ? requestedDecision : undefined;
                if ([...views.values()].some(v => !terminal(v.run.state)))
                    throw new Error(t("此对话已有未结束的协作任务，请先处理该任务或新建对话。"));
                notice.textContent = t("正在检查源项目，请稍候…");
                render();
                const context = await client.context(s, th, include.checked);
                if (!unchanged())
                    throw new Error(t("输入已变化，请确认后重新发送。"));
                const baseline = await client.preflight(s, repositoryValue, configuration);
                if (!unchanged())
                    throw new Error(t("输入已变化，请确认后重新发送。"));
                const id = requestID(), path = `/collaboration/runs/${id}`, input = { scope, source_session_id: s, repository: repositoryValue, text, context, baseline, ...configuration, ...(decision ? { decision } : {}) };
                const fingerprint = decision || configuration.workflow ? await automaticRequestDigest(input) : undefined;
                if (!unchanged())
                    throw new Error(t("输入已变化，请确认后重新发送。"));
                store.save({ id, session: s, thread: th, path, query: path, input, created: Date.now(), kind: "create", text, draft: value, ...(decision ? { decision_version: 1 } : {}), ...(configuration.workflow ? { workflow_version: 1 } : {}), ...(fingerprint ? { request_fingerprint: fingerprint } : {}) });
                render();
                const response = await client.put(path, input);
                if (!valid(s, th, e))
                    return;
                if (!matchesAutomaticReceipt(response, input, id))
                    throw new Error(t("自动任务回执与原请求不一致，请保留待确认记录。"));
                store.remove(id);
                notice.textContent = "";
                if (unchanged())
                    h.apply({ text: "", attachments: [], collaboration: draft });
                h.save();
                await h.reload();
                await poll();
            });
        },
        dispose() { if (S.token !== owner) {
            if (S.preserveChatCopiesOnSignout)
                store?.forget();
            else
                store?.clear();
        } life.abort(); if (timer)
            clearInterval(timer); live.close(); workflow.dispose(); detail.remove(); host.remove(); mode.hidden = true; document.getElementById("picker").hidden = false; setup.onclick = null; }
    };
}
