import { readChatReceipt } from "../../contracts/chat.js";
import { S } from "../../state.js";
import { api } from "../../api.js";
import { $, askConfirm } from "../../util.js";
import { actionButton } from "../../icons.js";
import { t, setTextRender } from "../../i18n.js";
import { APIError } from "../../problems.js";
import { keyboardInput } from "../../modality.js";
import { OutboxStore, receiptActive, draftFromRequest } from "./outbox-store.js";
const labels = { unconfirmed: "等待接收确认", accepted: "消息已接收", starting: "正在准备任务", running: "任务正在执行", completed: "任务已完成", failed: "任务未开始", interrupted: "任务已中断", uncertain: "结果待核对", reviewed: "已核对未知结果", abandoned: "已放弃此消息", deleted: "消息已删除" };
const identifier = () => Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, "0")).join("");
const key = (session, id) => session + "/" + id;
const envelope = (input) => JSON.stringify([input.scope, input.thread_id, input.text, input.model, input.effort, input.effort_control, input.attachments || []]);
function receipt(value, session, id) {
    try {
        return readChatReceipt(value, session, id);
    }
    catch {
        throw Error(t("消息状态响应无效，请重新查询。"));
    }
}
/** Owns only delivery/recovery. Polling and socket events never submit a task. */
export function initChatOutbox(hooks) {
    const owner = S.token, scope = S.chatScope, protocol = S.chatProtocol, lifetime = new AbortController();
    const storage = new OutboxStore(localStorage, scope, S.draftScope);
    const enabled = protocol === 1;
    const unsupported = protocol !== 0 && protocol !== 1;
    let active = true, context, epoch = 0, viewAbort = new AbortController();
    let loading = false, queryFailed = false, scopeChanged = false, refreshing = false, again = false, notice = "", noticeError = false, noticeTranslated = false;
    const dismissed = new Set();
    let listSignature = "", expanded = false;
    const showDetails = (open) => { expanded = open; $("chat-delivery-toggle").setAttribute("aria-expanded", String(open)); $("chat-delivery-list").classList.toggle("hidden", !open); };
    let confirmation = false;
    let socketState;
    const confirm = async (message) => { confirmation = true; try {
        return await askConfirm(() => t(message));
    }
    finally {
        confirmation = false;
    } };
    const known = new Map(), views = new Map(), busy = new Set(), missing = new Set();
    const owns = () => { try {
        return active && S.token === owner && localStorage.getItem("agentbox_token") === owner;
    }
    catch {
        return active && S.token === owner;
    } };
    const current = (session, generation) => owns() && context?.session === session && epoch === generation;
    const endpoint = (session, id) => `/sessions/${encodeURIComponent(session)}/chat/requests${id ? "/" + encodeURIComponent(id) : ""}`;
    const rows = () => {
        const result = new Map();
        for (const row of views.values())
            if (row.session === context?.session)
                result.set(row.id, row);
        for (const row of storage.rows(context?.session))
            if (row.session === context?.session)
                result.set(row.id, row);
        for (const row of result.values()) {
            const c = known.get(key(row.session, row.id));
            if (c && c.revision >= row.revision) {
                row.revision = c.revision;
                row.state = c.state;
            }
        }
        return [...result.values()].filter(row => row.thread === context?.thread || row.state === "unconfirmed" || receiptActive(row.state) || row.state !== "completed" && (row.draft || row.input)).sort((a, b) => b.created - a.created);
    };
    const blocked = () => !owns() || unsupported || enabled && (!scope || !context || loading || queryFailed || scopeChanged || rows().some(r => r.state === "unconfirmed" || receiptActive(r.state)));
    const say = (message, error = false) => { notice = message; noticeError = error; noticeTranslated = false; render(); };
    const apply = (c) => {
        const id = key(c.session_id, c.request_id), old = known.get(id);
        if (old && old.revision > c.revision)
            return;
        const original = storage.rows(c.session_id).find(r => r.id === c.request_id);
        if (original && original.revision > c.revision)
            return;
        if (original && (c.thread_id && c.thread_id !== original.thread || original.input && c.request && envelope(original.input) !== envelope(c.request)))
            throw Error(t("消息状态响应无效，请重新查询。"));
        known.set(id, c);
        missing.delete(id);
        storage.observe(c);
        if (dismissed.has(id))
            return;
        if (!views.has(id))
            views.set(id, { version: 1, id: c.request_id, session: c.session_id, thread: c.thread_id, created: Date.parse(c.created_at) || Date.now(), input: null, draft: null, revision: c.revision, state: c.state });
        const local = storage.rows(c.session_id).find(r => r.id === c.request_id);
        if (local)
            views.set(id, local);
        if (c.state === "completed")
            storage.remove(c.request_id, c.session_id);
    };
    const response = (value, session, id) => {
        if (value?.version !== 1)
            throw Error(t("消息状态响应无效，请重新查询。"));
        const c = receipt(value.receipt, session, id);
        apply(c);
        return c;
    };
    async function query(row, signal = lifetime.signal) {
        const id = key(row.session, row.id);
        try {
            const result = await api(endpoint(row.session, row.id), { signal: AbortSignal.any([signal, AbortSignal.timeout(12000)]) });
            if (!owns() || signal.aborted)
                return;
            return response(result, row.session, row.id);
        }
        catch (error) {
            if (!owns() || signal.aborted)
                return;
            if (error instanceof APIError && error.status === 404 && error.problem.code === "chat_request_not_found") {
                missing.add(id);
                return;
            }
            throw error;
        }
    }
    async function refresh() {
        if (!enabled || !owns() || !context || document.hidden)
            return;
        if (refreshing) {
            again = true;
            return;
        }
        refreshing = true;
        const { session } = context, generation = epoch, signal = viewAbort.signal;
        try {
            const result = await api(endpoint(session) + "?pending_only=1", { signal: AbortSignal.any([signal, AbortSignal.timeout(12000)]) });
            if (!current(session, generation))
                return;
            if (result?.version !== 1 || typeof result.scope !== "string" || !Array.isArray(result.requests) || result.requests.length > 50 || !Object.hasOwn(result, "pending") || result.pending !== null && (!result.pending || typeof result.pending !== "object" || Array.isArray(result.pending)))
                throw Error(t("消息状态响应无效，请重新查询。"));
            if (result.scope !== scope) {
                scopeChanged = true;
                throw Error(t("消息所属的实例或登录身份已变化"));
            }
            if (result.pending)
                apply(receipt(result.pending, session));
            // Query saved IDs even when the list has no active turn: a lost response can
            // belong to a task that already completed or has not arrived yet.
            const candidates = new Map(storage.rows(session).map(row => [row.id, row]));
            for (const row of views.values())
                if (row.session === session && receiptActive(known.get(key(session, row.id))?.state || row.state))
                    candidates.set(row.id, row);
            for (const row of candidates.values()) {
                const c = known.get(key(session, row.id));
                if (!c || receiptActive(c.state))
                    await query(row, signal);
                if (!current(session, generation))
                    return;
            }
            queryFailed = false;
            notice = "";
            noticeTranslated = false;
        }
        catch {
            if (current(session, generation)) {
                queryFailed = true;
                notice = "无法确认消息结果，已保留待确认内容。请重新查询。";
                noticeError = true;
                noticeTranslated = false;
            }
        }
        finally {
            refreshing = false;
            if (current(session, generation)) {
                loading = false;
                render();
            }
            if (again) {
                again = false;
                void refresh();
            }
        }
    }
    const guard = () => owns() && !scopeChanged && /^[a-f0-9]{64}$/.test(scope);
    async function transmit(row) {
        const id = key(row.session, row.id);
        if (!guard() || busy.has(id) || !row.input)
            return;
        const generation = epoch;
        busy.add(id);
        render();
        try {
            const value = await api(endpoint(row.session, row.id), { method: "PUT", body: JSON.stringify(row.input), signal: AbortSignal.any([lifetime.signal, AbortSignal.timeout(15000)]) });
            if (owns())
                response(value, row.session, row.id);
        }
        catch (error) {
            if (owns()) {
                if (current(row.session, generation)) {
                    notice = error instanceof APIError ? error.message : "发送确认未收到，内容已保留。请先查询结果。";
                    noticeError = true;
                    noticeTranslated = error instanceof APIError;
                }
                try {
                    await query(row);
                }
                catch {
                    if (current(row.session, generation))
                        queryFailed = true;
                }
            }
        }
        finally {
            busy.delete(id);
            if (owns())
                render();
        }
    }
    async function retry(row) {
        if (!guard() || busy.has(key(row.session, row.id)))
            return;
        const generation = epoch;
        // An explicit retry always queries first and only reuses the same envelope.
        try {
            const found = await query(row);
            if (!current(row.session, generation))
                return;
            if (found) {
                render();
                return;
            }
            if (!missing.has(key(row.session, row.id)) || !row.input) {
                say("待确认正文已过期；仍可查询结果或放弃原消息编号。", true);
                return;
            }
            if (context?.session !== row.session || context.thread !== row.thread) {
                say("请返回原对话后重试这条消息。", true);
                return;
            }
            if (!storage.add(row)) {
                say("待确认副本未保存，暂未发送。请检查本机存储，或关闭本机保存后重试。", true);
                return;
            }
            if (row.draft)
                hooks.move(row.draft);
            await transmit(row);
        }
        catch {
            if (current(row.session, generation))
                say("无法确认消息结果，已保留待确认内容。请重新查询。", true);
        }
    }
    async function act(row, action) {
        if (!guard() || busy.has(key(row.session, row.id)))
            return;
        const generation = epoch;
        if (action !== "interrupt") {
            const message = action === "review" ? "请先检查对话历史和工作区文件。核对后可发送新任务；原消息不会重新执行，也不保证已脱离连接的进程停止。" : "放弃会阻止这条尚未接收的消息以后执行。已接收的任务不会被取消；请先查询并核对结果。";
            if (!await confirm(message))
                return;
        }
        if (!guard() || !current(row.session, generation))
            return;
        const id = key(row.session, row.id);
        busy.add(id);
        render();
        try {
            const found = await query(row);
            if (!guard() || !current(row.session, generation))
                return;
            if (action === "abandon" && found) {
                say("消息已经接收，请按当前状态处理。", true);
                return;
            }
            if (action !== "abandon" && !found) {
                say("尚未查到该消息的接收记录", true);
                return;
            }
            const result = await api(endpoint(row.session, row.id), { method: "POST", body: JSON.stringify({ action, scope, revision: found?.revision }), signal: AbortSignal.any([lifetime.signal, AbortSignal.timeout(15000)]) });
            if (!owns())
                return;
            response(result, row.session, row.id);
            if (current(row.session, generation)) {
                notice = action === "interrupt" ? "已请求中断，正在等待任务结束确认。" : "";
                noticeError = false;
                noticeTranslated = false;
            }
            void refresh();
        }
        catch (error) {
            if (current(row.session, generation)) {
                notice = error instanceof Error ? error.message : t("无法确认消息结果，已保留待确认内容。请重新查询。");
                noticeError = true;
                noticeTranslated = true;
            }
        }
        finally {
            busy.delete(id);
            if (owns())
                render();
        }
    }
    function render() {
        if (!owns())
            return;
        const root = $("chat-delivery"), list = $("chat-delivery-list"), status = $("chat-delivery-status");
        root.classList.toggle("hidden", !context);
        if (!context)
            return;
        const items = rows();
        const highlighted = items.find(r => r.state === "unconfirmed" || r.state === "uncertain") || items.find(r => receiptActive(r.state)) || items[0];
        $("chat-delivery-toggle").disabled = !items.length;
        if (!items.length)
            showDetails(false);
        setTextRender($("chat-delivery-count"), () => t("{count} 条消息", { count: items.length }));
        $("chat-delivery-count").classList.toggle("hidden", items.length < 2);
        root.classList.toggle("hidden", enabled && !items.length && !loading && !queryFailed && !scopeChanged && !notice && !storage.error);
        root.classList.toggle("delivery-warning", queryFailed || scopeChanged || items.some(r => r.state === "unconfirmed" || r.state === "uncertain"));
        setTextRender(status, () => unsupported ? t("当前网页不支持此服务端的消息协议，请刷新页面或升级客户端。") : !enabled ? t("此服务端使用旧发送方式，无法按消息编号确认接收结果。") : !scope || scopeChanged ? t("消息所属的实例或登录身份已变化") : notice ? (noticeTranslated ? notice : t(notice)) : storage.error ? t("本机待确认副本保存失败，请保留本页并检查存储设置。") : loading ? t("正在查询消息接收状态…") : items.some(r => r.state === "unconfirmed") ? t("发送确认未收到，内容已保留。请先查询结果。") : !storage.enabled ? t("本机保存已关闭；待确认内容仅保留在当前页面。") : highlighted ? t(labels[highlighted.state]) + (highlighted.thread !== context?.thread ? " · " + t("另一对话的消息") : "") : t("消息确认与恢复"));
        status.classList.toggle("error", noticeError || storage.error);
        $("chat-delivery-refresh").disabled = !enabled || loading || refreshing;
        const signature = JSON.stringify([context, items.map(row => [row, known.get(key(row.session, row.id))?.request?.text, busy.has(key(row.session, row.id))])]);
        if (signature === listSignature) {
            hooks.changed();
            return;
        }
        listSignature = signature;
        const prior = new Map([...list.querySelectorAll(".delivery-item")].map(item => [item.dataset.requestId, { open: item.open, state: item.dataset.state }]));
        const focused = document.activeElement instanceof HTMLElement && list.contains(document.activeElement) ? document.activeElement : undefined;
        const focusID = focused?.closest(".delivery-item")?.dataset.requestId, focusAction = focused?.dataset.deliveryAction;
        list.replaceChildren();
        for (const row of items) {
            const id = key(row.session, row.id), c = known.get(id), working = busy.has(id);
            const entry = document.createElement("details");
            entry.className = "delivery-item";
            entry.dataset.requestId = row.id;
            entry.dataset.state = row.state;
            const previous = prior.get(row.id);
            entry.open = previous?.open ?? items.length === 1;
            const summary = document.createElement("summary"), label = document.createElement("span");
            label.className = "delivery-label";
            setTextRender(label, () => t(labels[row.state]) + (row.thread !== context?.thread ? " · " + t("另一对话的消息") : ""));
            summary.append(label);
            const preview = document.createElement("span");
            preview.className = "delivery-preview";
            preview.textContent = row.draft?.text || row.input?.text || c?.request?.text || "";
            summary.append(preview);
            entry.append(summary);
            const body = document.createElement("pre");
            body.textContent = row.draft?.text || row.input?.text || c?.request?.text || t("待确认正文已过期；仍可查询结果或放弃原消息编号。");
            entry.append(body);
            const frozen = row.input || c?.request;
            if (frozen) {
                const settings = document.createElement("p");
                settings.className = "delivery-settings";
                setTextRender(settings, () => t("模型：{model}；推理强度：{effort}", { model: frozen.model, effort: frozen.effort || t("默认") }));
                entry.append(settings);
            }
            if (row.draft?.attachments.length) {
                const files = document.createElement("p");
                files.className = "delivery-settings";
                files.textContent = row.draft.attachments.map(a => a.orig || a.name).join(" · ");
                entry.append(files);
            }
            const actions = document.createElement("div");
            actions.className = "delivery-actions";
            const button = (text, icon, run, disabled = false) => { const b = document.createElement("button"); b.type = "button"; b.className = "btn btn-sm"; b.dataset.deliveryAction = text; actionButton(b, () => t(text), icon); b.disabled = working || disabled; b.addEventListener("click", () => { if (owns())
                run(); }); actions.append(b); };
            if (row.state === "unconfirmed") {
                button("重试原消息", "send", () => void retry(row), !row.input || row.thread !== context.thread);
                button("放弃未接收消息", "close", () => void act(row, "abandon"));
            }
            if (row.state === "uncertain")
                button("已检查结果，继续", "check", () => void act(row, "review"));
            if (["accepted", "starting", "running"].includes(row.state))
                button("中断", "stop", () => void act(row, "interrupt"));
            if (["failed", "interrupted", "reviewed", "abandoned", "deleted"].includes(row.state))
                button("复制回输入框", "copy", () => void (async () => {
                    if (context?.session !== row.session)
                        return;
                    const generation = epoch;
                    if (context.thread !== row.thread && !await confirm("这条消息属于另一对话。复制到当前对话的输入框？不会自动发送，也不会覆盖已有草稿。"))
                        return;
                    if (!current(row.session, generation))
                        return;
                    const frozen = row.input || c?.request;
                    const draft = row.draft || (frozen ? draftFromRequest(frozen) : undefined);
                    if (draft)
                        hooks.restore(structuredClone(draft));
                })(), !row.draft && !row.input && !c?.request);
            if (row.state !== "unconfirmed" && !receiptActive(row.state))
                button("关闭状态", "close", () => { dismissed.add(id); storage.remove(row.id, row.session); views.delete(id); known.delete(id); render(); });
            entry.append(actions);
            list.append(entry);
        }
        if (focused && keyboardInput()) {
            const row = [...list.querySelectorAll(".delivery-item")].find(item => item.dataset.requestId === focusID);
            const button = [...row?.querySelectorAll("button") || []].find(b => b.dataset.deliveryAction === focusAction && !b.disabled);
            (button || row?.querySelector("summary") || $("chat-input")).focus();
        }
        hooks.changed();
    }
    $("chat-delivery-toggle").addEventListener("click", () => showDetails(!expanded), { signal: lifetime.signal });
    $("chat-delivery-list").addEventListener("keydown", event => { if (event.key === "Escape") {
        event.stopPropagation();
        showDetails(false);
        $("chat-delivery-toggle").focus();
    } }, { signal: lifetime.signal });
    $("chat-delivery-refresh").addEventListener("click", () => void refresh(), { signal: lifetime.signal });
    window.addEventListener("online", () => void refresh(), { signal: lifetime.signal });
    window.addEventListener("focus", () => void refresh(), { signal: lifetime.signal });
    window.addEventListener("storage", event => {
        if (!owns())
            return;
        if (event.key?.startsWith("agentbox.chat-outbox.v1.") || event.key?.startsWith("agentbox.chat-draft.v1.")) {
            if (!storage.enabled)
                storage.clearPersistent();
            render();
            void refresh();
        }
    }, { signal: lifetime.signal });
    window.addEventListener("chat-local-saving-changed", event => { storage.setEnabled(event.detail.enabled); render(); }, { signal: lifetime.signal });
    document.addEventListener("visibilitychange", () => { if (!document.hidden)
        void refresh(); }, { signal: lifetime.signal });
    const timer = setInterval(() => void refresh(), 4000);
    const closeConfirmation = () => { if (confirmation) {
        const dialog = $("dlg-ask");
        if (dialog.open)
            dialog.close("");
    } };
    const leave = () => { showDetails(false); ++epoch; closeConfirmation(); viewAbort.abort(); viewAbort = new AbortController(); context = undefined; loading = false; queryFailed = false; notice = ""; socketState = undefined; $("chat-delivery").classList.add("hidden"); };
    return {
        enabled, unsupported, blocked,
        socketStatus(state) { socketState = state === "running" ? "running" : "idle"; },
        executionState() {
            if (!owns() || !enabled || loading)
                return;
            const items = rows();
            if (items.some(r => ["accepted", "starting", "running"].includes(r.state)))
                return "running";
            if (items.some(r => r.state === "unconfirmed"))
                return;
            if (items.some(r => r.state === "uncertain" && known.has(key(r.session, r.id))))
                return "idle";
            // A terminal receipt from a prior turn must not erase a newer legacy WS
            // turn's running status. With a live socket its latest status remains valid.
            if ((!hooks.connected() || socketState === "idle") && items.some(r => known.has(key(r.session, r.id)) && !receiptActive(r.state)))
                return "idle";
        },
        enter(session, thread) { if (context?.session !== session || context.thread !== thread) {
            leave();
            context = { session, thread };
            loading = enabled;
            render();
        } void refresh(); },
        leave, refresh,
        message(c) { if (!owns() || !context || !enabled)
            return; try {
            const value = receipt(c, context.session);
            apply(value);
            render();
        }
        catch {
            void refresh();
        } },
        interrupt() { if (!owns())
            return true; const row = rows().find(r => ["accepted", "starting", "running"].includes(r.state)); if (row) {
            void act(row, "interrupt");
            return true;
        } return false; },
        async send(input, draft, unchanged) {
            if (!enabled || !guard() || !context)
                return false;
            const { session, thread } = context, generation = epoch;
            const prepare = async () => {
                if (!current(session, generation) || thread !== context?.thread || !unchanged() || blocked()) {
                    render();
                    return false;
                }
                const row = { version: 1, id: identifier(), session, thread, created: Date.now(), input: structuredClone(input), draft: structuredClone(draft), revision: 0, state: "unconfirmed" };
                if (!storage.add(row)) {
                    say("待确认副本未保存，暂未发送。请检查本机存储，或关闭本机保存后重试。", true);
                    return false;
                }
                for (const [id, c] of known)
                    if (c.state === "completed") {
                        known.delete(id);
                        views.delete(id);
                    }
                views.set(key(session, row.id), row);
                showDetails(false);
                hooks.move(draft);
                render();
                await transmit(row);
                return true;
            };
            if (navigator.locks)
                return navigator.locks.request("agentbox-chat-send-" + scope + "-" + session, { ifAvailable: true }, lock => lock ? prepare() : false);
            return prepare();
        },
        dispose() { if (!active)
            return; if (S.token !== owner) {
            if (S.preserveChatCopiesOnSignout)
                storage.forget();
            else
                storage.clear();
        } active = false; closeConfirmation(); lifetime.abort(); viewAbort.abort(); clearInterval(timer); context = undefined; known.clear(); views.clear(); $("chat-delivery-list").replaceChildren(); $("chat-delivery").classList.add("hidden"); },
    };
}
