import { setAttrRender, setText, setTextRender, t as i18nText } from "./i18n.js";
/* term：终端页 —— 终端标签（主终端 + 独立终端）、各自的 xterm 实例与 PTY WebSocket（含自动重连）、
 * 连接状态与顶栏提示轮播、终端内粘贴图片上传。 */
"use strict";
import { S, bus } from "./state.js";
import { $, askConfirm, isMobile, openLightbox, toast } from "./util.js";
import { api, wsURL, imgURLFromPath, uploadAttachment } from "./api.js";
import { fmtUSD } from "./quota.js";
import { fmtTokens } from "./usage-math.js";
import { refreshAll } from "./data.js";
import { pastedImages } from "./chat.js";
import { setTip } from "./tip.js";
import { attachTermInputDebug, traceTermEvent, traceTermInput } from "./term-input-debug.js";
import { attachTouchScroll } from "./term-touch.js";
import { attachTermClipboard, resetTermClipboard } from "./term-clipboard.js";
import { bindMenu, menuJustDismissed, openMenuAt } from "./menu.js";
import { svgIcon } from "./icons.js";
import { agentKey, agentName } from "./brand.js";
import { enter, leave } from "./motion.js";
import { usageLiveConnected, watchUsageLive } from "./usage-live.js";
import { chipRefresher, openKeepalive, termChipState } from "./features/workspaces/keepalive.js";
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
// 配色取 css/base.css 的 --term-* 令牌（深浅主题下都是深色，见那里的说明）；各界面风格只换
// 底色色调、光标和选区。xterm 只在创建时读一次，换风格/明暗时由 theme.ts 的事件重新下发。
const termTheme = () => ({
    background: token("--term-bg"),
    foreground: token("--term-fg"),
    cursor: token("--term-cursor"),
    selectionBackground: token("--term-sel"),
});
window.addEventListener("agentbox-theme-change", () => {
    for (const p of panes.values())
        if (p.term)
            p.term.options.theme = termTheme();
});
const MAIN = "main";
const panes = new Map();
let active = null;
let panesFor = ""; // 这些标签属于哪个空间；换空间时整体收尾
let listGen = 0; // 独立终端列表的请求代际，旧空间/旧请求的结果丢弃
const termOf = () => active?.term ?? null;
const AGENT_SHORT = { claude: "Claude", codex: "Codex" };
function paneLabel(p) {
    if (p.kind === "main")
        return i18nText("主终端");
    // 标签放不下「Claude Code」「Codex CLI」全名，用短名；新建菜单里仍是全名
    const name = p.kind === "agent" ? AGENT_SHORT[agentKey(S.current?.agent || "")] : "Shell";
    const same = [...panes.values()].filter((x) => x.kind === p.kind);
    const label = `${name} ${same.indexOf(p) + 1}`;
    return p.project ? `${label} · ${p.project}` : label;
}
function addPane(key, kind, project) {
    const host = document.createElement("div");
    host.className = "term-pane hidden";
    $("term-mount").append(host);
    const tab = document.createElement("div");
    tab.className = "term-tab";
    tab.setAttribute("role", "presentation");
    const p = { key, kind, project, host, tab, term: null, fit: null, ws: null, gen: 0, timer: null, attempt: 0, state: "closed", refused: "" };
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "term-tab-btn";
    btn.setAttribute("role", "tab");
    btn.append(svgIcon(kind === "agent" ? "term.agent" : "term.shell", 14), document.createElement("span"));
    btn.addEventListener("click", () => activatePane(p));
    tab.append(btn);
    if (kind !== "main") {
        const x = document.createElement("button");
        x.type = "button";
        x.className = "term-tab-x";
        x.append(svgIcon("term.close", 12));
        setAttrRender(x, "aria-label", () => i18nText("关闭 {name}", { name: paneLabel(p) }));
        setTip(x, () => i18nText("关闭终端"));
        x.addEventListener("click", () => void closePane(p));
        tab.append(x);
    }
    panes.set(key, p);
    return p;
}
function removePane(p) {
    dropPane(p);
    p.term?.dispose();
    p.host.remove();
    p.tab.remove();
    panes.delete(p.key);
    if (active === p) {
        active = null;
        const main = panes.get(MAIN);
        if (main && S.tab === "term")
            activatePane(main);
        else {
            active = main ?? null;
            S.term = active?.term ?? null;
            S.fit = active?.fit ?? null;
            renderConn();
        }
    }
}
function renderTabs() {
    $("term-tablist").replaceChildren(...[...panes.values()].map((p) => p.tab));
    for (const p of panes.values()) {
        const btn = p.tab.querySelector(".term-tab-btn");
        setTextRender(btn.querySelector("span"), () => paneLabel(p));
        btn.setAttribute("aria-selected", String(p === active));
        p.tab.classList.toggle("active", p === active);
        p.host.classList.toggle("hidden", p !== active);
    }
}
/* ---------------- 连接状态展示（顶栏右侧：状态点 + 文案 + 重连钮），只显示当前标签 ---------------- */
function setConnStatus(p, state) {
    p.state = state;
    if (state !== "closed")
        p.refused = "";
    if (p === active)
        renderConn();
}
function renderConn() {
    let state = active?.state ?? "closed";
    syncTermKeys(state === "connected");
    // 大遮罩只在首次/手动连接时出现；重连时终端画面还在，只在顶栏提示
    $("term-loading").classList.toggle("hidden", state !== "connecting");
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
            text = (active?.attempt ?? 0) > 1 ? "重连中…（第 {p0} 次）" : "重连中…";
            break;
        default:
            cls = "bad";
            state = "closed";
            text = "已断开，点右侧按钮重连";
            break;
    }
    if (dot)
        dot.className = "t-dot " + cls;
    // Bind only this generic label. An explicit diagnostic replacing it owns its
    // text, so switching language cannot overwrite a raw server refusal reason.
    setText(st, text, { p0: String(active?.attempt ?? 0) });
    if (active?.refused)
        st.textContent = active.refused;
    st.dataset.state = state;
    if (btn)
        btn.classList.toggle("busy", state === "connecting" || state === "reconnecting");
}
/* ---------------- 自动重连 ---------------- */
// 退避序列（毫秒），封顶后一直用最后一档；成功连上或手动重连时归零。
const RECONNECT_BACKOFF = [1000, 2000, 3000, 5000, 8000, 10000];
function clearReconnect(p) {
    if (p.timer) {
        clearTimeout(p.timer);
        p.timer = null;
    }
}
// 断线后排一次重连——仅当用户仍停留在终端页时才自动重连，切走了就停在「已断开」，
// 回到终端页（setTab → openTerm）会重新拉起。后台标签照样重连，切回来时画面是新的。
function scheduleReconnect(p) {
    if (S.tab !== "term" || !S.current) {
        setConnStatus(p, "closed");
        return;
    }
    const delay = RECONNECT_BACKOFF[Math.min(p.attempt, RECONNECT_BACKOFF.length - 1)];
    p.attempt++;
    setConnStatus(p, "reconnecting");
    p.timer = setTimeout(() => {
        p.timer = null;
        if (S.tab !== "term" || !S.current || panes.get(p.key) !== p) {
            setConnStatus(p, "closed");
            return;
        }
        connectPane(p, true);
    }, delay);
}
/* ---------------- 终端实例与 WebSocket ---------------- */
// 作废这个标签的连接与在途重连，保留画面与回滚缓冲。
function dropPane(p) {
    p.gen++;
    clearReconnect(p);
    p.attempt = 0;
    if (p.ws) {
        try {
            p.ws.close();
        }
        catch (_) { }
        p.ws = null;
    }
}
// 仅断开连接（停止容器时）：作废重连、关连接，保留终端画面与回滚缓冲。
export function termDisconnect() {
    for (const p of panes.values()) {
        dropPane(p);
        setConnStatus(p, "closed");
    }
    if (!active)
        renderConn();
}
// 完全收尾（切换/删除会话时）：连实例和标签一起销毁。
export function termTeardown() {
    termDisconnect();
    resetTermClipboard();
    for (const p of panes.values()) {
        p.term?.dispose();
        p.host.remove();
    }
    panes.clear();
    active = null;
    S.term = S.fit = null;
    panesFor = "";
    listGen++;
    $("term-tablist").replaceChildren();
    renderConn();
    fitTermViewport();
}
// 手动重连：作废旧连接与在途重连，保留已有终端实例（不丢回滚），立即重新连接当前标签。
export function termReconnect() {
    if (!S.current)
        return;
    if (!active || panesFor !== S.current.id) {
        openTerm();
        return;
    }
    dropPane(active);
    activatePane(active);
}
// Chrome（macOS 实测）对聚焦中的 textarea 切换过 readOnly（disableStdin 的底层实现）
// 后，IME 可能就此失灵——打拼音直接上屏字母，点页面别处再点回终端才恢复。这里主动做
// 一轮 blur→focus 刷新 IME 上下文；textarea 本就没聚焦时直接 focus，与原行为一致。
function refocusTerm() {
    const term = termOf();
    if (!term)
        return;
    const ta = term.textarea;
    if (ta && document.activeElement === ta) {
        ta.blur();
        requestAnimationFrame(() => termOf()?.focus());
    }
    else {
        term.focus();
    }
}
/* iOS Safari 的中文键盘直接上屏的标点（「，」等）会被 xterm 6 丢掉。xterm 只在三处发送：
 * keydown（要求可打印键码）、keypress、以及键码 229 时 setTimeout(0) 里的 textarea 差分；
 * 而 iOS 可能不给可打印键码、不发 keypress，且把字写进 textarea 晚于那次差分；最后到达的
 * insertText 输入事件又因「已见过 keydown」被 _inputEvent 跳过（上游 xtermjs/xterm.js#3070，
 * 修复 PR #5614 未合并）。这里等 xterm 所有发送时机都过去再看：从这次按键开始 xterm 一个字
 * 都没发、也不在组字，才由我们补发，因此不会重复。监听挂在外层捕获阶段，先于 xterm 记数。
 *
 * 连按同一个标点键时 iOS 在「，。？！」间循环：不发按键事件，直接把刚上屏的符号删掉或替换
 * 成下一个。xterm 只认插入、不认删除，终端里就剩下「，。」。所以补发前先比对 textarea 改动
 * 前后的内容，删了几个字就先发几个退格。 */
let dataSeq = 0;
const MAX_BRIDGE_DELETE = 8; // 超出就不是循环标点这类小改动，宁可少删也不误删用户的输入
/** 两段文本的差异：从末尾起删掉几个字符（按码点）、插入了什么 */
function textEdit(before, after) {
    const a = [...before], b = [...after];
    let p = 0;
    while (p < a.length && p < b.length && a[p] === b[p])
        p++;
    let s = 0;
    while (s < a.length - p && s < b.length - p && a[a.length - 1 - s] === b[b.length - 1 - s])
        s++;
    return { removed: a.length - p - s, inserted: b.slice(p, b.length - s).join("") };
}
function bridgeDroppedInput(host, ta) {
    let mark = -1; // 本次按键开始时的 dataSeq，-1 表示没有待确认的按键
    let keyCode = 0;
    let composing = false;
    let before = null; // beforeinput 时的 textarea 内容
    let queued = 0;
    const send = (out) => { traceTermInput("bridge", out); sendTermInput(modifiedTermInput(out)); };
    // 晚于 xterm 在 keydown/compositionend 时排下的 setTimeout(0)，同延迟的定时器按登记顺序触发。
    const later = (from, out) => {
        queued++;
        setTimeout(() => {
            queued--;
            if (dataSeq === from && !composing)
                send(out);
        }, 0);
    };
    const on = (type, fn) => host.addEventListener(type, (e) => { if (e.target === ta)
        fn(e); }, true);
    on("keydown", (e) => { mark = dataSeq; keyCode = e.keyCode; });
    // xterm 已为这次按键发过数据就结束；还没发（iOS 晚到的输入）则留着等输入事件。
    on("keyup", () => { if (mark !== dataSeq)
        mark = -1; });
    on("compositionstart", () => { composing = true; });
    on("compositionend", () => { composing = false; });
    on("beforeinput", () => { before = ta.value; });
    on("input", (e) => {
        const keyed = mark >= 0;
        const from = keyed ? mark : dataSeq;
        const prev = before;
        mark = -1;
        before = null;
        if (e.isComposing || composing)
            return;
        const edit = prev === null ? null : textEdit(prev, ta.value);
        let removed = edit && edit.removed <= MAX_BRIDGE_DELETE ? edit.removed : 0;
        let data = "";
        if (e.inputType === "insertText" && e.data) {
            data = e.data;
            if (edit?.inserted !== data)
                removed = 0;
        }
        else if (e.inputType !== "deleteContentBackward" || edit?.inserted !== "")
            return;
        const del = "\x7f".repeat(removed);
        if (!del && !data)
            return;
        // 键码 229 时 xterm 的差分定时器可能也为这次改动发退格或文字，整体等它跑完再决定。
        if (keyed && keyCode === 229) {
            later(from, del + data);
            return;
        }
        // xterm 从不发删除：立即发出，赶在它可能同步发出的这次插入之前（循环标点的删除与插入
        // 常是相邻两个输入事件）。插入照旧等 xterm 的时机都过去再看要不要补。
        if (del && dataSeq === from) {
            if (queued)
                later(from, del);
            else
                send(del);
        }
        if (data)
            later(from, data);
    });
}
// 创建这个标签的 xterm 实例（若尚不存在）：渲染器、图片路径链接、输入/尺寸回调都在这里挂一次，
// 之后跨重连复用同一实例，回调动态读取 p.ws，避免重复叠加监听。
function ensureTerm(p) {
    if (p.term)
        return;
    if (!TerminalClass) {
        setConnStatus(p, "closed");
        setText($("term-state"), "xterm.js 加载失败");
        return;
    }
    const term = new TerminalClass({
        fontFamily: "JetBrains Mono, Menlo, Consolas, monospace",
        fontSize: isMobile() ? 12 : 13, // 窄屏降一号，约 46 列
        cursorBlink: true,
        theme: termTheme(),
    });
    const fit = new FitAddonClass();
    term.loadAddon(fit);
    p.host.replaceChildren();
    term.open(p.host);
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
    p.term = term;
    p.fit = fit;
    fitTermViewport();
    attachTermClipboard(term, refocusTerm);
    if (term.element && term.textarea) {
        bridgeDroppedInput(term.element, term.textarea);
        attachTermInputDebug(term.element, term.textarea);
    }
    attachTouchScroll(term, {
        longPress: (x, y) => {
            traceTermEvent("longpress", { ios: systemConfirmsPaste() });
            if (termKeys.disabled)
                return;
            if (systemConfirmsPaste())
                showPasteHint(x, y);
            else
                openPasteMenu(x, y);
        },
        longPressEnd: (released, x, y) => {
            traceTermEvent("longpress-end", { released, hint: !!pasteHint });
            if (!pasteHint)
                return;
            hidePasteHint();
            if (released && !termKeys.disabled)
                void pasteOnRelease(x, y);
        },
        swallowTap: menuJustDismissed,
    });
    term.onData((d) => {
        if (p !== active)
            return; // 只有当前标签收得到键盘，防御一下
        dataSeq++;
        traceTermInput("xterm", d);
        // 焦点上报不是用户按键，不能消耗一次性的 Ctrl / Alt。
        if (d === "\x1b[I" || d === "\x1b[O") {
            sendTermInput(d);
        }
        else {
            sendTermInput(modifiedTermInput(d));
        }
    });
    term.onResize(({ cols, rows }) => {
        if (p.ws && p.ws.readyState === WebSocket.OPEN) {
            p.ws.send(JSON.stringify({ type: "resize", cols, rows }));
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
function paneURL(p, sessID) {
    return p.kind === "main" ? `/sessions/${sessID}/term` : `/sessions/${sessID}/client-terminals/${p.key}/stream`;
}
// 拉起 PTY WebSocket。isReconnect 为 true 时不弹大遮罩（终端画面还在），只在顶栏提示重连中。
function connectPane(p, isReconnect) {
    const sess = S.current;
    if (!sess || !p.term)
        return;
    clearReconnect(p);
    const gen = ++p.gen;
    setConnStatus(p, isReconnect ? "reconnecting" : "connecting");
    const ws = new WebSocket(wsURL(paneURL(p, sess.id)));
    ws.binaryType = "arraybuffer";
    p.ws = ws;
    ws.onopen = () => {
        if (gen !== p.gen) {
            ws.close();
            return;
        } // 已被更新的连接取代
        p.attempt = 0;
        p.term.write(RESET_INPUT_MODES); // 复位上一个程序遗留的鼠标上报/备用屏等模式，再接新 bash
        setConnStatus(p, "connected");
        ws.send(JSON.stringify({ type: "resize", cols: p.term.cols, rows: p.term.rows }));
        if (p === active) {
            p.fit?.fit();
            refocusTerm(); // 重连场景顺带刷新 IME 上下文
        }
        refreshAll(); // 终端会自动拉起容器，刷新状态灯
    };
    ws.onmessage = (e) => {
        p.term?.write(typeof e.data === "string" ? e.data : new Uint8Array(e.data));
    };
    ws.onclose = (e) => {
        if (gen !== p.gen)
            return; // 被 teardown/disconnect/手动重连接管，忽略
        p.ws = null;
        // 独立终端可能已在别处（另一个页面、桌面客户端）关掉，重新对一次列表，关掉的标签直接收起
        if (p.kind !== "main")
            void syncPanes();
        // 4000–4999 是服务端的应用私有关闭码（额度用尽、账号不可用、独立终端已关闭）：reason 是写给
        // 人看的一句话，写进终端画面并顶到状态栏。绝不自动重连——理由不会因为重试
        // 而改变，只会被同一句话再挡一次。
        if (e && e.code >= 4000 && e.code < 5000) {
            const why = e.reason || i18nText("连接已被服务端拒绝");
            p.term?.write("\r\n\x1b[31m" + why + "\x1b[0m\r\n");
            p.refused = why;
            setConnStatus(p, "closed");
            return;
        }
        // 干净关闭（1000，服务端在 shell 进程退出时发）意味着：用户主动 exit，
        // 或本终端被另一处打开的终端顶掉（tmux attach -D / -d）。这两种都不该自动重连
        // ——尤其后者，自动重连会把对方再顶掉，两个页面无限拉锯。停在「已断开」，
        // 想连点右侧按钮。网络类异常断开（1006 等）才走自动重连。
        if (e && e.code === 1000) {
            setConnStatus(p, "closed");
            return;
        }
        scheduleReconnect(p);
    };
}
// 切到某个标签：第一次切到才创建 xterm 并连接；已连上只重排尺寸，断开了（未在等重连）就重新连。
function activatePane(p) {
    active = p;
    renderTabs();
    renderConn();
    ensureTerm(p);
    S.term = p.term;
    S.fit = p.fit;
    if (!p.term)
        return; // xterm 加载失败，ensureTerm 已提示
    if ((p.ws && p.ws.readyState <= WebSocket.OPEN) || p.timer) {
        requestAnimationFrame(() => {
            if (active !== p)
                return;
            p.fit?.fit();
            refocusTerm();
        });
        return;
    }
    p.attempt = 0;
    connectPane(p, false);
}
// 进入终端页时调用：已有活动连接则只重排字形，否则（新开 / 断开后重进）拉起 shell。
export function openTerm() {
    const sess = S.current;
    if (!sess)
        return;
    if (panesFor !== sess.id) {
        termTeardown();
        panesFor = sess.id;
        active = addPane(MAIN, "main", "");
    }
    void syncPanes();
    activatePane(active ?? panes.get(MAIN));
}
// 对齐服务端的独立终端列表：新出现的（别处开的）加标签，已关闭的收起。旧服务端没有这组接口时只留主终端。
async function syncPanes() {
    const sess = S.current;
    if (!sess)
        return;
    const gen = ++listGen;
    let terms, projects;
    try {
        [terms, projects] = await Promise.all([
            api(`/sessions/${sess.id}/client-terminals`),
            api(`/sessions/${sess.id}/client-projects`),
        ]);
    }
    catch (_) {
        return;
    }
    if (gen !== listGen || panesFor !== sess.id || !Array.isArray(terms) || !Array.isArray(projects))
        return;
    const names = new Map(projects.map((x) => [x.id, x.path === "." ? "" : x.name]));
    const open = terms.filter((t) => t.state === "open" && (t.kind === "shell" || t.kind === "agent"));
    const keep = new Set(open.map((t) => t.id));
    for (const p of [...panes.values()]) {
        if (p.kind !== "main" && !keep.has(p.key))
            removePane(p);
    }
    for (const t of open) {
        if (!panes.has(t.id))
            addPane(t.id, t.kind, names.get(t.project_id) ?? "");
    }
    renderTabs();
}
const JSON_HEADERS = { "Content-Type": "application/json" };
// 网页新建的独立终端都挂在工作区根目录这个项目下；没有就建一个（与桌面客户端共用，桌面里也看得到）。
async function rootProject(id) {
    const find = async () => (await api(`/sessions/${id}/client-projects`)).find((x) => x.path === ".");
    const found = await find();
    if (found)
        return found;
    try {
        return await api(`/sessions/${id}/client-projects`, {
            method: "POST", headers: JSON_HEADERS,
            body: JSON.stringify({ name: i18nText("工作区"), path: ".", arguments: [] }),
        });
    }
    catch (err) {
        const again = await find(); // 另一个页面同时建好了（同路径 409）
        if (again)
            return again;
        throw err;
    }
}
let creating = false;
async function newPane(kind) {
    const sess = S.current;
    if (!sess || creating)
        return;
    creating = true;
    try {
        const project = await rootProject(sess.id);
        const t = await api(`/sessions/${sess.id}/client-terminals`, {
            method: "POST", headers: JSON_HEADERS,
            body: JSON.stringify({ project_id: project.id, kind }),
        });
        if (panesFor !== sess.id)
            return;
        const p = panes.get(t.id) ?? addPane(t.id, kind, "");
        activatePane(p);
    }
    catch (err) {
        toast(i18nText("新建终端失败：{p0}", { p0: err.message }), true);
    }
    finally {
        creating = false;
    }
}
async function closePane(p) {
    const sess = S.current;
    if (!sess || p.kind === "main")
        return;
    const ok = await askConfirm(() => i18nText("关闭「{name}」？", { name: paneLabel(p) }), {
        get title() { return i18nText("关闭终端"); },
        get hint() { return i18nText("终端里正在运行的程序会被结束，屏幕上的内容不会保留。"); },
        danger: true,
        get okLabel() { return i18nText("关闭"); },
        icon: "term.close",
    });
    if (!ok || panes.get(p.key) !== p)
        return;
    try {
        await api(`/sessions/${sess.id}/client-terminals/${p.key}`, { method: "DELETE" });
    }
    catch (err) {
        toast(i18nText("关闭终端失败：{p0}", { p0: err.message }), true);
        return;
    }
    if (panes.get(p.key) === p) {
        removePane(p);
        renderTabs();
    }
}
$("term-reconnect").addEventListener("click", termReconnect);
bindMenu($("term-new"), () => {
    const agent = agentName(S.current?.agent || "");
    return [
        { label: i18nText("Shell 终端"), icon: "term.shell", tip: i18nText("在工作区根目录新开一个 Shell"), run: () => void newPane("shell") },
        { label: i18nText("{agent} 终端", { agent }), icon: "term.agent", tip: i18nText("在工作区根目录直接启动 {agent}", { agent }), run: () => void newPane("agent") },
    ];
});
// 侧栏收起也会改变终端宽度，但不会触发 window.resize。
new ResizeObserver(([entry]) => {
    if (active?.fit && S.tab === "term" && entry.contentRect.width && entry.contentRect.height)
        active.fit.fit();
}).observe($("term-mount"));
/* ---------------- 手机辅助键 ---------------- */
const termKeys = $("term-keys");
const termModifiers = { ctrl: false, alt: false };
let keysConnected = false;
function resetTermModifiers() {
    termModifiers.ctrl = termModifiers.alt = false;
    renderTermModifiers();
}
function renderTermModifiers() {
    for (const button of termKeys.querySelectorAll("[data-term-mod]")) {
        const mod = button.dataset.termMod;
        button.setAttribute("aria-pressed", String(termModifiers[mod]));
    }
}
function syncTermKeys(connected = keysConnected) {
    keysConnected = connected;
    termKeys.disabled = !connected || !!termOf()?.options.disableStdin;
    if (termKeys.disabled)
        resetTermModifiers();
}
function sendTermInput(data) {
    const ws = active?.ws;
    if (!active?.term || active.term.options.disableStdin || !ws || ws.readyState !== WebSocket.OPEN)
        return;
    ws.send(ENC.encode(data));
}
function modifiedTermInput(data) {
    const { ctrl, alt } = termModifiers;
    if (!ctrl && !alt)
        return data;
    resetTermModifiers();
    // 只组合单个 ASCII 按键，中文输入和整段粘贴原样通过。
    if (data.length !== 1 || data.charCodeAt(0) > 127)
        return data;
    if (ctrl) {
        const code = data.toUpperCase().charCodeAt(0);
        if (code >= 64 && code <= 95)
            data = String.fromCharCode(code - 64);
        else if (data === " ")
            data = "\x00";
        else if (data === "?")
            data = "\x7f";
    }
    return (alt ? "\x1b" : "") + data;
}
// 阻止按钮抢走 textarea 焦点；触摸仍允许横向滚动，点击后同步 focus 可唤起 iOS 键盘。
termKeys.addEventListener("mousedown", (event) => event.preventDefault());
termKeys.addEventListener("click", (event) => {
    const button = event.target.closest("button");
    const term = termOf();
    if (!button || termKeys.disabled || !term)
        return;
    if (button.id === "term-keyboard") {
        resetTermModifiers();
        if (document.activeElement === term.textarea)
            term.textarea?.blur();
        else
            term.focus();
        return;
    }
    const mod = button.dataset.termMod;
    if (mod === "ctrl" || mod === "alt") {
        termModifiers[mod] = !termModifiers[mod];
        renderTermModifiers();
    }
    else {
        const key = button.dataset.termKey;
        const arrows = { up: "A", down: "B", right: "C", left: "D" };
        const fixed = { escape: "\x1b", tab: "\t", "shift-tab": "\x1b[Z", interrupt: "\x03" };
        let data;
        if (arrows[key]) {
            const modifier = 1 + (termModifiers.alt ? 2 : 0) + (termModifiers.ctrl ? 4 : 0);
            data = modifier > 1 ? `\x1b[1;${modifier}${arrows[key]}`
                : `\x1b${term.modes.applicationCursorKeysMode ? "O" : "["}${arrows[key]}`;
            resetTermModifiers();
        }
        else if (key === "interrupt" || key === "shift-tab") {
            data = fixed[key];
            resetTermModifiers();
        }
        else {
            data = modifiedTermInput(fixed[key] ?? key);
        }
        sendTermInput(data);
    }
    term.focus();
});
const touchTerminal = matchMedia("(max-width: 760px), (pointer: coarse)");
function fitTermViewport() {
    const viewport = window.visualViewport;
    const on = !!termOf() && S.view === "work" && S.tab === "term" && touchTerminal.matches;
    const app = $("app");
    // 放大页面时保留浏览器的平移与缩放行为。
    app.classList.toggle("term-viewport", on && !!viewport && viewport.scale === 1);
    if (on && viewport) {
        app.style.setProperty("--term-viewport-height", `${viewport.height}px`);
        app.style.setProperty("--term-viewport-top", `${viewport.offsetTop}px`);
    }
    if (!on)
        resetTermModifiers();
}
window.visualViewport?.addEventListener("resize", fitTermViewport);
window.visualViewport?.addEventListener("scroll", fitTermViewport);
touchTerminal.addEventListener("change", fitTermViewport);
const termVisibility = new MutationObserver(() => {
    resetTermModifiers();
    fitTermViewport();
});
for (const id of ["tab-term", "view-work"]) {
    termVisibility.observe($(id), { attributes: true, attributeFilter: ["class"] });
}
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
const termKeepalive = chipRefresher(() => $("term-keepalive"), termChipState);
$("term-keepalive").addEventListener("click", () => void openKeepalive());
let spendTimer = null;
let spendFor = ""; // 当前显示的是哪个工作空间的数，切工作空间时先清空免得串台
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
        className: "ts-label",
    });
    setText(label, "本空间已花");
    const cost = Object.assign(document.createElement("span"), {
        className: "ts-cost", textContent: fmtUSD(total.cost_micro_usd, 2),
    });
    const tok = Object.assign(document.createElement("span"), {
        className: "ts-tok", textContent: fmtTokens(tokens) + " tok", // 与使用记录同一套 K/M/B 缩写
    });
    box.replaceChildren(label, cost, tok);
    // 终端里有了新请求（用量推送），补记扫描随后会给它排保活计划
    termKeepalive.soon(3000);
    setTip(box, () => i18nText("{p0} 个回合 / {p1} 条记录，含网页对话、终端和起标题。", { p0: String(total.turns), p1: String(total.rows) })
        + i18nText("合计 {p0} tokens。", { p0: tokens.toLocaleString("en-US") })
        + i18nText("金额按价目表与 provider 报价折算，订阅账号下只是等价估算；终端消耗同样扣额度，余额用完时终端里的 Agent 会被停止。"));
    box.classList.remove("hidden");
}
/** 进出终端页时开关刷新：不在终端页就没必要一直问。终端里刚花掉的量靠用量推送
 * （usage-live.ts）即时刷新；推送连着时轮询停手，断了才照旧每 15 秒问一次。 */
export function termSpendPolling(on) {
    if (spendTimer) {
        clearInterval(spendTimer);
        spendTimer = null;
    }
    watchUsageLive("term-spend", on);
    if (!on) {
        termKeepalive.reset();
        return;
    }
    termKeepalive.soon(0);
    void refreshSpend();
    spendTimer = setInterval(() => { if (!usageLiveConnected())
        void refreshSpend(); }, SPEND_POLL_MS);
}
/* 推送与轮询都走这里：同一时刻只读一次，读的途中又有变更就读完再补一次。 */
let spendLoading = false;
let spendAgain = false;
async function refreshSpend() {
    if (spendLoading) {
        spendAgain = true;
        return;
    }
    spendLoading = true;
    try {
        await loadSpend();
    }
    finally {
        spendLoading = false;
        const again = spendAgain;
        spendAgain = false;
        if (again && spendTimer)
            void refreshSpend();
    }
}
bus.addEventListener("usage-changed", e => {
    const { sessions } = e.detail;
    if (spendTimer && S.current && (!sessions || sessions.includes(S.current.id)))
        void refreshSpend();
});
/* 终端粘贴图片：上传后把容器内路径写入 PTY（capture 阶段拦截，避免 xterm 处理）。
 * 上传期间整个终端页盖遮罩转圈，并通过 disableStdin 禁止键入，防止用户不知道发生了什么。 */
const termUpload = { total: 0, done: 0 };
function termUploadUI() {
    const on = termUpload.total > 0;
    $("term-overlay").classList.toggle("hidden", !on);
    if (on) {
        setTextRender($("term-overlay-text"), () => termUpload.total > 1
            ? i18nText("图片上传中… ({p0}/{p1})", { p0: String(termUpload.done + 1), p1: String(termUpload.total) })
            : i18nText("图片上传中…"));
    }
    const term = termOf();
    if (term) {
        term.options.disableStdin = on;
        if (!on)
            refocusTerm(); // disableStdin 切过 readOnly，须刷新 IME 上下文
    }
    syncTermKeys();
}
async function uploadTermImages(files) {
    const pane = active; // 路径送回开始上传时的那个标签
    termUpload.total += files.length;
    termUploadUI();
    for (const f of files) {
        try {
            const res = await uploadAttachment(f);
            if (pane?.ws && pane.ws.readyState === WebSocket.OPEN) {
                pane.ws.send(ENC.encode(res.path + " "));
            }
        }
        catch (err) {
            pane?.term?.write(i18nText("\r\n\u001b[31m图片上传失败: {p0}\u001b[0m\r\n", { p0: String(err.message) }));
        }
        termUpload.done++;
        if (termUpload.done === termUpload.total) {
            termUpload.total = 0;
            termUpload.done = 0;
        }
        termUploadUI();
    }
}
$("term-mount").addEventListener("paste", (e) => {
    const files = pastedImages(e);
    if (!files.length || !S.current)
        return;
    e.preventDefault();
    e.stopPropagation();
    void uploadTermImages(files);
}, true);
/* iPhone / iPad 上网页每次读剪贴板，系统都会在手指旁再弹一个「粘贴」气泡确认（只有本站自己
 * 写进去的内容例外），先弹自己的菜单就得点两次，所以那边长按时不弹菜单，只提示「松开粘贴」：
 * - 按在输入光标附近、键盘开着时，iOS 会在 0.5 秒多后接管这次长按（网页收到 touchcancel），
 *   抬手弹原生编辑菜单，点「粘贴」一次就粘上——这时什么都不做，别跟它抢；
 * - 正常抬手先直接读剪贴板，抬手若算手势，系统气泡出来点一下就好。但 WebKit 目前只把长按后的
 *   抬手当「可能的手势」（够播音频、不够读剪贴板），读取立刻被拒、不出任何界面，于是当场在手指
 *   处弹「粘贴」菜单，点它（真正的点击）再读，跟系统气泡确认。
 * 安卓读剪贴板不确认，长按松手直接粘贴太容易误触，仍先弹菜单。iOS 上所有浏览器都是 WebKit；
 * iPadOS 默认报成 Mac，靠触点数认出来。每次现算，测试里可以换 UA。 */
function systemConfirmsPaste() {
    const ua = navigator.userAgent;
    return /iP(hone|ad|od)/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1);
}
let pasteHint = null;
function showPasteHint(x, y) {
    hidePasteHint();
    const hint = document.createElement("div");
    hint.className = "term-paste-hint";
    hint.setAttribute("aria-hidden", "true");
    hint.textContent = i18nText("松开粘贴");
    document.body.append(hint);
    // 浮在手指上方、别被手指挡住，贴着视口边缘时往里收
    const w = hint.offsetWidth, h = hint.offsetHeight, edge = 8;
    hint.style.left = Math.round(Math.min(Math.max(x - w / 2, edge), innerWidth - w - edge)) + "px";
    hint.style.top = Math.round(Math.max(y - h - 44, edge)) + "px";
    enter(hint, 4);
    pasteHint = hint;
}
function hidePasteHint() {
    const hint = pasteHint;
    pasteHint = null;
    if (hint)
        leave(hint, () => hint.remove(), 4);
}
function openPasteMenu(x, y) {
    openMenuAt(x, y, [{ label: i18nText("粘贴"), icon: "paste", run: () => void pasteClipboard() }]);
}
async function pasteOnRelease(x, y) {
    if (await pasteClipboard(true) === "refused" && termOf() && !termKeys.disabled)
        openPasteMenu(x, y);
}
/* 被拒得这么快就不是人点掉了系统气泡，而是浏览器根本没给机会（这次不算手势） */
const REFUSED_MS = 300;
/* 手机上长按终端的「粘贴」。触屏上 xterm 的输入框只是光标处看不见的一个点，长按别处出不来
 * 系统的粘贴菜单，只能自己读剪贴板（要 HTTPS）：iOS 会再弹一个系统「粘贴」气泡确认，Android
 * 第一次会问剪贴板权限。图片与桌面粘贴一样上传后写入路径；文字走 xterm 的 paste，程序开着
 * 括号粘贴（Claude 会开）时多行文字整段送达，不会被当成一行行回车。 */
async function pasteClipboard(onRelease = false) {
    const clip = navigator.clipboard;
    if (!clip?.read && !clip?.readText) {
        toast(i18nText("浏览器不允许网页读取剪贴板（需要 HTTPS 访问）"), true);
        return "failed";
    }
    let text = "";
    const images = [];
    const started = performance.now();
    try {
        if (clip.read) {
            for (const item of await clip.read()) {
                const image = item.types.find((t) => t.startsWith("image/"));
                if (image)
                    images.push(await item.getType(image));
                else if (item.types.includes("text/plain"))
                    text += await (await item.getType("text/plain")).text();
            }
        }
        else {
            text = await clip.readText();
        }
    }
    catch (e) {
        const ms = Math.round(performance.now() - started);
        traceTermEvent("clipboard-read", { ok: false, error: e instanceof DOMException ? e.name : String(e), ms, onRelease });
        // 长按抬手时的读取：立刻被拒是这次抬手不算手势，交给调用方弹菜单；过一会儿才被拒是用户
        // 点了系统「粘贴」气泡以外的地方，算取消
        if (onRelease && e instanceof DOMException && e.name === "NotAllowedError")
            return ms < REFUSED_MS ? "refused" : "dismissed";
        toast(i18nText("没能读取剪贴板：请允许浏览器访问剪贴板后再试"), true);
        return "failed";
    }
    traceTermEvent("clipboard-read", { ok: true, text: text.length, images: images.length, ms: Math.round(performance.now() - started), onRelease });
    const term = termOf();
    if (!term || !S.current || termKeys.disabled)
        return "failed";
    if (images.length)
        void uploadTermImages(images);
    else if (text)
        term.paste(text);
    else
        toast(i18nText("剪贴板里没有文字或图片"));
    return "pasted";
}
