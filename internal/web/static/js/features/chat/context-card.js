import { setText, setTextRender, t as i18nText } from "../../i18n.js";
import { fmtTokens } from "../../usage-math.js";
/* /context 的回答卡片：CLI 在 -p 模式下随这条本地回答附带结构化的 context_usage
 * （模型、总量、窗口、按类别的估算，以及 MCP 工具 / 记忆文件 / 技能明细），这里画成
 * 终端里那张格子图：每格是窗口的 1/200，按类别着色，自动压缩预留排在末尾。
 * 数字是 CLI 的估算，只用来看，不进计费。没有该字段的旧 CLI 仍按文字回答显示。 */
const COLS = 20, ROWS = 10, CELLS = COLS * ROWS;
// 颜色取各风格都定义了的全局令牌，配色大致对齐终端（消息紫、技能黄、记忆橙…）
const CATEGORIES = {
    "system prompt": { label: () => i18nText("系统提示词"), color: "var(--muted)" },
    "system tools": { label: () => i18nText("系统工具"), color: "color-mix(in srgb, var(--muted) 55%, var(--text))" },
    "mcp tools": { label: () => i18nText("MCP 工具"), color: "var(--info)" },
    "mcp server instructions": { label: () => i18nText("MCP 服务说明"), color: "var(--green)" },
    "custom agents": { label: () => i18nText("自定义 Agent"), color: "var(--syntax-function)" },
    "memory files": { label: () => i18nText("记忆文件"), color: "var(--amber)" },
    "skills": { label: () => i18nText("技能"), color: "var(--warn)" },
    "messages": { label: () => i18nText("消息"), color: "var(--syntax-keyword)" },
    "autocompact buffer": { label: () => i18nText("自动压缩预留"), color: "var(--line)" },
    "compact buffer": { label: () => i18nText("自动压缩预留"), color: "var(--line)" },
    "free space": { label: () => i18nText("可用空间"), color: "transparent" },
};
const tokens = (v) => typeof v === "number" && Number.isFinite(v) && v > 0 ? v : 0;
/** 这条 assistant 事件带的 /context 结构化结果；其它回答返回 undefined */
export function contextUsageOf(ev) {
    const usage = ev.context_usage;
    return usage && typeof usage === "object" && Array.isArray(usage.categories) ? usage : undefined;
}
function category(name) {
    const deferred = /\s*\(deferred\)$/i.test(name);
    const base = name.replace(/\s*\(deferred\)$/i, "");
    const known = CATEGORIES[base.toLowerCase()];
    const label = known ? known.label : () => base;
    return {
        color: known?.color || "var(--syntax-function)", deferred,
        label: deferred ? () => i18nText("{p0}（按需加载）", { p0: label() }) : label,
    };
}
const el = (tag, className, text) => {
    const node = document.createElement(tag);
    node.className = className;
    if (text !== undefined)
        node.textContent = text;
    return node;
};
function percentOf(n, max) {
    if (!max)
        return "";
    const p = n / max * 100;
    return (p > 0 && p < 0.1 ? "<0.1" : p.toFixed(1).replace(/\.0$/, "")) + "%";
}
/** 明细列表：名称 + 补充说明 + token 数，按 token 从多到少 */
function details(title, rows) {
    const box = document.createElement("details");
    box.className = "ctx-more";
    const head = document.createElement("summary");
    setTextRender(head, () => i18nText("{p0}（{p1}）", { p0: title(), p1: String(rows.length) }));
    const list = el("div", "ctx-list");
    for (const row of [...rows].sort((a, b) => b.tokens - a.tokens)) {
        const line = el("div", "ctx-item");
        const name = el("span", "ctx-item-name mono", row.name);
        line.append(name);
        if (row.note)
            line.append(el("span", "ctx-item-note", row.note));
        line.append(el("span", "ctx-item-tokens mono", fmtTokens(row.tokens)));
        list.append(line);
    }
    box.append(head, list);
    return box;
}
export function contextCard(usage) {
    const card = el("div", "msg agent ctx-card");
    const max = tokens(usage.raw_max_tokens), total = tokens(usage.total_tokens);
    const categories = (usage.categories || []).filter(c => c && typeof c.name === "string" && tokens(c.tokens) > 0);
    // 左侧格子：已用类别依次铺开，可用空间留空，自动压缩预留放在最后
    const grid = el("div", "ctx-grid");
    grid.setAttribute("role", "img");
    if (max) {
        const per = max / CELLS;
        const cells = [];
        let buffer = 0;
        for (const c of categories) {
            const n = Math.max(1, Math.round(tokens(c.tokens) / per));
            if (c.kind === "buffer")
                buffer += n;
            else if (c.kind === "used")
                for (let i = 0; i < n; i++)
                    cells.push({ color: category(c.name).color, kind: "used" });
        }
        buffer = Math.min(buffer, CELLS);
        const used = cells.slice(0, CELLS - buffer);
        const free = CELLS - used.length - buffer;
        const bufferColor = categories.find(c => c.kind === "buffer");
        for (const cell of [...used, ...Array.from({ length: free }, () => ({ color: "", kind: "free" })),
            ...Array.from({ length: buffer }, () => ({ color: bufferColor ? category(bufferColor.name).color : "", kind: "buffer" }))]) {
            const square = el("span", "ctx-cell");
            square.dataset.kind = cell.kind;
            if (cell.color)
                square.style.setProperty("--c", cell.color);
            grid.append(square);
        }
        grid.style.setProperty("--cols", String(COLS));
        grid.style.setProperty("--rows", String(ROWS));
    }
    else
        grid.hidden = true;
    // 右侧：模型、总量与类别图例
    const side = el("div", "ctx-side");
    const title = el("div", "ctx-title");
    setText(title, "上下文用量");
    const model = el("div", "ctx-model mono", usage.model || "");
    const amount = el("div", "ctx-amount");
    const percent = typeof usage.percentage === "number" ? usage.percentage : max ? Math.round(total / max * 100) : -1;
    setTextRender(amount, () => max && percent >= 0
        ? i18nText("{p0} / {p1} tokens（{p2}%）", { p0: fmtTokens(total), p1: fmtTokens(max), p2: String(percent) })
        : max ? `${fmtTokens(total)} / ${fmtTokens(max)} tokens` : `${fmtTokens(total)} tokens`);
    grid.setAttribute("aria-label", amount.textContent || "");
    side.append(title, model, amount);
    const over = tokens(usage.over_limit?.tokens_over);
    if (over) {
        const warn = el("div", "ctx-over");
        setTextRender(warn, () => i18nText("已超出上下文窗口 {p0} tokens", { p0: fmtTokens(over) }));
        side.append(warn);
    }
    const legendHead = el("div", "ctx-legend-head");
    setText(legendHead, "按类别估算");
    const legend = el("div", "ctx-legend");
    for (const c of categories) {
        const info = category(c.name);
        const row = el("div", "ctx-row");
        row.dataset.kind = info.deferred ? "deferred" : c.kind || "used";
        const swatch = el("span", "ctx-swatch");
        swatch.dataset.kind = c.kind === "free" ? "free" : c.kind === "buffer" ? "buffer" : info.deferred ? "deferred" : "used";
        swatch.style.setProperty("--c", info.color);
        const label = el("span", "ctx-row-label");
        setTextRender(label, info.label);
        const value = el("span", "ctx-row-value mono", `${fmtTokens(tokens(c.tokens))}${max ? ` · ${percentOf(tokens(c.tokens), max)}` : ""}`);
        row.append(swatch, label, value);
        legend.append(row);
    }
    side.append(legendHead, legend);
    const top = el("div", "ctx-top");
    top.append(grid, side);
    card.append(top);
    const mcp = (usage.mcp_tools || []).filter(t => t && t.name);
    if (mcp.length)
        card.append(details(() => i18nText("MCP 工具"), mcp.map(t => ({
            name: String(t.name).replace(new RegExp(`^mcp__${(t.server_name || "").replace(/[^\w-]/g, "")}__`), ""),
            note: t.server_name || "", tokens: tokens(t.tokens),
        }))));
    const memory = (usage.memory_files || []).filter(f => f && f.path);
    if (memory.length)
        card.append(details(() => i18nText("记忆文件"), memory.map(f => ({ name: String(f.path), note: f.type || "", tokens: tokens(f.tokens) }))));
    const skills = (usage.skills || []).filter(s => s && s.name);
    if (skills.length)
        card.append(details(() => i18nText("技能"), skills.map(s => ({ name: String(s.name), note: s.source || "", tokens: tokens(s.tokens) }))));
    const agents = (usage.agents || []).map(a => ({ name: String(a?.name || a?.agentType || a?.agent_type || ""), note: a?.source || "", tokens: tokens(a?.tokens) })).filter(a => a.name);
    if (agents.length)
        card.append(details(() => i18nText("自定义 Agent"), agents));
    return card;
}
