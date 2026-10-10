import { htmlText as trHTML, setText, setTextRender, t as i18nText } from "./i18n.js";
import { createGitSurface } from "./git-surface.js";
import { api } from "./api.js";
import { S, bus } from "./state.js";
import { fmtBytes, fmtTime } from "./util.js";
import { actionButton } from "./icons.js";
import { ProgressBar, RateMeter } from "./progress.js";
import { skelRows } from "./skeleton.js";
const phaseNames = { get preparing() { return i18nText("准备与检查"); }, get transferring() { return i18nText("传输数据"); }, get checking() { return i18nText("核对工作区"); }, get merging() { return i18nText("快进合并"); }, get checkout() { return i18nText("检出文件"); }, get publishing() { return i18nText("发布仓库目录"); } };
const names = { get "review.list"() { return i18nText("查询 PR/MR"); }, get "review.preview"() { return i18nText("预览 PR/MR"); }, get "review.create"() { return i18nText("创建 PR/MR"); }, get "connection.share"() { return i18nText("共享授权"); }, get "remote.add"() { return i18nText("添加远程"); }, get "remote.update"() { return i18nText("修改远程"); }, get "remote.remove"() { return i18nText("删除远程"); }, get commit() { return i18nText("本地提交"); }, get discard() { return i18nText("丢弃改动"); }, get "oauth.revoke"() { return i18nText("撤销 OAuth"); }, get fetch() { return i18nText("获取"); }, get pull() { return i18nText("快进拉取"); }, get push() { return i18nText("推送"); }, get "push-preview"() { return i18nText("推送预览"); }, get clone() { return i18nText("克隆"); }, get "connection.test"() { return i18nText("测试读取"); }, get "connection.create"() { return i18nText("添加连接"); }, get "connection.update"() { return i18nText("编辑连接"); }, get "connection.delete"() { return i18nText("删除连接"); }, get "binding.update"() { return i18nText("修改绑定"); }, get "default.update"() { return i18nText("修改默认连接"); } };
const results = { get success() { return i18nText("成功"); }, get running() { return i18nText("执行中"); }, get failed() { return i18nText("失败"); }, get failed_unknown() { return i18nText("未确认，请核对远程"); }, get cancelled_unknown() { return i18nText("已中断，请核对最终状态"); }, get interrupted_unknown() { return i18nText("服务曾中断，请核对最终状态"); }, get success_binding_failed() { return i18nText("克隆成功，绑定未保存"); } };
/* Git --progress 的阶段名（服务端只放行这几个键）。remote_ 开头的是远端经边带转述的进度 */
const stageNames = { get counting() { return i18nText("统计对象"); }, get compressing() { return i18nText("压缩对象"); }, get receiving() { return i18nText("接收对象"); }, get writing() { return i18nText("上传对象"); }, get resolving() { return i18nText("处理差异"); }, get updating() { return i18nText("更新文件"); }, get connectivity() { return i18nText("检查完整性"); },
    get remote_counting() { return i18nText("远程统计对象"); }, get remote_compressing() { return i18nText("远程压缩对象"); }, get remote_resolving() { return i18nText("远程处理差异"); } };
/* 各阶段在整条进度里占的区间：获取/拉取/克隆的大头是接收对象，推送的大头是上传对象。
 * 阶段可能被跳过（小仓库远端不压缩），整条只取最大值，不往回退。 */
const stageRange = { remote_counting: [0, .04], remote_compressing: [.04, .12], receiving: [.12, .88], resolving: [.88, .97], updating: [.97, .99], connectivity: [.97, .99],
    counting: [0, .04], compressing: [.04, .16], writing: [.16, .92], remote_resolving: [.92, .99] };
const afterTransfer = new Set(["checking", "merging", "checkout", "publishing"]);
const sends = (op) => op.operation === "push" || op.operation.endsWith(".push");
function progress(op) {
    const step = op.cancel_requested ? i18nText("正在取消") : op.stage && !afterTransfer.has(op.phase) ? stageNames[op.stage] || op.stage : phaseNames[op.phase] || op.phase;
    const count = op.stage && !afterTransfer.has(op.phase) && op.stage_total ? ` ${op.stage_done}/${op.stage_total}` : "";
    return `${names[op.operation] || op.operation} · ${step}${count}`;
}
/* 整条进度：还没有 Git 计数时返回 null（画不确定进度） */
function overall(op, floor) {
    if (afterTransfer.has(op.phase))
        return Math.max(floor, .97);
    const range = op.stage ? stageRange[op.stage] : undefined;
    if (!range || op.phase !== "transferring")
        return floor || null;
    return Math.max(floor, range[0] + (range[1] - range[0]) * Math.min(100, op.stage_percent || 0) / 100);
}
/** 一次 Git 操作的进度块：标题行（操作 · 阶段 + 百分比）、进度条、字节与速率、取消钮。
 * 弹窗底部的在途请求与「操作记录」里的活动操作共用。 */
export function progressView(op, cancellable = true) {
    const box = document.createElement("div");
    box.className = "git-operation-progress";
    const label = document.createElement("p");
    label.setAttribute("role", "status");
    const pct = document.createElement("span");
    pct.className = "git-op-pct mono";
    const bar = new ProgressBar(() => i18nText("Git 操作进度"));
    const meta = document.createElement("span");
    meta.className = "git-op-meta";
    const cancel = document.createElement("button");
    cancel.type = "button";
    cancel.className = "btn btn-sm btn-danger";
    actionButton(cancel, () => i18nText("取消"), "close", () => i18nText("取消 Git 操作"));
    box.append(label, pct, bar.el, meta);
    if (cancellable)
        box.append(cancel);
    const rate = new RateMeter();
    let floor = 0;
    const update = (op) => {
        label.textContent = progress(op);
        const value = overall(op, floor);
        if (value !== null)
            floor = value;
        bar.set(value);
        pct.textContent = value === null ? "" : Math.floor(value * 100) + "%";
        const primary = sends(op) ? op.sent_bytes : op.received_bytes, speed = rate.sample(primary);
        const parts = [];
        if (op.received_bytes)
            parts.push(i18nText("接收 {p0}", { p0: fmtBytes(op.received_bytes) }));
        if (op.sent_bytes)
            parts.push(i18nText("发送 {p0}", { p0: fmtBytes(op.sent_bytes) }));
        if (speed > 0 && op.phase === "transferring")
            parts.push(fmtBytes(Math.round(speed)) + "/s");
        parts.push(i18nText("{p0} 秒", { p0: String(Math.floor(op.elapsed_ms / 1000)) }));
        meta.textContent = parts.join(" · ");
    };
    if (op)
        update(op);
    return { box, label, cancel, meta, bar, update };
}
function requestID() {
    const bytes = crypto.getRandomValues(new Uint8Array(20));
    return [...bytes].map(b => b.toString(16).padStart(2, "0")).join("");
}
/** Run a request with a separate cancellation handle. Do not abort the HTTP
 * response: it carries the final outcome, which can still be success if remote
 * accepted a push just before cancellation. Poll only while this request lives. */
export async function gitRequest(path, body, host) {
    const id = requestID(), token = S.token;
    const view = progressView(), { box, label, cancel, meta } = view;
    setText(label, "准备 Git 操作…");
    cancel.disabled = true;
    // 弹窗里挂在正文下方（固定不随正文滚动），页内面板直接接在末尾
    host.append(box);
    let ended = false, requested = false, timer;
    const cancelRequest = async () => {
        if (ended || token !== S.token)
            return;
        cancel.disabled = true;
        try {
            await api(`/git/operations/${id}/cancel`, { method: "POST" });
            requested = true;
            setText(label, "正在取消并等待收尾；已到达远程的提交不会回滚。");
        }
        catch (e) {
            if (!ended) {
                meta.textContent = e.message;
                cancel.disabled = false;
            }
        }
    };
    cancel.addEventListener("click", () => void cancelRequest());
    const poll = async () => {
        try {
            const page = await api("/git/operations", { signal: AbortSignal.timeout(5000) });
            if (ended || token !== S.token)
                return;
            const op = page.active.find(op => op.request_id === id);
            if (op) {
                if (!requested)
                    view.update(op);
                cancel.disabled = requested || op.cancel_requested;
            }
        }
        catch { /* Keep the operation response authoritative if status polling fails. */ }
        if (!ended && token === S.token)
            timer = setTimeout(() => void poll(), 700);
    };
    timer = setTimeout(() => void poll(), 150);
    try {
        return await api(path, { method: "POST", headers: { "X-Git-Request-ID": id }, body: JSON.stringify(body) });
    }
    catch (e) {
        if (requested)
            throw new Error(i18nText("操作已请求取消；请刷新操作记录与远程状态确认最终结果。"));
        throw e;
    }
    finally {
        ended = true;
        clearTimeout(timer);
        box.remove();
    }
}
let activeDialog = null;
export function openGitOperations() {
    if (activeDialog)
        return;
    const token = S.token, d = createGitSurface(() => i18nText("Git 操作记录"), `
    <p class="field-hint"><span data-i18n="仅显示你发起的操作。中断或未确认不代表远程回滚，需核对仓库状态。活动操作每秒刷新。">${trHTML("仅显示你发起的操作。中断或未确认不代表远程回滚，需核对仓库状态。活动操作每秒刷新。")}</span></p>
    <p data-error role="alert" class="login-error"></p><div data-active></div><div data-history></div>
    <div class="dlg-actions"><button class="btn btn-sm" data-refresh data-icon="refresh" data-tip="${trHTML("回到最新 Git 操作记录")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;回到最新 Git 操作记录&quot;}"><span class="action-label" data-i18n="最新">${trHTML("最新")}</span></button><button class="btn btn-sm" data-more disabled data-icon="arrow-left" data-tip="${trHTML("查看更早的 Git 操作记录")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;查看更早的 Git 操作记录&quot;}"><span class="action-label" data-i18n="更早">${trHTML("更早")}</span></button></div>`);
    activeDialog = d;
    d.id = "dlg-git-operations";
    const history = d.querySelector("[data-history]"), live = d.querySelector("[data-active]"), error = d.querySelector("[data-error]");
    const more = d.querySelector("[data-more]");
    let next = 0, generation = 0, timer;
    async function load(before = 0) {
        const gen = ++generation;
        more.disabled = true;
        try {
            const page = await api("/git/operations" + (before ? `?before=${before}` : ""), { signal: AbortSignal.timeout(8000) });
            if (!d.open || token !== S.token || gen !== generation)
                return;
            error.textContent = "";
            history.replaceChildren();
            next = page.next_before;
            for (const op of page.rows) {
                const row = document.createElement("div");
                row.className = "git-connection-row";
                const title = document.createElement("strong");
                title.textContent = `${names[op.operation] || op.operation} · ${results[op.result] || op.result}`;
                const info = document.createElement("p");
                info.className = "field-hint";
                setTextRender(info, () => `${fmtTime(Date.parse(op.started_at))} · ${op.session_id || i18nText("个人连接")}${op.repo ? " / " + op.repo : ""}\n${op.target || ""}`);
                row.append(title, info);
                history.append(row);
            }
            if (!page.rows.length)
                setText(history, "暂无 Git 操作记录。");
            renderActive(page.active);
            more.disabled = !next;
        }
        catch (e) {
            if (d.open && gen === generation) {
                error.textContent = e.message;
                more.disabled = !next;
                history.querySelector(".skeleton-list")?.remove();
            }
        }
    }
    // 活动操作按请求 ID 保留各自的进度块：每秒刷新只更新数字，进度条不重播、速率能接着算
    const views = new Map();
    function renderActive(operations) {
        for (const [id, view] of views)
            if (!operations.some(op => op.request_id === id)) {
                view.box.remove();
                views.delete(id);
            }
        for (const op of operations) {
            let view = views.get(op.request_id);
            if (!view) {
                const created = progressView(op);
                view = created;
                views.set(op.request_id, created);
                created.cancel.addEventListener("click", async () => { created.cancel.disabled = true; try {
                    await api(`/git/operations/${op.request_id}/cancel`, { method: "POST" });
                }
                catch (e) {
                    error.textContent = e.message;
                } });
            }
            else
                view.update(op);
            view.cancel.disabled = op.cancel_requested;
            live.append(view.box);
        }
    }
    async function poll() {
        try {
            const page = await api("/git/operations", { signal: AbortSignal.timeout(5000) });
            if (d.open && token === S.token)
                renderActive(page.active);
        }
        catch { }
        if (d.open && token === S.token)
            timer = setTimeout(() => void poll(), 1000);
    }
    d.querySelector("[data-refresh]").addEventListener("click", () => void load());
    more.addEventListener("click", () => void load(next));
    d.addEventListener("close", () => { clearTimeout(timer); generation++; d.remove(); activeDialog = null; });
    history.append(skelRows(4, i18nText("读取中…"), { rowClass: "git-connection-row", lines: 2, title: [22, 20] }));
    void load();
    timer = setTimeout(() => void poll(), 1000);
}
bus.addEventListener("signed-out", () => activeDialog?.close());
