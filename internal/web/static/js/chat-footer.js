import { answerSources, copyText } from "./chat-render.js";
import { svgIcon } from "./icons.js";
import { EFFORT_LABELS } from "./reasoning.js";
import { S } from "./state.js";
import { setTip } from "./tip.js";
import { toast, fmtDateTime } from "./util.js";
/** Old Claude logs can supply a model; missing effort remains unknown. */
export function observeAnswer(context, ev, ts) {
    if (!ev || ev.parent_tool_use_id)
        return;
    // Old transcripts have no turn ID to join with the ledger. Preserve the
    // CLI's reported amount, explicitly labelled, without pricing old tokens now.
    if (ev.type === "result" && !context.turn) {
        const models = ev.modelUsage;
        const costs = models && typeof models === "object" ? Object.values(models).map(m => m?.costUSD) : [];
        const value = costs.length && costs.every(n => typeof n === "number" && Number.isFinite(n) && n >= 0)
            ? costs.reduce((sum, n) => sum + Math.round(n * 1e6), 0)
            : typeof ev.total_cost_usd === "number" && Number.isFinite(ev.total_cost_usd) ? Math.round(ev.total_cost_usd * 1e6) : 0;
        context.legacyCost = value > 0 ? value : undefined;
    }
    if (ev.type === "stream_event" && ev.event?.delta?.type === "text_delta" && ts)
        context.ts = ts;
    const model = ev.type === "assistant" ? ev.message?.model
        : ev.type === "system" && ev.subtype === "init" ? ev.model : undefined;
    if (typeof model === "string" && model && !model.startsWith("<"))
        context.reportedModel = model;
    const text = ev.type === "assistant" && Array.isArray(ev.message?.content)
        ? ev.message.content.some(block => block.type === "text" && block.text)
        : ev.type === "item.completed" && ev.item?.type === "agent_message"
            || ev.msg?.type === "agent_message" || ev.type === "agent_message";
    if (text && ts)
        context.ts = ts;
}
function reasoningText(turn) {
    if (!turn)
        return "未记录";
    if (turn.unsupported)
        return "不支持调整";
    if (!turn.effort)
        return "默认";
    if (turn.control === "budget") {
        return turn.budget_tokens ? `思考预算 ${turn.budget_tokens.toLocaleString("en-US")} tokens`
            : `思考预算 · ${EFFORT_LABELS[turn.effort] || turn.effort}`;
    }
    return EFFORT_LABELS[turn.effort] || turn.effort;
}
/* 计费来源写成用户看得懂的话：「价目表」「CLI 报告」是实现细节 */
const SOURCE_TEXT = { table: "按单价估算", provider: "客户端上报", mixed: "部分按单价估算" };
function costView(context) {
    const money = (micro) => "$" + (micro / 1e6).toFixed(micro > 0 && micro < 100 ? 6 : 4);
    const cost = context.cost;
    if (cost && cost.source !== "unknown") {
        if (cost.source === "unpriced")
            return { label: "未定价", detail: "本轮已记录用量，但这个模型还没有设置单价，所以没有算出金额；这不表示模型服务免费。", priced: false };
        const source = SOURCE_TEXT[cost.source] || "混合计价";
        return {
            label: (cost.partial ? "≥ " : "") + money(cost.cost_micro_usd),
            detail: `本轮用量成本 ${money(cost.cost_micro_usd)}，${cost.partial ? "部分模型未设置单价，金额不完整" : source}。与使用记录一致，包含本轮子模型，不含自动起标题；不代表订阅额外扣费或中转站实际账单。`,
            priced: true,
        };
    }
    if (!context.turn && context.legacyCost !== undefined)
        return {
            label: money(context.legacyCost),
            detail: "历史回答中客户端上报的用量成本；无法关联当时的入账记录，不代表订阅额外扣费或中转站实际账单。", priced: true,
        };
    const pending = !cost && !context.historical;
    return { label: pending ? "费用待结算" : "费用未记录", detail: pending ? "回合结束后显示已入账的用量成本。" : "未找到这轮回答对应的费用记录，未按当前价格重算历史。", priced: false };
}
export function answerFooter(body, context) {
    const el = document.createElement("div");
    el.className = "answer-footer";
    el.setAttribute("role", "group");
    el.setAttribute("aria-label", "回答信息与操作");
    const copy = document.createElement("button");
    copy.type = "button";
    copy.className = "answer-copy";
    copy.setAttribute("aria-label", "复制回答");
    copy.append(svgIcon("copy", 16));
    setTip(copy, "复制回答（Markdown）");
    const actions = document.createElement("div");
    actions.className = "answer-actions";
    const more = document.createElement("button");
    more.type = "button";
    more.className = "answer-copy answer-more";
    more.setAttribute("aria-label", "回答详情");
    more.setAttribute("aria-expanded", "false");
    more.append(svgIcon("info", 16));
    setTip(more, "回答详情");
    // 外面只放时间和金额；模型、推理强度、计费说明与本轮 token 收进「详情」
    const details = document.createElement("div");
    details.className = "answer-details";
    details.hidden = true;
    const detailRow = (key) => {
        const row = document.createElement("div");
        row.className = "ad-row";
        const k = Object.assign(document.createElement("span"), { className: "ad-k", textContent: key });
        const v = Object.assign(document.createElement("span"), { className: "ad-v" });
        const note = Object.assign(document.createElement("span"), { className: "ad-note" });
        row.append(k, v, note);
        return { row, v, note };
    };
    const model = detailRow("模型");
    const effort = detailRow("推理强度");
    const costDetail = detailRow("费用");
    const receipt = document.createElement("div");
    receipt.className = "answer-receipt";
    details.append(model.row, effort.row, costDetail.row, receipt);
    more.addEventListener("click", () => {
        details.hidden = !details.hidden;
        more.setAttribute("aria-expanded", String(!details.hidden));
    });
    actions.append(copy, more);
    const info = document.createElement("div");
    info.className = "answer-context";
    const date = document.createElement("time");
    date.className = "answer-time";
    const cost = document.createElement("span");
    cost.className = "answer-cost";
    info.append(date, cost);
    el.append(actions, info, details);
    copy.addEventListener("click", async () => {
        const source = [...body.querySelectorAll(".msg.agent")]
            .map(node => answerSources.get(node) || "").filter(Boolean).join("\n\n");
        if (!source)
            return;
        try {
            await copyText(source);
        }
        catch (_) {
            toast("复制失败，请选择回答后手动复制", true);
            return;
        }
        copy.replaceChildren(svgIcon("check", 16));
        copy.classList.add("copied");
        copy.setAttribute("aria-label", "已复制回答");
        setTip(copy, "已复制");
        copy.disabled = true;
        setTimeout(() => {
            copy.replaceChildren(svgIcon("copy", 16));
            copy.classList.remove("copied");
            copy.setAttribute("aria-label", "复制回答");
            setTip(copy, "复制回答（Markdown）");
            copy.disabled = false;
        }, 1400);
    });
    function update() {
        el.hidden = ![...body.querySelectorAll(".msg.agent")].some(node => answerSources.has(node));
        const turn = context.turn;
        model.v.textContent = context.reportedModel || turn?.model || "未记录";
        model.note.textContent = context.reportedModel
            ? (turn && turn.model !== context.reportedModel ? `请求的是 ${turn.model}，以模型实际返回为准` : "")
            : turn ? "服务未返回实际模型，这里是请求的模型" : "这条历史回答未记录模型";
        effort.v.textContent = reasoningText(turn);
        effort.note.textContent = !turn ? "这条历史回答未记录推理设置"
            : turn.unsupported ? "本轮模型不支持调整推理强度"
                : !turn.effort ? "本轮未指定，沿用模型默认"
                    : turn.control === "budget" ? "这是请求的思考预算上限，并非实际消耗"
                        : "";
        const d = context.ts ? new Date(context.ts) : null;
        if (d && Number.isFinite(d.getTime())) {
            const zone = S.timeZone || "Asia/Shanghai";
            date.textContent = fmtDateTime(d, false);
            date.dateTime = d.toISOString();
            setTip(date, `回答时间：${fmtDateTime(d)}（${zone}）`);
        }
        else {
            date.textContent = "时间未记录";
            date.removeAttribute("datetime");
            setTip(date, "这条回答未记录时间");
        }
        const price = costView(context);
        cost.textContent = price.label;
        cost.classList.toggle("priced", price.priced);
        setTip(cost, price.detail);
        costDetail.v.textContent = price.label;
        costDetail.note.textContent = price.detail;
    }
    return { el, update, setReceipt: (node) => receipt.replaceChildren(node) };
}
