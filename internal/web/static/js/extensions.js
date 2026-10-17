import { pickLocal, setAttrRender, setTextRender, t as i18nText } from "./i18n.js";
import { api } from "./api.js";
import { APIError } from "./problems.js";
import { S, bus } from "./state.js";
import { $, fmtDateTime, toast } from "./util.js";
import { showView } from "./shell.js";
import { buttonLabel } from "./icons.js";
import { enhanceSelects } from "./select.js";
/* extensions：管理员的「扩展」页（docs/architecture/extensions-spi.md）。
 * 列表 → 详情（概览与权限、设置、事件、日志）。扩展提供的名称、说明、错误文案
 * 都是服务端校验过的纯文本，这里一律当文本节点插入，扩展不能带自己的脚本或样式。
 * 停留在本页时每 5 秒刷新状态；表单改动未保存时不覆盖输入。 */
const PERMISSIONS = {
    "model.stream.text": { label: () => i18nText("读取模型回复文字"), risk: () => i18nText("能读到模型回复与推理的文字增量，可能包含代码或密钥片段。") },
    "model.request.fingerprint": { label: () => i18nText("请求指纹"), risk: () => i18nText("只能判断两次请求是否相同，拿不到提示词原文。") },
    "model.context": { label: () => i18nText("空间与用户标识"), risk: () => i18nText("能把流量关联到具体空间、账号和用户。") },
};
const HOOK_PERMISSION = { "model.request": "model.request.fingerprint", "model.stream": "model.stream.text" };
const STATES = {
    disabled: () => i18nText("未启用"), invalid: () => i18nText("配置无效"), starting: () => i18nText("启动中"),
    running: () => i18nText("运行中"), degraded: () => i18nText("降级"), restarting: () => i18nText("重启中"),
    failed: () => i18nText("已失败"), stopped: () => i18nText("已停止"),
};
const KINDS = {
    abort: () => i18nText("中断回复"), reject: () => i18nText("拒绝请求"),
    unavailable: () => i18nText("扩展不可用"), overloaded: () => i18nText("检查跟不上"),
};
function el(tag, cls = "", text) {
    const node = document.createElement(tag);
    if (cls)
        node.className = cls;
    if (typeof text === "function")
        setTextRender(node, text);
    else if (text !== undefined)
        node.textContent = text;
    return node;
}
const local = (text) => () => pickLocal(text);
function stateChip(state) {
    const chip = el("span", "ext-state", STATES[state] || (() => state));
    chip.dataset.state = state;
    return chip;
}
function bindingSummary(ext) {
    const b = ext.setting.bindings;
    if (b.mode === "all")
        return i18nText("全部 API Key / 中转账号");
    if (b.mode === "accounts" && b.accounts?.length)
        return i18nText("{n} 个账号", { n: String(b.accounts.length) });
    return i18nText("未绑定账号");
}
/** App-owned lifetime: logout aborts requests and timers and clears the page. */
export function initExtensionsPage() {
    const btn = $("btn-extensions");
    const admin = S.role === "admin";
    btn.classList.toggle("hidden", !admin);
    if (!admin)
        return () => { };
    const lifetime = new AbortController();
    const { signal } = lifetime;
    let data = null;
    let gen = 0;
    let dirty = false;
    let saving = false;
    let events = [];
    let logs = [];
    let logsFor = "";
    const onPage = () => S.view === "extensions";
    const selected = () => data?.extensions.find(e => e.id === S.extSel) || null;
    async function load(resetForm) {
        const mine = ++gen;
        try {
            const next = await api("/extensions", { signal });
            if (mine !== gen || signal.aborted)
                return;
            data = { extensions: (next.extensions || []).map(e => ({ ...e, requires: e.requires || [] })) };
        }
        catch (err) {
            if (signal.aborted || mine !== gen)
                return;
            if (!data)
                $("ext-list").replaceChildren(el("p", "ext-error", err.message));
            return;
        }
        renderList();
        const ext = selected();
        if (ext) {
            await loadActivity(ext.id, mine);
            if (mine !== gen)
                return;
        }
        if (resetForm || !dirty)
            renderDetail();
        else
            renderLive();
    }
    async function loadActivity(id, mine) {
        if (logsFor !== id) {
            logs = [];
            logsFor = id;
        }
        const since = logs.length ? logs[logs.length - 1].seq : 0;
        try {
            const [ev, lg] = await Promise.all([
                api(`/extensions/${encodeURIComponent(id)}/events`, { signal }),
                api(`/extensions/${encodeURIComponent(id)}/logs?since=${since}`, { signal }),
            ]);
            if (mine !== gen || signal.aborted)
                return;
            events = ev.events || [];
            logs = logs.concat(lg.logs || []).slice(-200);
        }
        catch { /* keep the previous activity on a transient failure */ }
    }
    function renderList() {
        const list = $("ext-list");
        list.classList.toggle("hidden", !!S.extSel);
        list.replaceChildren();
        for (const ext of data?.extensions || []) {
            const card = el("button", "card ext-card");
            card.type = "button";
            const head = el("div", "ext-card-head");
            head.append(el("b", "", local(ext.name)), stateChip(ext.status.state));
            const meta = el("div", "ext-meta", () => [ext.version, ext.publisher, ext.builtin ? i18nText("官方内置") : "", bindingSummary(ext)].filter(Boolean).join(" · "));
            card.append(head, el("p", "ext-desc", local(ext.description)), meta);
            card.addEventListener("click", () => openExtension(ext.id));
            list.append(card);
        }
        if (!data?.extensions.length)
            list.append(el("p", "field-hint", () => i18nText("还没有安装扩展。")));
    }
    /* ---- 详情 ---- */
    let form = null;
    let liveStatus = null;
    let liveActivity = null;
    function renderDetail() {
        const box = $("ext-detail");
        const ext = selected();
        box.classList.toggle("hidden", !ext);
        box.replaceChildren();
        form = null;
        dirty = false;
        if (!ext)
            return;
        const back = el("button", "btn btn-ghost btn-sm ext-back");
        back.type = "button";
        buttonLabel(back, () => i18nText("全部扩展"), "arrow-left");
        back.addEventListener("click", () => openExtension(""));
        box.append(back, overviewCard(ext), (liveStatus = el("div")), settingsCard(ext), (liveActivity = el("div")));
        renderLive();
        enhanceSelects(box);
    }
    function renderLive() {
        const ext = selected();
        if (!ext || !liveStatus || !liveActivity)
            return;
        liveStatus.replaceChildren(statusCard(ext));
        liveActivity.replaceChildren(eventsCard(), logsCard());
    }
    function overviewCard(ext) {
        const card = el("section", "card ext-overview");
        const head = el("div", "ext-card-head");
        head.append(el("h3", "", local(ext.name)), stateChip(ext.status.state));
        const meta = el("div", "ext-meta", () => [i18nText("版本 {v}", { v: ext.version }), ext.publisher, ext.builtin ? i18nText("官方内置") : "", ext.license || ""].filter(Boolean).join(" · "));
        card.append(head, meta, el("p", "ext-desc", local(ext.description)));
        if (ext.homepage) {
            const link = el("a", "ext-link", () => i18nText("说明文档"));
            link.href = ext.homepage;
            link.target = "_blank";
            link.rel = "noopener noreferrer";
            card.append(link);
        }
        if (ext.errors.length) {
            const list = el("ul", "ext-errors");
            for (const e of ext.errors) {
                const li = el("li");
                li.append(el("code", "", e.code), el("span", "", local(e.message)));
                list.append(li);
            }
            card.append(el("h4", "", () => i18nText("可能给出的中断原因")), list);
        }
        return card;
    }
    /* Infrastructure the hooks run on (today only the model gateway for
     * model.* hooks). Its settings live on the settings page. */
    function requirementLine(req) {
        const row = el("div", "ext-require");
        const line = el("p", "gw-state", () => req.active
            ? i18nText("依赖模型网关 {addr} · 运行中", { addr: req.bind })
            : req.error
                ? i18nText("依赖模型网关 {addr}，未运行：{error}", { addr: req.bind, error: req.error })
                : i18nText("依赖模型网关 {addr} · 启用后开始监听", { addr: req.bind }));
        line.dataset.state = req.active ? "on" : req.error ? "error" : "off";
        const link = el("a", "ext-link", () => i18nText("网关设置"));
        link.href = "#/settings/proxies";
        row.append(line, link);
        return row;
    }
    function statusCard(ext) {
        const st = ext.status;
        const card = el("section", "card ext-status");
        card.append(el("h3", "", () => i18nText("运行状态")));
        for (const req of ext.requires)
            if (req.id === "model_gateway")
                card.append(requirementLine(req));
        const grid = el("dl", "ext-stats");
        const stat = (label, value) => { grid.append(el("dt", "", label), el("dd", "mono", value)); };
        stat(() => i18nText("状态"), STATES[st.state]?.() || st.state);
        stat(() => i18nText("检查中的流"), String(st.active_streams));
        stat(() => i18nText("重启次数"), String(st.restarts));
        if (st.started_at)
            stat(() => i18nText("启动时间"), fmtDateTime(st.started_at));
        for (const m of ext.metrics)
            stat(local(m.title), String(st.metrics?.[m.name] ?? 0));
        card.append(grid);
        if (st.error)
            card.append(el("p", "ext-error", st.error));
        if (st.dropped_logs)
            card.append(el("p", "field-hint", () => i18nText("因频率限制丢弃了 {n} 条日志", { n: String(st.dropped_logs) })));
        return card;
    }
    function field(name, prop, value) {
        const wrap = el("label", prop.type === "boolean" ? "check" : "field");
        wrap.dataset.key = name;
        if (prop.type === "boolean") {
            const input = el("input");
            input.type = "checkbox";
            input.checked = value === true;
            input.addEventListener("change", () => { dirty = true; });
            wrap.append(input, el("span", "", local(prop.title)));
            return { node: wrap, read: () => input.checked };
        }
        wrap.append(el("span", "", local(prop.title)));
        let read;
        if (prop.type === "enum") {
            const select = el("select");
            for (const v of prop.enum || []) {
                const opt = el("option", "", v);
                opt.value = v;
                select.append(opt);
            }
            select.value = String(value ?? prop.default ?? "");
            select.addEventListener("change", () => { dirty = true; });
            wrap.append(select);
            read = () => select.value;
        }
        else {
            const input = el("input");
            if (prop.type === "string") {
                input.type = "text";
                if (prop.maxLength)
                    input.maxLength = prop.maxLength;
                input.value = value == null ? "" : String(value);
                read = () => input.value;
            }
            else {
                input.type = "number";
                if (prop.minimum != null)
                    input.min = String(prop.minimum);
                if (prop.maximum != null)
                    input.max = String(prop.maximum);
                input.step = prop.type === "integer" ? "1" : "any";
                input.value = value == null ? "" : String(value);
                read = () => input.value.trim() === "" ? undefined : Number(input.value);
            }
            input.addEventListener("input", () => { dirty = true; });
            wrap.append(input);
        }
        const hint = [];
        if (prop.minimum != null || prop.maximum != null)
            hint.push(`${prop.minimum ?? "…"} – ${prop.maximum ?? "…"}`);
        const desc = prop.description;
        const hintNode = el("span", "hint", () => [pickLocal(desc), ...hint].filter(Boolean).join(" · "));
        if (desc || hint.length)
            wrap.append(hintNode);
        return { node: wrap, read };
    }
    function settingsCard(ext) {
        const s = ext.setting;
        const card = el("section", "card ext-settings");
        card.append(el("h3", "", () => i18nText("设置")));
        const enabled = el("input");
        enabled.type = "checkbox";
        enabled.checked = s.enabled;
        enabled.addEventListener("change", () => { dirty = true; });
        const enabledRow = el("label", "check ext-enable");
        enabledRow.append(enabled, el("span", "", () => i18nText("启用这个扩展")));
        card.append(enabledRow);
        // Permissions: approved one by one; the ones hooks need are marked.
        const required = new Set(ext.hooks.map(h => HOOK_PERMISSION[h]).filter(Boolean));
        const permBox = el("fieldset", "ext-perms");
        permBox.append(el("legend", "", () => i18nText("批准的权限")));
        const permInputs = new Map();
        for (const perm of ext.permissions) {
            const row = el("label", "check ext-perm");
            const input = el("input");
            input.type = "checkbox";
            input.checked = s.permissions.includes(perm);
            input.addEventListener("change", () => { dirty = true; });
            permInputs.set(perm, input);
            const text = el("span", "ext-perm-text");
            const info = PERMISSIONS[perm];
            text.append(el("b", "", info ? info.label : () => perm), el("code", "", perm));
            if (required.has(perm))
                text.append(el("span", "tag", () => i18nText("必需")));
            text.append(el("span", "hint", info ? info.risk : () => ""));
            row.append(input, text);
            permBox.append(row);
        }
        card.append(permBox);
        // Bindings: only API key / relay accounts can be routed through the gateway.
        const bindBox = el("fieldset", "ext-bindings");
        bindBox.append(el("legend", "", () => i18nText("作用于哪些账号")));
        const mode = el("select");
        for (const [value, label] of [["none", () => i18nText("不绑定")], ["accounts", () => i18nText("指定账号")], ["all", () => i18nText("全部 API Key / 中转账号（含以后新增的）")]]) {
            const opt = el("option", "", label);
            opt.value = value;
            mode.append(opt);
        }
        mode.value = s.bindings.mode || "none";
        const accountBox = el("div", "ext-accounts");
        const accountInputs = new Map();
        for (const a of ext.accounts) {
            const row = el("label", "check");
            const input = el("input");
            input.type = "checkbox";
            input.checked = !!s.bindings.accounts?.includes(a.id);
            input.disabled = !a.eligible && !input.checked;
            input.addEventListener("change", () => { dirty = true; });
            accountInputs.set(a.id, input);
            const label = el("span", "", `${a.label || a.id} (${a.type})`);
            row.append(input, label);
            if (!a.eligible)
                row.append(el("span", "hint", () => i18nText("仅支持 API Key / 中转账号")));
            accountBox.append(row);
        }
        const syncMode = () => accountBox.classList.toggle("hidden", mode.value !== "accounts");
        mode.addEventListener("change", () => { dirty = true; syncMode(); });
        syncMode();
        bindBox.append(mode, accountBox, el("p", "hint", () => i18nText("订阅登录（Claude 订阅、ChatGPT 登录）的账号不能绑定。绑定后，新开的对话与终端会改走模型网关；已打开的终端里，Claude 要新开窗口，Codex 重新启动即可。")));
        card.append(bindBox);
        const policyField = el("label", "field");
        policyField.append(el("span", "", () => i18nText("扩展失效时")));
        const policy = el("select");
        const defaultLabel = ext.failure_policy === "fail_closed" ? () => i18nText("按扩展默认（拦截）") : () => i18nText("按扩展默认（放行）");
        for (const [value, label] of [["", defaultLabel], ["fail_open", () => i18nText("放行：不检查，照常请求")], ["fail_closed", () => i18nText("拦截：拒绝请求或中断回复")]]) {
            const opt = el("option", "", label);
            opt.value = value;
            policy.append(opt);
        }
        policy.value = s.failure_policy || "";
        policy.addEventListener("change", () => { dirty = true; });
        policyField.append(policy, el("span", "hint", () => i18nText("扩展未运行、回答超时或检查跟不上时怎么处理。放行时扩展页会标出降级。")));
        card.append(policyField);
        const fields = new Map();
        const props = Object.entries(ext.config_schema.properties || {}).sort(([a], [b]) => a.localeCompare(b));
        if (props.length) {
            const cfgBox = el("fieldset", "ext-config");
            cfgBox.append(el("legend", "", () => i18nText("参数")));
            const row = el("div", "field-row");
            for (const [name, prop] of props) {
                const f = field(name, prop, s.config?.[name]);
                fields.set(name, f.read);
                row.append(f.node);
            }
            cfgBox.append(row);
            card.append(cfgBox);
        }
        const foot = el("div", "card-foot");
        foot.append(el("span", "note", () => i18nText("保存后扩展会重启，内存里的冷却记录随之清空。")));
        const save = el("button", "btn btn-primary btn-sm");
        save.type = "button";
        buttonLabel(save, () => i18nText("保存"), "check");
        save.addEventListener("click", () => void submit(ext, save));
        foot.append(save);
        card.append(foot);
        form = {
            collect: () => {
                const config = {};
                for (const [name, read] of fields) {
                    const v = read();
                    if (v !== undefined)
                        config[name] = v;
                }
                return {
                    expected_revision: s.revision, enabled: enabled.checked, order: s.order, config,
                    permissions: [...permInputs].filter(([, i]) => i.checked).map(([p]) => p),
                    bindings: { mode: mode.value, accounts: mode.value === "accounts" ? [...accountInputs].filter(([, i]) => i.checked).map(([id]) => id) : [] },
                    failure_policy: policy.value,
                };
            },
        };
        return card;
    }
    async function submit(ext, button) {
        if (!form || saving)
            return;
        saving = true;
        button.disabled = true;
        const mine = ++gen;
        try {
            const saved = await api(`/extensions/${encodeURIComponent(ext.id)}`, { method: "PUT", body: JSON.stringify(form.collect()), signal });
            if (signal.aborted)
                return;
            if (data)
                data.extensions = data.extensions.map(e => e.id === saved.id ? saved : e);
            toast(i18nText("扩展设置已保存"));
            if (mine === gen) {
                renderList();
                renderDetail();
            }
            void load(true);
        }
        catch (err) {
            if (signal.aborted)
                return;
            if (err instanceof APIError && err.status === 409) {
                toast(i18nText("扩展设置已被其他人修改，已重新载入"), true);
                void load(true);
            }
            else {
                toast(err.message, true);
            }
        }
        finally {
            saving = false;
            button.disabled = false;
        }
    }
    function eventsCard() {
        const card = el("section", "card ext-events");
        card.append(el("h3", "", () => i18nText("最近的中断与拒绝")), el("p", "card-desc", () => i18nText("只保留最近 200 条，服务重启后清空；不记录提示词和回复内容。")));
        if (!events.length) {
            card.append(el("p", "field-hint", () => i18nText("还没有记录。")));
            return card;
        }
        const table = el("table", "ext-table");
        const head = el("tr");
        for (const label of [() => i18nText("时间"), () => i18nText("动作"), () => i18nText("原因"), () => i18nText("用户"), () => i18nText("工作空间"), () => i18nText("账号")])
            head.append(el("th", "", label));
        table.append(el("thead"), el("tbody"));
        table.tHead.append(head);
        const names = new Map(S.sessions.map(s => [s.id, s.name]));
        for (const e of events) {
            const row = el("tr");
            const cells = [[() => i18nText("时间"), fmtDateTime(e.time)], [() => i18nText("动作"), KINDS[e.kind]?.() || e.kind], [() => i18nText("原因"), e.code],
                [() => i18nText("用户"), e.user || "—"], [() => i18nText("工作空间"), names.get(e.session || "") || e.session || "—"], [() => i18nText("账号"), e.account || "—"]];
            for (const [label, value] of cells) {
                const td = el("td", "", value);
                setAttrRender(td, "data-l", label);
                row.append(td);
            }
            table.tBodies[0].append(row);
        }
        card.append(table);
        return card;
    }
    function logsCard() {
        const card = el("section", "card ext-logs");
        card.append(el("h3", "", () => i18nText("扩展日志")));
        if (!logs.length) {
            card.append(el("p", "field-hint", () => i18nText("还没有日志。")));
            return card;
        }
        const list = el("ol", "ext-log mono");
        for (const line of logs.slice().reverse()) {
            const li = el("li");
            li.dataset.level = line.level;
            li.append(el("time", "", fmtDateTime(line.time)), el("span", "", " " + line.level + " "), el("span", "", line.message));
            list.append(li);
        }
        card.append(list);
        return card;
    }
    function openExtension(id) {
        if (S.extSel !== id)
            dirty = false;
        S.extSel = id;
        showView("extensions");
        renderList();
        renderDetail();
        void load(true);
    }
    btn.addEventListener("click", () => openExtension(""), { signal });
    bus.addEventListener("open-extensions", e => openExtension(e.detail || ""), { signal });
    window.addEventListener("agentbox-language-change", () => { if (data) {
        renderList();
        renderLive();
    } }, { signal });
    const timer = setInterval(() => {
        if (onPage() && !saving && document.visibilityState === "visible")
            void load(false);
    }, 5000);
    return () => {
        lifetime.abort();
        clearInterval(timer);
        gen++;
        data = null;
        events = [];
        logs = [];
        form = null;
        S.extSel = "";
        $("ext-list").replaceChildren();
        $("ext-detail").replaceChildren();
        btn.classList.add("hidden");
    };
}
