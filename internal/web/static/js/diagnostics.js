import { api, wsURL } from "./api.js";
import { skelBar } from "./skeleton.js";
import { S, bus } from "./state.js";
import { t, setText, setTextRender } from "./i18n.js";
import { actionButton } from "./icons.js";
import { diagnosticMessages } from "./diagnostic-messages.js";
const labels = {
    configuration: "配置", docker: "Docker 服务", agent_image: "指定镜像", data_disk: "数据盘空间",
    data_permissions: "服务端写入权限", container_ownership: "容器属主设置", account_configuration: "账号配置",
    account_access: "账号授权", quota: "额度准入", workspace_permissions: "空间目录权限", websocket: "WebSocket（浏览器）", model: "模型调用",
};
const sessionChecks = new Set(["account_access", "docker", "agent_image", "data_disk", "workspace_permissions", "account_configuration", "quota", "websocket", "model"]);
const states = { passed: "通过", failed: "失败", not_checked: "未检查" };
const validID = (value) => typeof value === "string" && /^[a-f0-9]{32}$/.test(value);
function safeCheck(check) {
    let code = typeof check.code === "string" && Object.hasOwn(diagnosticMessages, check.code) ? check.code : "not_requested";
    let state = typeof check.state === "string" && Object.hasOwn(states, check.state) && code === check.code ? check.state : "not_checked";
    // This protocol version never calls a model, regardless of other green rows.
    if (check.id === "model") {
        code = "model_not_checked";
        state = "not_checked";
    }
    const [message, hint] = diagnosticMessages[code];
    return { id: check.id, state, code, message, hint };
}
// Export only the known report fields. Names, paths, arbitrary backend fields
// and raw error messages must not accidentally become part of a shared file.
export function cleanDiagnosticReport(value, scope) {
    if (value.version !== 1 || value.scope !== scope || !Array.isArray(value.checks) || !Number.isFinite(value.checked_at)) {
        throw new Error(t("诊断协议不兼容，请更新服务端后重试。"));
    }
    return { version: 1, scope, checked_at: value.checked_at, operation_id: validID(value.operation_id) ? value.operation_id : undefined,
        checks: value.checks.filter(row => row && typeof row.id === "string" && Object.hasOwn(labels, row.id) && (scope !== "session" || sessionChecks.has(row.id))).map(safeCheck) };
}
function probeSocket(path, signal) {
    const id = Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, "0")).join("");
    return new Promise(resolve => {
        let socket;
        let done = false;
        const finish = (state, code) => {
            if (done)
                return;
            done = true;
            clearTimeout(timer);
            signal.removeEventListener("abort", abort);
            socket?.close(1000);
            const [message, hint] = diagnosticMessages[code];
            resolve({ id: "websocket", state, code, message, hint, source: "browser", checked_at: Date.now(), operation_id: id });
        };
        const abort = () => finish("not_checked", "check_cancelled");
        const timer = setTimeout(() => finish("failed", "websocket_failed"), 5000);
        signal.addEventListener("abort", abort, { once: true });
        if (signal.aborted) {
            abort();
            return;
        }
        try {
            socket = new WebSocket(wsURL(path) + "&connection_id=" + id);
        }
        catch {
            finish("failed", "websocket_failed");
            return;
        }
        socket.onmessage = event => {
            try {
                const result = JSON.parse(event.data);
                if (result.type === "diagnostic" && result.id === "websocket" && result.state === "passed" && result.code === "websocket_ok" && result.operation_id === id)
                    finish("passed", "websocket_ok");
                else
                    finish("failed", "websocket_failed");
            }
            catch {
                finish("failed", "websocket_failed");
            }
        };
        socket.onerror = () => finish("failed", "websocket_failed");
        socket.onclose = () => finish("failed", "websocket_failed");
    });
}
let active;
export function openDiagnostics(sessionID, onComplete) {
    active?.close();
    const scope = sessionID ? "session" : "instance";
    const path = sessionID ? `/sessions/${encodeURIComponent(sessionID)}/diagnostics` : "/diagnostics";
    const lifetime = new AbortController();
    const dialog = document.createElement("dialog");
    dialog.className = "diagnostics-dialog";
    active = dialog;
    const title = document.createElement("h2");
    title.id = "diagnostic-title";
    dialog.setAttribute("aria-labelledby", title.id);
    setText(title, sessionID ? "空间环境检查" : "实例环境检查");
    const note = document.createElement("p");
    note.className = "muted";
    setText(note, sessionID ? "只检查当前空间，不运行模型或修改项目文件。" : "检查当前实例；写入探测仅创建并清理独立临时文件，不运行模型。");
    const status = document.createElement("p");
    status.setAttribute("role", "status");
    const list = document.createElement("ol");
    list.className = "diagnostic-checks";
    const error = document.createElement("p");
    error.setAttribute("role", "alert");
    const reference = document.createElement("p");
    reference.className = "note diagnostic-reference";
    const actions = document.createElement("div");
    actions.className = "dlg-actions";
    const run = document.createElement("button"), download = document.createElement("button"), close = document.createElement("button");
    for (const button of [run, download, close]) {
        button.type = "button";
        button.className = "btn btn-sm";
    }
    actionButton(run, () => t("重新检查"), "refresh");
    actionButton(download, () => t("导出诊断"), "download");
    actionButton(close, () => t("关闭"), "close");
    download.disabled = true;
    actions.append(run, download, close);
    const body = document.createElement("div");
    body.className = "dlg-body";
    body.append(note, status, list, error, reference);
    const head = document.createElement("div");
    head.className = "dlg-head";
    head.append(title);
    dialog.append(head, body, actions);
    let request;
    let report;
    let client;
    let generation = 0;
    const render = () => {
        list.replaceChildren();
        if (!report)
            return;
        for (const row of report.checks) {
            const check = row.id === "websocket" && client ? client : row;
            const li = document.createElement("li");
            li.dataset.check = check.id;
            const heading = document.createElement("div");
            heading.className = "diagnostic-heading";
            const label = document.createElement("strong");
            setText(label, labels[check.id]);
            const state = document.createElement("span");
            state.dataset.state = check.state;
            setText(state, states[check.state]);
            heading.append(label, state);
            const message = document.createElement("p"), hint = document.createElement("p");
            hint.className = "note";
            setText(message, check.message);
            setText(hint, check.hint);
            li.append(heading, message, hint);
            if (check === client) {
                const connectionID = client.operation_id;
                const ref = document.createElement("p");
                ref.className = "note diagnostic-reference";
                setTextRender(ref, () => t("操作编号：{id}", { id: connectionID }));
                li.append(ref);
            }
            list.append(li);
        }
        setTextRender(reference, () => report?.operation_id ? t("操作编号：{id}", { id: report.operation_id }) : "");
    };
    const checksSkeleton = (count) => Array.from({ length: count }, (_, i) => {
        const li = document.createElement("li");
        li.className = "skeleton-row";
        li.setAttribute("aria-hidden", "true");
        const heading = document.createElement("div");
        heading.className = "diagnostic-heading";
        heading.append(skelBar(25 + (i * 13) % 30, "text"), skelBar("4em", "text"));
        li.append(heading, skelBar(50 + (i * 17) % 40, "text"), skelBar(35 + (i * 11) % 40, "text note"));
        return li;
    });
    const check = async () => {
        request?.abort();
        const controller = request = new AbortController();
        const epoch = ++generation;
        const timeout = setTimeout(() => controller.abort(), 18000);
        run.disabled = download.disabled = true;
        error.textContent = "";
        report = undefined;
        client = undefined;
        reference.textContent = "";
        // 检查期间铺与上次同样多的骨架项，重新检查时弹窗不先缩下去
        list.replaceChildren(...checksSkeleton(Math.min(12, list.querySelectorAll("li").length || 8)));
        setText(status, "正在检查环境…");
        try {
            const result = await api(path, { method: "POST", signal: controller.signal });
            if (!dialog.open || epoch !== generation || controller.signal.aborted)
                return;
            report = cleanDiagnosticReport(result, scope);
            render();
            setText(status, "正在检查浏览器连接…");
            client = await probeSocket(path + "/ws", controller.signal);
            if (!dialog.open || epoch !== generation)
                return;
            if (controller.signal.aborted)
                throw new DOMException("cancelled", "AbortError");
            render();
            setText(status, "检查完成；请分别查看通过、失败和未检查的项目。");
            download.disabled = false;
            onComplete?.(report, client);
        }
        catch (reason) {
            if (dialog.open && epoch === generation) {
                setText(status, "检查未完成");
                if (!report)
                    list.replaceChildren(); // 一项结果都没拿到：收起骨架
                error.textContent = controller.signal.aborted ? t("检查已取消或超时，请重试。") : String(reason.message);
            }
        }
        finally {
            clearTimeout(timeout);
            if (dialog.open && epoch === generation)
                run.disabled = false;
        }
    };
    run.addEventListener("click", () => void check());
    close.addEventListener("click", () => dialog.close());
    download.addEventListener("click", () => {
        if (!report || download.disabled)
            return;
        const blob = new Blob([JSON.stringify({ ...report, client_checks: client ? [client] : [] }, null, 2)], { type: "application/json" });
        const url = URL.createObjectURL(blob), link = document.createElement("a");
        link.href = url;
        link.download = "agentbox-diagnostics.json";
        link.click();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
    });
    const leave = () => dialog.close();
    bus.addEventListener("signed-out", leave, { signal: lifetime.signal });
    bus.addEventListener("view-changed", leave, { signal: lifetime.signal });
    bus.addEventListener("navigation-changed", () => { if (sessionID && S.current?.id !== sessionID)
        leave(); }, { signal: lifetime.signal });
    dialog.addEventListener("close", () => { ++generation; request?.abort(); lifetime.abort(); dialog.remove(); if (active === dialog)
        active = undefined; }, { once: true });
    document.body.append(dialog);
    dialog.showModal();
    close.focus();
    void check();
}
