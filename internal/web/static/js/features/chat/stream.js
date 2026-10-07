import { liveNode, renderEvent, formatText, answerSources } from "../../chat-render.js";
import { t as i18nText } from "../../i18n.js";
export function createChatStream(hooks) {
    /* ---------------- 流式增量渲染 ----------------
     * Claude：--include-partial-messages 下，完整 assistant 事件之前会先收到
     * stream_event（content_block_start/delta/stop）。delta 到达节奏不均
     * （常一次一整句），直接上屏会一段一段蹦：先进缓冲，由 rAF 打字机
     * 匀速放出，积压越多放得越快，显示只滞后生成零点几秒。text 块每次
     * 放出后整段重跑 Markdown（与最终渲染同一管线），代码块、列表边生成
     * 边呈现；完整事件到达后移除临时节点交回 renderEvent 正式渲染，
     * 两边产物一致，替换无观感跳变。增量事件服务端只广播不落盘，
     * 历史回放天然只有完整事件。
     *
     * Codex：服务端经 app-server 协议拿到真增量，翻译成与 Claude 相同的
     * stream_event 形状广播，这条管线原样消费——真流式与 Claude 无异；
     * 完整 item.completed 到达时同样移除临时节点交回正式渲染。
     *
     * 兜底（旧容器里的 codex 走 exec --json，没有增量，全文只在
     * item.completed 一次性到达）：把全文喂进同一条 rAF 管线做「整段回放」，
     * 速度取 max(140 字/秒, 全文/3s)，短文按打字节奏、长文封顶 3 秒放完。
     * 回放中到达的其余事件（工具行、回合完成章等）排队，动画完成后按序
     * 补上；回放不因回合结束被截断，但新用户消息 / 出错 / 切线程时立刻放完。 */
    let liveEls = []; // 本条消息的临时节点，完整事件到达后整体移除
    let liveBlock = null; // 当前追加目标
    /* 回合结束收尾：缓冲余量上屏，临时节点保留在页面上（中断时留住
     * 已生成的部分），只是不再跟踪、去掉光标 */
    function streamSettle() {
        if (liveBlock && liveBlock.replay)
            return; // codex 回放自行收尾，不被回合结束截断
        liveDrain();
        for (const el of liveEls)
            el.classList.remove("streaming");
        liveEls = [];
        liveBlock = null;
        hooks.footer()?.update();
    }
    function liveClear() {
        if (liveBlock && liveBlock.raf)
            cancelAnimationFrame(liveBlock.raf);
        for (const el of liveEls)
            el.remove();
        liveEls = [];
        liveBlock = null;
    }
    function handleAgentEvent(ev) {
        if (ev && ev.type === "stream_event") {
            handleStreamEvent(ev.event);
            return;
        }
        // 完整 assistant 事件带全部内容，先移除对应的增量节点再正式渲染
        if (ev && ev.type === "assistant")
            liveClear();
        workLabelFromEvent(ev);
        if (replayFeed(ev))
            return; // codex 整段文本 → 打字机回放
        for (const node of renderEvent(ev))
            replayAppend(node);
    }
    /* ---- codex 整段回放队列 ---- */
    let replayQueue = [];
    function replayActive() { return !!(liveBlock && liveBlock.replay) || replayQueue.length > 0; }
    /* codex 的文本事件（新旧两种结构）转打字任务；消费掉返回 true */
    function replayFeed(ev) {
        if (!ev || typeof ev !== "object")
            return false;
        let kind = "", text = "";
        if (ev.type === "item.completed" && ev.item) {
            if (ev.item.type === "agent_message") {
                kind = "text";
                text = ev.item.text;
            }
            else if (ev.item.type === "reasoning") {
                kind = "thinking";
                text = ev.item.text;
            }
        }
        else if (ev.msg && typeof ev.msg === "object") {
            if (ev.msg.type === "agent_message") {
                kind = "text";
                text = ev.msg.message;
            }
            else if (ev.msg.type === "agent_reasoning") {
                kind = "thinking";
                text = ev.msg.text;
            }
        }
        if (!kind || !text)
            return false;
        // 真流式（app-server 增量）已把这条消息现场打出来：清掉临时节点交回
        // 正式渲染（与 Claude 完整事件同一套路），不再回放
        if (liveEls.length) {
            liveClear();
            return false;
        }
        replayQueue.push({ kind, text });
        replayPump();
        return true;
    }
    /* 回放进行中时后续事件的节点排队，保持时间线顺序；空闲时直接上屏 */
    function replayAppend(node) {
        if (replayActive())
            replayQueue.push({ node });
        else
            hooks.append(node);
    }
    function replayPump() {
        if (liveBlock)
            return; // 已有动画在放
        while (replayQueue.length) {
            const job = replayQueue.shift();
            if (job.node) {
                hooks.append(job.node);
                continue;
            }
            hooks.working(() => job.kind === "thinking" ? i18nText("推理中…") : i18nText("生成回复…"));
            liveBlock = {
                ...liveNode(job.kind), kind: job.kind, shown: "", buf: "", carry: 0, tick: 0, raf: 0,
                replay: true, rate: Math.max(140, job.text.length / 3),
            };
            liveBlock.el.classList.add("streaming");
            hooks.append(liveBlock.el);
            liveFeed(job.text);
            return;
        }
    }
    /* 一段回放放完：定格为与正式渲染一致的形态，接着放下一个排队项 */
    function replayFinish() {
        const b = liveBlock;
        if (!b || !b.replay)
            return;
        if (b.raf)
            cancelAnimationFrame(b.raf);
        b.el.classList.remove("streaming", "live-text");
        if (b.kind === "thinking")
            b.el.open = false; // 与正式渲染一致：思考默认折叠
        liveBlock = null;
        hooks.footer()?.update();
        replayPump();
    }
    /* 立刻放完全部积压：新用户消息 / 出错时调用，保证时间线完整不乱序 */
    function replayFlush() {
        while (liveBlock && liveBlock.replay)
            liveDrain();
    }
    /* 丢弃回放状态：切线程 / 切会话时调用，对话流马上要整体重建 */
    function replayReset() {
        replayQueue = [];
        // Both real streaming and exec replay belong to the old view. Never let a
        // pending frame use the next thread's footer or scroll position.
        if (liveBlock?.raf)
            cancelAnimationFrame(liveBlock.raf);
        liveClear();
    }
    /* 依据整包事件刷新执行指示的阶段文案（流式的文字/思考在 handleStreamEvent 里更新） */
    function workLabelFromEvent(ev) {
        if (!ev || hooks.state() !== "running")
            return;
        if (ev.type === "assistant" && ev.message && Array.isArray(ev.message.content)) {
            for (const b of ev.message.content) {
                if (b.type === "tool_use") {
                    hooks.working(() => i18nText("运行工具 ") + b.name + "…");
                    return;
                }
            }
        }
        else if (ev.type === "item.started" && ev.item && ev.item.type === "command_execution") {
            hooks.working(() => i18nText("执行命令…"));
        }
        else if (ev.type === "item.completed" && ev.item && ev.item.type === "reasoning") {
            hooks.working(() => i18nText("推理中…"));
        }
    }
    function handleStreamEvent(e) {
        if (!e || typeof e !== "object")
            return;
        switch (e.type) {
            case "message_start":
                // 正常流程上一条已被完整事件清掉（此处空转）；若断连错过了完整
                // 事件则保留残留文本，只停止跟踪，避免删掉用户已看到的内容
                streamSettle();
                break;
            case "content_block_start": {
                liveDrain(); // 上一块若有残余（丢了 stop 事件）先补齐
                const t = e.content_block && e.content_block.type;
                if (t === "text" || t === "thinking") {
                    hooks.working(() => t === "thinking" ? i18nText("思考中…") : i18nText("生成回复…"));
                    liveBlock = { ...liveNode(t), kind: t, shown: "", buf: "", carry: 0, tick: 0, raf: 0 };
                    liveBlock.el.classList.add("streaming");
                    hooks.append(liveBlock.el);
                    liveEls.push(liveBlock.el);
                }
                else {
                    liveBlock = null; // tool_use 等交给完整事件渲染
                }
                break;
            }
            case "content_block_delta": {
                if (!liveBlock || !e.delta)
                    break;
                const txt = e.delta.type === "text_delta" ? e.delta.text
                    : e.delta.type === "thinking_delta" ? e.delta.thinking : "";
                if (txt)
                    liveFeed(txt);
                break;
            }
            case "content_block_stop":
                if (liveBlock) {
                    liveDrain();
                    liveBlock.el.classList.remove("streaming");
                }
                liveBlock = null;
                break;
        }
    }
    /* ---- 打字机：缓冲 → rAF 匀速放出 ---- */
    function liveFeed(text) {
        const b = liveBlock;
        b.buf += text;
        if (document.hidden) {
            liveDrain();
            return;
        } // 后台标签页 rAF 停摆，直接上屏
        if (!b.raf) {
            b.tick = performance.now();
            b.raf = requestAnimationFrame(now => { if (liveBlock === b)
                liveTick(now); });
        }
    }
    function liveTick(now) {
        const b = liveBlock;
        if (!b || !b.raf)
            return;
        b.raf = 0;
        const dt = Math.min(now - b.tick, 200);
        if (dt >= 33) { // Markdown 整段重渲染有成本，帧率封顶 ~30fps
            b.tick = now;
            // 真流式：基础 80 字/秒，按积压加速（约 0.4s 追平），滞后有上界；
            // codex 回放：全文早已到齐，按定速放（rate 已封顶总时长）
            b.carry += (dt / 1000) * (b.rate || Math.max(80, b.buf.length * 2.5));
            const n = Math.min(b.buf.length, Math.floor(b.carry));
            if (n > 0) {
                b.carry -= n;
                b.shown += b.buf.slice(0, n);
                b.buf = b.buf.slice(n);
                liveRender(b);
            }
        }
        if (b.buf)
            b.raf = requestAnimationFrame(now => { if (liveBlock === b)
                liveTick(now); });
        else if (b.replay)
            replayFinish();
        else
            b.carry = 0;
    }
    /* 缓冲余量一次性上屏（块结束/回合收尾/后台标签页时用） */
    function liveDrain() {
        const b = liveBlock;
        if (!b)
            return;
        if (b.raf) {
            cancelAnimationFrame(b.raf);
            b.raf = 0;
        }
        if (b.buf) {
            b.shown += b.buf;
            b.buf = "";
            liveRender(b);
        }
        if (b.replay)
            replayFinish();
    }
    function liveRender(b) {
        const log = hooks.log();
        const stick = hooks.nearBottom(log);
        if (b.kind === "text") {
            b.body.replaceChildren(formatText(b.shown));
            answerSources.set(b.el, b.shown);
            const footer = hooks.footer();
            if (footer && b.shown)
                footer.el.hidden = false;
        }
        else
            b.body.textContent = b.shown; // thinking 与最终渲染一致，保持纯文本
        if (stick)
            log.scrollTop = log.scrollHeight;
    }
    return { event: handleAgentEvent, append: replayAppend, settle: streamSettle, flush: replayFlush, reset: replayReset };
}
