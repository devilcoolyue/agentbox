import { setAttrRender, t as i18nText } from "../../i18n.js";
import { setTip } from "../../tip.js";
import { fmtTokens } from "../../usage-math.js";
/* 上下文占用：CLI 状态栏（claude-hud）那根上下文条的网页版。-p 模式不跑状态栏，
 * 但每次模型请求的用量本来就在事件流里：主对话最近一次请求的
 * 输入 + 缓存写入 + 缓存读取 = 这次喂给模型的整段上下文，与 claude-hud 同口径；
 * 窗口大小取回合结果 modelUsage 里的 contextWindow，不按模型名猜。
 * 子 Agent（parent_tool_use_id）与跑腿各有各的上下文，不算进来。
 * 只认 Claude：Codex 的事件里暂时没有上下文窗口。 */
const WARN = 70, CRITICAL = 85; // claude-hud 的默认阈值
const count = (v) => typeof v === "number" && Number.isFinite(v) && v > 0 ? v : 0;
/** 一次请求喂给模型的上下文：未命中缓存的输入 + 缓存写入 + 缓存读取 */
export function contextTokens(usage) {
    return usage ? count(usage.input_tokens) + count(usage.cache_creation_input_tokens) + count(usage.cache_read_input_tokens) : 0;
}
/** 回合结果里这个模型的上下文窗口；键可能带 [1m] 之类的后缀，对不上时取最大的那个 */
export function contextWindow(models, model) {
    if (!models || typeof models !== "object")
        return 0;
    let exact = 0, largest = 0;
    for (const [key, value] of Object.entries(models)) {
        const size = count(value?.contextWindow);
        if (!size)
            continue;
        if (model && (key === model || key.replace(/\[[^\]]*\]$/, "") === model))
            exact = Math.max(exact, size);
        largest = Math.max(largest, size);
    }
    return exact || largest;
}
export function createContextMeter(hooks) {
    let used = 0; // 主对话最近一次请求的上下文
    let model = "";
    let size = 0; // 本线程结果里报过的窗口
    let compacted = 0; // 压缩前的规模；压缩后到下一次请求前一直标着
    // 压缩边界之后 CLI 重发的保留消息带的是压缩前的用量，到回合结束都不算
    let replays = new Set();
    // 同一空间同一模型的窗口在新线程里沿用，第一轮还没结束也能显示占比
    const known = new Map();
    const knownKey = () => `${hooks.session() || ""}|${model}`;
    function observe(ev) {
        if (!ev || typeof ev !== "object" || ev.parent_tool_use_id)
            return;
        if (ev.type === "assistant" && typeof ev.uuid === "string" && replays.has(ev.uuid))
            return;
        let message;
        if (ev.type === "assistant")
            message = ev.message;
        else if (ev.type === "stream_event" && ev.event?.type === "message_start")
            message = ev.event.message;
        if (message) {
            // 本地命令（/context 等）的回答是 <synthetic>，用量全是 0
            const tokens = contextTokens(message.usage);
            if (!tokens || typeof message.model === "string" && message.model.startsWith("<"))
                return;
            used = tokens;
            compacted = 0;
            if (typeof message.model === "string" && message.model)
                model = message.model;
        }
        else if (ev.type === "system" && ev.subtype === "compact_boundary") {
            const meta = ev.compact_metadata;
            compacted = count(meta?.pre_tokens) || used || 1;
            used = count(meta?.post_tokens); // 新版 CLI 报压缩后的规模；没有就等下一次请求
            const kept = meta?.preserved_messages;
            const ids = Array.isArray(kept?.all_uuids) ? kept.all_uuids : Array.isArray(kept?.uuids) ? kept.uuids : [];
            replays = new Set(ids.filter((id) => typeof id === "string"));
        }
        else if (ev.type === "result") {
            replays.clear();
            const window = contextWindow(ev.modelUsage, model);
            if (!window)
                return;
            size = window;
            if (model)
                known.set(knownKey(), window);
        }
        else
            return;
        render();
    }
    function render() {
        const el = hooks.el();
        const window = size || known.get(knownKey()) || 0;
        const show = hooks.agent() === "claude" && (used > 0 || compacted > 0);
        el.classList.toggle("hidden", !show);
        if (!show)
            return;
        const percent = used && window ? Math.min(100, Math.round(used / window * 100)) : -1;
        el.dataset.level = percent >= CRITICAL ? "critical" : percent >= WARN ? "warn" : "ok";
        el.style.setProperty("--ctx", String(Math.max(percent, 0)));
        const value = el.querySelector(".ctx-value");
        const summary = () => compacted && used
            ? percent >= 0
                ? i18nText("上下文已压缩：{p0} → {p1} tokens（{p2}%）", { p0: fmtTokens(compacted), p1: fmtTokens(used), p2: String(percent) })
                : i18nText("上下文已压缩：{p0} → {p1} tokens", { p0: fmtTokens(compacted), p1: fmtTokens(used) })
            : compacted
                ? i18nText("上下文已压缩（压缩前 {p0} tokens），下一次请求后更新", { p0: fmtTokens(compacted) })
                : percent >= 0
                    ? i18nText("上下文已用 {p0} / {p1} tokens（{p2}%）", { p0: fmtTokens(used), p1: fmtTokens(window), p2: String(percent) })
                    : i18nText("上下文已用 {p0} tokens，本轮结束后显示占比", { p0: fmtTokens(used) });
        value.textContent = !used ? "—" : percent >= 0 ? `${percent}%` : fmtTokens(used);
        setAttrRender(el, "aria-label", summary);
        setTip(el, () => [
            summary(),
            i18nText("按最近一次模型请求计算（输入 + 缓存），与 CLI 状态栏同口径"),
            ...percent >= CRITICAL ? [i18nText("接近上限时 CLI 会自动压缩较早的对话")] : [],
        ].join("\n"));
    }
    return {
        observe,
        /** 按历史记录还原：最新一页里最后的请求就是当前上下文 */
        replay(entries) {
            for (const e of entries)
                if (e.kind === "event")
                    observe(e.event);
            render();
        },
        reset() { used = 0; model = ""; size = 0; compacted = 0; replays = new Set(); render(); },
    };
}
