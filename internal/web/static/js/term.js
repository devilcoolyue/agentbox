/* term：终端页 —— xterm 实例、PTY WebSocket（含自动重连）、连接状态与顶栏提示轮播、
 * 终端内粘贴图片上传。 */
"use strict";
import { S, bus } from "./state.js";
import { $, isMobile, openLightbox } from "./util.js";
import { api, wsURL, imgURLFromPath, uploadAttachment } from "./api.js";
import { fmtUSD } from "./quota.js";
import { refreshAll } from "./data.js";
import { pastedImages } from "./chat.js";
const TerminalClass = window.Terminal && (window.Terminal.Terminal || window.Terminal);
const FitAddonClass = window.FitAddon && (window.FitAddon.FitAddon || window.FitAddon);
const WebglAddonClass = window.WebglAddon && (window.WebglAddon.WebglAddon || window.WebglAddon);
const ENC = new TextEncoder();
const IMG_PATH_RE = /\/shared\/\.images\/[A-Za-z0-9._-]+/g;
// 复用同一个 xterm 实例跨重连能保留回滚，但也把上一个程序（如 claude）开启的 DEC
// 私有模式一起带了过来：鼠标上报、备用屏、括号粘贴、焦点上报、应用光标键等。而服务端
// 每次重连都起一个全新的 bash（旧 claude 被强杀，来不及复位），于是新 shell 前台却还
// 开着鼠标上报——鼠标一动就把每次移动编码成 \x1b[<...M 发给 bash，回显成乱码、按回车
// 又被当命令执行（见 0723 报告）。故每次新连接建立时先复位这些输入相关的模式，让 xterm
// 与新 shell 对齐；首连时 xterm 本就是默认态，这串复位是空操作，安全。不清屏、不丢回滚。
const RESET_INPUT_MODES = "\x1b[?9l\x1b[?1000l\x1b[?1001l\x1b[?1002l\x1b[?1003l" + // 各类鼠标追踪关
    "\x1b[?1004l" + // 焦点上报关
    "\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l" + // 鼠标编码关
    "\x1b[?2004l" + // 括号粘贴关
    "\x1b[?1l\x1b>" + // 光标键/小键盘改回普通模式
    "\x1b[?1049l\x1b[?1047l\x1b[?47l" + // 退出备用屏
    "\x1b[?25h\x1b[?7h"; // 显示光标、自动换行
const token = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
/* ---------------- 连接状态展示（顶栏右侧：状态点 + 文案 + 重连钮） ---------------- */
function setConnStatus(state) {
    const dot = $("term-conn-dot");
    const st = $("term-state");
    const btn = $("term-reconnect");
    if (!st)
        return;
    let cls, text;
    switch (state) {
        case "connecting":
            cls = "warn";
            text = "连接中…";
            break;
        case "connected":
            cls = "good";
            text = "shell 已连接";
            break;
        case "reconnecting":
            cls = "warn";
            text = reconnectAttempt > 1 ? `重连中…（第 ${reconnectAttempt} 次）` : "重连中…";
            break;
        default:
            cls = "bad";
            state = "closed";
            text = "已断开，点右侧按钮重连";
            break;
    }
    if (dot)
        dot.className = "t-dot " + cls;
    st.textContent = text;
    st.dataset.state = state;
    if (btn)
        btn.classList.toggle("busy", state === "connecting" || state === "reconnecting");
}
/* ---------------- 自动重连 ---------------- */
// 退避序列（毫秒），封顶后一直用最后一档；成功连上或手动重连时归零。
const RECONNECT_BACKOFF = [1000, 2000, 3000, 5000, 8000, 10000];
let reconnectTimer = null;
let reconnectAttempt = 0;
function clearReconnect() {
    if (reconnectTimer) {
        clearTimeout(reconnectTimer);
        reconnectTimer = null;
    }
}
// 断线后排一次重连——仅当用户仍停留在终端页时才自动重连，切走了就停在「已断开」，
// 回到终端页（setTab → openTerm）会重新拉起。
function scheduleReconnect() {
    if (S.tab !== "term" || !S.current) {
        setConnStatus("closed");
        return;
    }
    const delay = RECONNECT_BACKOFF[Math.min(reconnectAttempt, RECONNECT_BACKOFF.length - 1)];
    reconnectAttempt++;
    setConnStatus("reconnecting");
    reconnectTimer = setTimeout(() => {
        reconnectTimer = null;
        if (S.tab !== "term" || !S.current) {
            setConnStatus("closed");
            return;
        }
        connectTermWS(true);
    }, delay);
}
/* ---------------- 终端实例与 WebSocket ---------------- */
// 仅断开连接（停止容器时）：作废重连、关连接，保留终端画面与回滚缓冲。
export function termDisconnect() {
    S.termWSGen++;
    clearReconnect();
    reconnectAttempt = 0;
    if (S.termWS) {
        S.termWS.close();
        S.termWS = null;
    }
    $("term-loading").classList.add("hidden");
    setConnStatus("closed");
}
// 完全收尾（切换/删除会话时）：连实例一起销毁。
export function termTeardown() {
    termDisconnect();
    if (S.term) {
        S.term.dispose();
        S.term = null;
        S.fit = null;
    }
}
// 手动重连：作废旧连接与在途重连，保留已有终端实例（不丢回滚），立即重新连接。
export function termReconnect() {
    if (!S.current)
        return;
    S.termWSGen++;
    clearReconnect();
    reconnectAttempt = 0;
    if (S.termWS) {
        try {
            S.termWS.close();
        }
        catch (_) { }
        S.termWS = null;
    }
    ensureTerm();
    if (!S.term)
        return;
    connectTermWS(false);
}
// Chrome（macOS 实测）对聚焦中的 textarea 切换过 readOnly（disableStdin 的底层实现）
// 后，IME 可能就此失灵——打拼音直接上屏字母，点页面别处再点回终端才恢复。这里主动做
// 一轮 blur→focus 刷新 IME 上下文；textarea 本就没聚焦时直接 focus，与原行为一致。
function refocusTerm() {
    if (!S.term)
        return;
    const ta = S.term.textarea;
    if (ta && document.activeElement === ta) {
        ta.blur();
        requestAnimationFrame(() => { if (S.term)
            S.term.focus(); });
    }
    else {
        S.term.focus();
    }
}
// 创建 xterm 实例（若尚不存在）：渲染器、图片路径链接、输入/尺寸回调都在这里挂一次，
// 之后跨重连复用同一实例，回调动态读取 S.termWS，避免重复叠加监听。
function ensureTerm() {
    if (S.term)
        return;
    if (!TerminalClass) {
        $("term-loading").classList.add("hidden");
        setConnStatus("closed");
        $("term-state").textContent = "xterm.js 加载失败";
        return;
    }
    const term = new TerminalClass({
        fontFamily: "JetBrains Mono, Menlo, Consolas, monospace",
        fontSize: isMobile() ? 12 : 13, // 窄屏降一号，约 46 列
        cursorBlink: true,
        // 配色取 css/base.css 的 --term-* 令牌（深浅主题下都是深色，见那里的说明）
        theme: {
            background: token("--term-bg"),
            foreground: token("--term-fg"),
            cursor: token("--term-cursor"),
            selectionBackground: token("--term-sel"),
        },
    });
    const fit = new FitAddonClass();
    term.loadAddon(fit);
    $("term-mount").replaceChildren();
    term.open($("term-mount"));
    // GPU 渲染器：方块/半格字形按整格实心填充（customGlyphs 默认开），
    // 像素图（如 Claude 小人）与制表线严丝合缝，接近原生终端的细腻度。
    // 必须在 term.open 之后加载；WebGL 不可用或上下文丢失时自动回退默认 DOM 渲染。
    if (WebglAddonClass) {
        try {
            const webgl = new WebglAddonClass();
            webgl.onContextLoss(() => webgl.dispose());
            term.loadAddon(webgl);
        }
        catch (_) { /* 保持默认 DOM 渲染 */ }
    }
    fit.fit();
    S.term = term;
    S.fit = fit;
    term.onData((d) => {
        if (S.termWS && S.termWS.readyState === WebSocket.OPEN)
            S.termWS.send(ENC.encode(d));
    });
    term.onResize(({ cols, rows }) => {
        if (S.termWS && S.termWS.readyState === WebSocket.OPEN) {
            S.termWS.send(JSON.stringify({ type: "resize", cols, rows }));
        }
    });
    // 终端里出现的 /shared/.images/ 路径可点击弹出图片预览
    if (term.registerLinkProvider) {
        term.registerLinkProvider({
            provideLinks(y, cb) {
                const line = term.buffer.active.getLine(y - 1);
                if (!line)
                    return cb(undefined);
                const text = line.translateToString(true);
                const links = [];
                for (const m of text.matchAll(IMG_PATH_RE)) {
                    links.push({
                        range: { start: { x: m.index + 1, y }, end: { x: m.index + m[0].length, y } },
                        text: m[0],
                        activate: (_e, p) => openLightbox(imgURLFromPath(p), p),
                    });
                }
                cb(links.length ? links : undefined);
            },
        });
    }
}
// 拉起 PTY WebSocket。isReconnect 为 true 时不弹大遮罩（终端画面还在），只在顶栏提示重连中。
function connectTermWS(isReconnect) {
    const sess = S.current;
    if (!sess || !S.term)
        return;
    clearReconnect();
    const gen = ++S.termWSGen; // 本次连接的代际；旧连接的 onopen/onclose 据此作废
    if (!isReconnect) {
        $("term-loading").classList.remove("hidden"); // 大遮罩仅首次/手动连接时出现
    }
    setConnStatus(isReconnect ? "reconnecting" : "connecting");
    const ws = new WebSocket(wsURL(`/sessions/${sess.id}/term`));
    ws.binaryType = "arraybuffer";
    S.termWS = ws;
    ws.onopen = () => {
        if (gen !== S.termWSGen) {
            ws.close();
            return;
        } // 已被更新的连接取代
        reconnectAttempt = 0;
        S.term.write(RESET_INPUT_MODES); // 复位上一个程序遗留的鼠标上报/备用屏等模式，再接新 bash
        $("term-loading").classList.add("hidden");
        setConnStatus("connected");
        ws.send(JSON.stringify({ type: "resize", cols: S.term.cols, rows: S.term.rows }));
        if (S.fit)
            S.fit.fit();
        refocusTerm(); // 重连场景顺带刷新 IME 上下文
        refreshAll(); // 终端会自动拉起容器，刷新状态灯
    };
    ws.onmessage = (e) => {
        if (!S.term)
            return;
        S.term.write(typeof e.data === "string" ? e.data : new Uint8Array(e.data));
    };
    ws.onclose = (e) => {
        if (gen !== S.termWSGen)
            return; // 被 teardown/disconnect/手动重连接管，忽略
        S.termWS = null;
        $("term-loading").classList.add("hidden"); // 连接失败时不能留着转圈
        // 4000–4999 是服务端的应用私有关闭码（目前只有额度用尽一种）：reason 是写给
        // 人看的一句话，写进终端画面并顶到状态栏。绝不自动重连——理由不会因为重试
        // 而改变，只会被同一句话再挡一次。
        if (e && e.code >= 4000 && e.code < 5000) {
            const why = e.reason || "连接已被服务端拒绝";
            if (S.term)
                S.term.write("\r\n\x1b[31m" + why + "\x1b[0m\r\n");
            setConnStatus("closed");
            $("term-state").textContent = why;
            return;
        }
        // 干净关闭（1000，服务端在 shell 进程退出时发）意味着：用户主动 exit，
        // 或本终端被另一处打开的终端顶掉（tmux attach -D）。这两种都不该自动重连
        // ——尤其后者，自动重连会把对方再顶掉，两个页面无限拉锯。停在「已断开」，
        // 想连点右侧按钮。网络类异常断开（1006 等）才走自动重连。
        if (e && e.code === 1000) {
            setConnStatus("closed");
            return;
        }
        scheduleReconnect();
    };
}
// 进入终端页时调用：已有活动连接则只重排字形，否则（新开 / 断开后重进）拉起 shell。
export function openTerm() {
    const sess = S.current;
    if (!sess)
        return;
    // 连接中或已连上：不重建，避免切标签页把正在跑的会话打断，只需重新贴合尺寸
    if (S.termWS && S.termWS.readyState <= WebSocket.OPEN) {
        if (S.fit)
            requestAnimationFrame(() => S.fit.fit());
        return;
    }
    clearReconnect();
    reconnectAttempt = 0;
    ensureTerm();
    if (!S.term)
        return; // xterm 加载失败，ensureTerm 已提示
    connectTermWS(false);
}
$("term-reconnect").addEventListener("click", termReconnect);
window.addEventListener("resize", () => {
    if (S.fit && S.tab === "term")
        S.fit.fit();
});
/* ---------------- 顶栏提示轮播 ----------------
 * 提示语与频率/动画在系统设置「界面与提示」里配置，经 /me 下发给所有用户（S.termTips）。
 * 单行视窗 + 纵向轨道：每隔 interval 逐条上移一行，末尾追加首条克隆做无缝回卷。
 * animation 目前只有 "scroll"（滚动），作为字段保留以便日后扩展滑动/翻转等。 */
let tipsTimer = null;
function initTermTips() {
    const host = $("term-tips");
    const wrap = $("term-tip-wrap");
    if (!host || !wrap)
        return;
    if (tipsTimer) {
        clearInterval(tipsTimer);
        tipsTimer = null;
    }
    host.replaceChildren();
    const cfg = S.termTips || {};
    const tips = Array.isArray(cfg.tips) ? cfg.tips.filter((t) => t && t.trim()) : [];
    wrap.classList.toggle("hidden", tips.length === 0);
    if (!tips.length)
        return;
    const track = document.createElement("div");
    track.className = "term-tips-track anim-" + (cfg.animation || "scroll");
    for (const t of tips) {
        track.appendChild(Object.assign(document.createElement("div"), { className: "term-tip", textContent: t }));
    }
    host.appendChild(track);
    const interval = Math.floor(cfg.interval_sec || 0) * 1000;
    if (tips.length <= 1 || interval <= 0)
        return; // 单条或频率 0：静止显示第一条
    // 末尾克隆首条，滚到它时无缝跳回顶部
    track.appendChild(track.firstElementChild.cloneNode(true));
    let idx = 0;
    tipsTimer = setInterval(() => {
        const h = host.clientHeight;
        if (!h)
            return; // 窄屏隐藏（display:none）时不动
        idx++;
        track.style.transition = "transform .5s ease";
        track.style.transform = `translateY(${-idx * h}px)`;
        if (idx >= tips.length) {
            setTimeout(() => {
                track.style.transition = "none";
                track.style.transform = "translateY(0)";
                idx = 0;
            }, 520);
        }
    }, interval);
}
bus.addEventListener("tips-updated", initTermTips);
/* ---------------- 顶栏「本会话已花」 ----------------
 * 会话内的全部消耗：网页对话 + 终端手敲 + 起标题，也就是使用记录里按会话筛出来的
 * 那份合计。数据走 /usage/events 的 total（服务端按筛选条件算，与翻页无关），
 * 顺带触发一次终端消耗补记，所以在终端里刚花完的量这里也是最新的。
 *
 * 订阅账号下这个金额是「按 API 价折算的等价金额」而非真实支出；终端行还可能因为
 * 价目表里没配 claude 而算不出钱——所以 token 也一并显示，金额为 0 时它才是唯一
 * 有信息量的那个数。 */
const SPEND_POLL_MS = 15000;
let spendTimer = null;
let spendFor = ""; // 当前显示的是哪个会话的数，切会话时先清空免得串台
function fmtTok(n) {
    if (n >= 1e6)
        return (n / 1e6).toFixed(n >= 1e7 ? 0 : 1) + "M";
    if (n >= 1e3)
        return (n / 1e3).toFixed(n >= 1e4 ? 0 : 1) + "K";
    return String(n);
}
async function loadSpend() {
    const sess = S.current;
    const box = $("term-spend");
    if (!sess) {
        box.classList.add("hidden");
        return;
    }
    if (spendFor !== sess.id) {
        box.replaceChildren();
        spendFor = sess.id;
    }
    let total;
    try {
        const data = await api("/usage/events?limit=1&session=" + encodeURIComponent(sess.id));
        total = data.total;
    }
    catch (_) {
        return; // 拉不到就保持上一次的数，顶栏不该因为这个闪
    }
    if (S.current?.id !== sess.id)
        return; // 请求在途时用户切走了
    const tokens = total.input_tokens + total.output_tokens
        + total.cache_read_tokens + total.cache_write_tokens;
    if (!total.rows) {
        box.classList.add("hidden");
        return;
    }
    const label = Object.assign(document.createElement("span"), {
        className: "ts-label", textContent: "本会话已花",
    });
    const cost = Object.assign(document.createElement("span"), {
        className: "ts-cost", textContent: fmtUSD(total.cost_micro_usd, 2),
    });
    const tok = Object.assign(document.createElement("span"), {
        className: "ts-tok", textContent: fmtTok(tokens) + " tok",
    });
    box.replaceChildren(label, cost, tok);
    box.title = `${total.turns} 个回合 / ${total.rows} 条记录，含网页对话、终端和起标题。`
        + "金额按价目表与 provider 报价折算，订阅账号下只是等价估算；终端消耗计入这里但不扣额度。";
    box.classList.remove("hidden");
}
/** 进出终端页时开关轮询：不在终端页就没必要一直问。 */
export function termSpendPolling(on) {
    if (spendTimer) {
        clearInterval(spendTimer);
        spendTimer = null;
    }
    if (!on)
        return;
    void loadSpend();
    spendTimer = setInterval(() => void loadSpend(), SPEND_POLL_MS);
}
/* 终端粘贴图片：上传后把容器内路径写入 PTY（capture 阶段拦截，避免 xterm 处理）。
 * 上传期间整个终端页盖遮罩转圈，并通过 disableStdin 禁止键入，防止用户不知道发生了什么。 */
const termUpload = { total: 0, done: 0 };
function termUploadUI() {
    const on = termUpload.total > 0;
    $("term-overlay").classList.toggle("hidden", !on);
    if (on) {
        $("term-overlay-text").textContent = termUpload.total > 1
            ? `图片上传中… (${termUpload.done + 1}/${termUpload.total})`
            : "图片上传中…";
    }
    if (S.term) {
        S.term.options.disableStdin = on;
        if (!on)
            refocusTerm(); // disableStdin 切过 readOnly，须刷新 IME 上下文
    }
}
$("term-mount").addEventListener("paste", (e) => {
    const files = pastedImages(e);
    if (!files.length || !S.current)
        return;
    e.preventDefault();
    e.stopPropagation();
    (async () => {
        termUpload.total += files.length;
        termUploadUI();
        for (const f of files) {
            try {
                const res = await uploadAttachment(f);
                if (S.termWS && S.termWS.readyState === WebSocket.OPEN) {
                    S.termWS.send(ENC.encode(res.path + " "));
                }
            }
            catch (err) {
                if (S.term)
                    S.term.write(`\r\n\x1b[31m图片上传失败: ${err.message}\x1b[0m\r\n`);
            }
            termUpload.done++;
            if (termUpload.done === termUpload.total) {
                termUpload.total = 0;
                termUpload.done = 0;
            }
            termUploadUI();
        }
    })();
}, true);
