import { setAttrRender, setTextRender, t as i18nText } from "../../i18n.js";
import { isMobile, onMobileChange } from "../../util.js";
import { motionOff } from "../../motion.js";
import { svgIcon } from "../../icons.js";
import { dockLift, lastAtOrBefore, outlineAnswer, outlineQuestion, railLayout } from "./outline-text.js";
/* 对话刻度导航：对话流旁一列短横线，一条用户消息一根，自上而下按时间排。
 * - 桌面在内容栏左侧留白里：鼠标划过时像 macOS 程序坞一样鼓起（只变长，不变色），
 *   旁边浮出这一轮的问题原文和回答第一句，点击跳过去；Tab 进来后 ↑↓ 选、Enter 跳。
 * - 手机在右边缘（左边缘是 Safari 的返回手势）：滚动时浮现、停下 1.5 秒隐去；
 *   按住上下拖动预览，松手跳转（往左拖远再松手取消）；轻点打开「对话目录」列表。
 * 颜色只分两档：指着的那根和当前阅读位置那根是正文色，其余一律同一灰色。
 * 数据来自 GET outline（整个线程，含还没加载的较早几页）；跳往没加载的位置时
 * 先从那条消息起补拉历史（loadFrom），服务端嫌范围太大就逐页往前拉（loadOlder）。
 * 刻度与对话流里的消息按发送时间（.user-turn 的 data-ts）对应。 */
const TICK_MAX = 28; // 鼓到最长时的线长（px）
const TICK_BASE = 10; // 静止线长
const TICK_ACTIVE = 18; // 当前阅读位置的线长
const DOCK_RADIUS = 45; // 鼓起的影响半径（px）
const GAP = 10, MIN_GAP = 6;
const MIN_MARKS = 3; // 少于三轮不显示
const READ_LINE = 0.33; // 视口这一高度处往上最近的消息算「当前」
const JUMP_TOP = 0.18; // 跳转后消息顶端停在视口的这一高度
const SLEEP_MS = 1500;
export function createOutline(hooks) {
    let marks = [];
    let byMs = new Map();
    let loaded = []; // 已在对话流里的刻度下标，升序且连续
    let thread = "";
    let request;
    let gen = 0, refreshTimer = 0;
    /* ---------- DOM ---------- */
    const rail = document.createElement("nav");
    rail.className = "outline-rail";
    rail.hidden = true;
    setAttrRender(rail, "aria-label", () => i18nText("对话导航"));
    const track = document.createElement("div");
    track.className = "outline-track";
    const inner = document.createElement("div");
    inner.className = "outline-inner";
    track.append(inner);
    rail.append(track);
    rail.style.setProperty("--tick-max", TICK_MAX + "px");
    const tip = document.createElement("div");
    tip.className = "outline-tip";
    tip.setAttribute("aria-hidden", "true");
    const tipQ = document.createElement("div");
    tipQ.className = "outline-tip-q";
    const tipA = document.createElement("div");
    tipA.className = "outline-tip-a";
    tip.append(tipQ, tipA);
    const toc = document.createElement("button");
    toc.type = "button";
    toc.className = "icon-btn outline-toc";
    toc.hidden = true;
    toc.append(svgIcon("chat.outline", 18));
    setAttrRender(toc, "aria-label", () => i18nText("对话目录"));
    toc.addEventListener("click", () => openSheet());
    let ticks = [];
    let mounted = false;
    function mount() {
        if (mounted)
            return;
        mounted = true;
        hooks.pane().append(rail, tip);
        hooks.tocBefore().before(toc);
        const log = hooks.log();
        new ResizeObserver(() => layout()).observe(log);
        log.addEventListener("scroll", onScroll, { passive: true });
        // 用户自己动了滚动：不再钉住跳转目标，也打断跳转动画
        for (const type of ["wheel", "touchstart", "touchmove", "keydown", "pointerdown"]) {
            log.addEventListener(type, () => { pinned = null; anim.stop(); handScrollAt = performance.now(); }, { passive: true });
        }
        onMobileChange(() => { leave(); layout(); });
    }
    /* ---------- 数据 ---------- */
    const attachLabel = (kind) => kind === "图片" ? i18nText("[图片]") : i18nText("[附件]");
    function toMark(item) {
        return { ms: Date.parse(item.ts), line: item.line, q: outlineQuestion(item.text || "", attachLabel), a: outlineAnswer(item.answer || ""), el: null };
    }
    function setMarks(next) {
        marks = next.filter(m => Number.isFinite(m.ms));
        byMs = new Map(marks.map((m, i) => [m.ms, i]));
        inner.replaceChildren();
        ticks = marks.map((m, i) => {
            const b = document.createElement("button");
            b.type = "button";
            b.className = "outline-tick";
            b.tabIndex = -1;
            setAttrRender(b, "aria-label", () => i18nText("第 {n} 轮：{q}", { n: i + 1, q: m.q.slice(0, 60) }));
            b.append(document.createElement("i"));
            inner.append(b);
            return b;
        });
        prevScale = new Float32Array(ticks.length).fill(-1);
        hot = null;
        active = -1;
        resolve();
        layout();
    }
    async function load(tid) {
        mount();
        const sid = hooks.session();
        if (!sid || !tid) {
            reset();
            return;
        }
        if (tid !== thread) {
            thread = tid;
            setMarks([]);
        }
        request?.abort();
        const mine = ++gen, ctl = request = new AbortController();
        try {
            const value = await hooks.read(`/sessions/${sid}/outline?thread=${encodeURIComponent(tid)}`, ctl.signal);
            if (mine !== gen || hooks.session() !== sid || thread !== tid)
                return;
            setMarks((value.items || []).map(toMark));
        }
        catch (_) {
            // 拿不到就保持原样：刻度只是导航，不影响对话本身
        }
    }
    /* ---------- 刻度与对话流对应 ---------- */
    function resolve() {
        for (const m of marks)
            m.el = null;
        for (const el of hooks.log().querySelectorAll(":scope > .user-turn[data-ts]")) {
            const i = byMs.get(Date.parse(el.dataset.ts));
            if (i !== undefined)
                marks[i].el = el;
        }
        loaded = [];
        marks.forEach((m, i) => { if (m.el)
            loaded.push(i); });
    }
    /* ---------- 排布 ---------- */
    let gap = GAP, trackH = 0, off = 0, windowed = false, mobile = false;
    function layout() {
        if (!mounted)
            return;
        const log = hooks.log();
        mobile = isMobile();
        rail.classList.toggle("mobile", mobile);
        tip.classList.toggle("mobile", mobile);
        const scrollable = log.scrollHeight > log.clientHeight + 40 || (loaded.length > 0 && loaded[0] > 0);
        const show = marks.length >= MIN_MARKS && scrollable && log.clientHeight > 0 && !log.classList.contains("hidden");
        toc.hidden = !(show && mobile);
        if (!show) {
            if (!rail.hidden) {
                rail.hidden = true;
                leave();
            }
            return;
        }
        rail.hidden = false;
        const avail = Math.round(log.clientHeight * (mobile ? 0.66 : 0.6)) - 24;
        ({ gap, windowed, height: trackH } = railLayout(marks.length, Math.max(avail, GAP * MIN_MARKS), GAP, MIN_GAP));
        rail.classList.toggle("windowed", windowed);
        rail.style.top = log.offsetTop + log.clientHeight / 2 + "px";
        track.style.height = trackH + "px";
        inner.style.height = marks.length * gap + "px";
        ticks.forEach((b, i) => { b.style.top = i * gap + "px"; b.style.height = gap + "px"; });
        prevScale.fill(-1);
        updateActive(true);
    }
    function setOffset(v) {
        off = windowed ? Math.min(Math.max(0, marks.length * gap - trackH), Math.max(0, v)) : 0;
        inner.style.transform = `translateY(${-off}px)`;
    }
    /** 开窗时让第 i 根留在可见段里（离上下边缘至少三根） */
    function keepVisible(i) {
        if (!windowed)
            return setOffset(0);
        const y = i * gap, pad = gap * 3;
        if (y < off + pad || y > off + trackH - pad - gap)
            setOffset(y - trackH / 2 + gap / 2);
    }
    /* ---------- 绘制：鼓起只改线长；颜色由 .hot / .active 两个类决定 ---------- */
    let prevScale = new Float32Array(0);
    let pointerY = null; // 刻度内容坐标（含 off）
    let hot = null;
    const indexAt = (y) => Math.max(0, Math.min(marks.length - 1, Math.floor(y / gap)));
    function paint() {
        const y = pointerY, reduced = motionOff();
        const next = y === null ? null : ticks[indexAt(y)] || null;
        if (next !== hot) {
            hot?.classList.remove("hot");
            next?.classList.add("hot");
            hot = next;
        }
        for (let i = 0; i < ticks.length; i++) {
            const d = y === null ? Infinity : Math.abs(i * gap + gap / 2 - y);
            const lift = reduced ? (d < gap / 2 ? 1 : 0) : dockLift(d, DOCK_RADIUS);
            let len = TICK_BASE + (TICK_MAX - TICK_BASE) * lift;
            if (i === active)
                len = Math.max(len, TICK_ACTIVE);
            const k = Math.round((len / TICK_MAX) * 1000) / 1000;
            if (k === prevScale[i])
                continue;
            prevScale[i] = k;
            ticks[i].firstChild.style.transform = `scaleX(${k})`;
        }
    }
    /* ---------- 当前阅读位置 ---------- */
    let active = -1;
    /* 跳转目标钉成「当前」：被点的那轮很短或在最底下时，按阅读位置算会落到别的轮上。
     * 只钉到它还停在跳过去的位置为止——at 是落定后它离视口顶的距离（途中为 null）。
     * 顶部插入更早一页时阅读位置被平移保持，它不动、照样钉着；用户滚动、「滚到最新」
     * 按钮或别的代码把它挪开了，就回到按阅读位置算。 */
    let pinned = null;
    const offsetOf = (el) => el.offsetTop - hooks.log().scrollTop;
    function computeActive() {
        if (pinned) {
            const i = byMs.get(pinned.ms), el = i === undefined ? null : marks[i].el;
            if (i !== undefined && (pinned.at === null || (el && Math.abs(offsetOf(el) - pinned.at) <= 4)))
                return i;
            pinned = null;
        }
        if (!loaded.length)
            return -1;
        const log = hooks.log();
        if (log.scrollHeight - log.scrollTop - log.clientHeight < 4)
            return loaded[loaded.length - 1];
        const y = log.scrollTop + log.clientHeight * READ_LINE;
        const k = lastAtOrBefore(0, loaded.length, j => marks[loaded[j]].el.offsetTop, y);
        return loaded[Math.max(0, k)];
    }
    function updateActive(force = false) {
        if (rail.hidden)
            return;
        const a = computeActive();
        if (a === active && !force)
            return;
        ticks[active]?.classList.remove("active");
        active = a;
        ticks[a]?.classList.add("active");
        if (!rail.contains(document.activeElement)) {
            ticks.forEach((b, i) => { b.tabIndex = i === Math.max(0, a) ? 0 : -1; });
            if (pointerY === null && a >= 0)
                keepVisible(a);
        }
        paint();
    }
    /* 手机上只有用户自己滚（含松手后的惯性）才浮现：回复流式输出时对话流一直被拉到底，
     * 那也是滚动事件，若跟着浮现，右边缘会一直挡着拇指 */
    let scrollFrame = 0, sleepTimer = 0, handScrollAt = -Infinity;
    function onScroll() {
        if (mobile && !rail.hidden && performance.now() - handScrollAt < 1500) {
            rail.classList.add("awake");
            sleepSoon();
        }
        if (scrollFrame)
            return;
        scrollFrame = requestAnimationFrame(() => {
            scrollFrame = 0;
            if (rail.hidden)
                layout(); // 内容长到能滚了才出现
            else
                updateActive();
        });
    }
    function sleepSoon() {
        clearTimeout(sleepTimer);
        sleepTimer = window.setTimeout(() => { if (!drag)
            rail.classList.remove("awake"); }, SLEEP_MS);
    }
    /* ---------- 摘要浮层 ---------- */
    let tipFor = -1;
    function showTip(i, clientY, cancel = false) {
        if (i < 0 || !marks[i])
            return;
        if (tipFor !== i) {
            tipQ.textContent = marks[i].q;
            tipA.textContent = marks[i].a;
            tipA.hidden = !marks[i].a;
            tipFor = i;
        }
        tip.classList.toggle("cancel", cancel);
        const pane = hooks.pane().getBoundingClientRect(), r = rail.getBoundingClientRect(), t = track.getBoundingClientRect();
        const log = hooks.log();
        const tw = tip.offsetWidth, th = tip.offsetHeight;
        let x, y;
        if (mobile) {
            x = r.left - pane.left - tw - 6;
            // 拖动时在手指左上方，避开拇指；键盘选中时与刻度对齐
            y = clientY === undefined ? t.top - pane.top + i * gap + gap / 2 - off - th / 2 : clientY - pane.top - th - 26;
            if (clientY !== undefined && y < log.offsetTop + 8)
                y = clientY - pane.top + 30;
        }
        else {
            x = r.left - pane.left + TICK_MAX + 14;
            y = t.top - pane.top + i * gap + gap / 2 - off - th / 2;
        }
        x = Math.max(8, Math.min(x, pane.width - tw - 8));
        y = Math.max(log.offsetTop + 8, Math.min(y, log.offsetTop + log.clientHeight - th - 8));
        const first = !tip.classList.contains("show");
        if (first)
            tip.classList.add("instant");
        tip.style.setProperty("--x", x + "px");
        tip.style.setProperty("--y", y + "px");
        if (first) {
            void tip.offsetWidth;
            tip.classList.remove("instant");
        }
        tip.classList.add("show");
    }
    function hideTip() { tip.classList.remove("show", "cancel"); tipFor = -1; }
    /* ---------- 指针：桌面悬停 / 手机按住拖动 ---------- */
    let drag = null;
    let holdTimer = 0, edgeFrame = 0;
    const contentY = (clientY) => clientY - track.getBoundingClientRect().top + off;
    function hoverAt(clientY) {
        pointerY = contentY(clientY);
        rail.classList.add("dock");
        paint();
        return indexAt(pointerY);
    }
    function leave() {
        pointerY = null;
        rail.classList.remove("dock");
        hideTip();
        paint();
    }
    rail.addEventListener("pointermove", e => {
        if (drag)
            return scrubMove(e);
        if (e.pointerType !== "mouse" || mobile)
            return;
        showTip(hoverAt(e.clientY));
    });
    rail.addEventListener("pointerleave", e => { if (!drag && e.pointerType === "mouse")
        leave(); });
    rail.addEventListener("pointerdown", e => {
        if (e.button !== 0)
            return;
        e.preventDefault(); // 不让按下抢走焦点、也不开始选中文字
        if (e.pointerType === "mouse" && !mobile)
            return; // 桌面鼠标：抬起时跳
        rail.setPointerCapture(e.pointerId);
        drag = { x0: e.clientX, y0: e.clientY, cx: e.clientX, cy: e.clientY, scrub: false, last: -1 };
        rail.classList.add("dragging");
        clearTimeout(sleepTimer);
        holdTimer = window.setTimeout(() => { if (drag && !drag.scrub)
            startScrub(); }, 300);
    });
    rail.addEventListener("pointerup", e => {
        if (!drag) {
            if (e.pointerType === "mouse" && !mobile && e.button === 0 && pointerY !== null)
                void jump(indexAt(pointerY));
            return;
        }
        const d = endDrag();
        if (!d.scrub) {
            if (mobile)
                openSheet();
            else
                void jump(indexAt(contentY(e.clientY)));
        }
        else if (!inCancel(e.clientX))
            void jump(indexAt(pointerY ?? contentY(e.clientY)));
        leave();
    });
    rail.addEventListener("pointercancel", () => { if (drag) {
        endDrag();
        leave();
    } });
    function endDrag() {
        const d = drag;
        drag = null;
        clearTimeout(holdTimer);
        cancelAnimationFrame(edgeFrame);
        rail.classList.remove("dragging");
        sleepSoon();
        return d;
    }
    function inCancel(clientX) { return mobile && clientX < rail.getBoundingClientRect().left - 70; }
    function startScrub() {
        drag.scrub = true;
        scrubAt(drag.cx, drag.cy);
        if (windowed)
            edgeScroll();
    }
    function scrubMove(e) {
        const d = drag;
        d.cx = e.clientX;
        d.cy = e.clientY;
        if (d.scrub)
            scrubAt(e.clientX, e.clientY);
        else if (Math.abs(e.clientY - d.y0) > 6 || Math.abs(e.clientX - d.x0) > 6) {
            clearTimeout(holdTimer);
            startScrub();
        }
    }
    function scrubAt(cx, cy) {
        const t = track.getBoundingClientRect();
        const i = hoverAt(Math.max(t.top, Math.min(t.bottom - 1, cy)));
        showTip(i, mobile ? cy : undefined, inCancel(cx));
        if (i !== drag.last) {
            drag.last = i;
            navigator.vibrate?.(4);
        }
    }
    /* 开窗时手指停在刻度条上下边缘：窗口自动滚动，越靠边越快 */
    function edgeScroll() {
        cancelAnimationFrame(edgeFrame);
        const step = () => {
            if (!drag?.scrub)
                return;
            const t = track.getBoundingClientRect(), zone = 36;
            let v = 0;
            if (drag.cy < t.top + zone)
                v = -(1 - (drag.cy - t.top) / zone);
            else if (drag.cy > t.bottom - zone)
                v = 1 - (t.bottom - drag.cy) / zone;
            if (v) {
                setOffset(off + Math.max(-1, Math.min(1, v)) * 5);
                scrubAt(drag.cx, drag.cy);
            }
            edgeFrame = requestAnimationFrame(step);
        };
        edgeFrame = requestAnimationFrame(step);
    }
    // 刻度条上的滚轮：开窗时滚刻度，否则照常滚对话
    rail.addEventListener("wheel", e => {
        e.preventDefault();
        const dy = e.deltaY * (e.deltaMode === 1 ? 16 : 1);
        if (windowed) {
            setOffset(off + dy * 0.5);
            if (pointerY !== null)
                showTip(hoverAt(e.clientY));
            else
                paint();
        }
        else {
            pinned = null;
            anim.stop();
            hooks.log().scrollTop += dy;
        }
    }, { passive: false });
    /* ---------- 键盘 ---------- */
    let focusIdx = -1;
    rail.addEventListener("focusin", e => {
        const i = ticks.indexOf(e.target);
        if (i < 0)
            return;
        focusIdx = i;
        keepVisible(i);
        pointerY = i * gap + gap / 2;
        rail.classList.add("dock");
        paint();
        showTip(i);
    });
    rail.addEventListener("focusout", e => {
        if (rail.contains(e.relatedTarget))
            return;
        focusIdx = -1;
        leave();
        updateActive(true);
    });
    rail.addEventListener("keydown", e => {
        if (focusIdx < 0)
            return;
        const n = marks.length;
        const to = { ArrowUp: focusIdx - 1, ArrowDown: focusIdx + 1, Home: 0, End: n - 1, PageUp: focusIdx - 10, PageDown: focusIdx + 10 }[e.key];
        if (to !== undefined) {
            e.preventDefault();
            const j = Math.max(0, Math.min(n - 1, to));
            ticks[focusIdx].tabIndex = -1;
            ticks[j].tabIndex = 0;
            ticks[j].focus();
        }
        else if (e.key === "Escape")
            e.target.blur();
    });
    // Enter / 空格 / 读屏软件的激活都是不带指针的 click（detail 为 0）；鼠标和触摸在 pointerup 里处理
    rail.addEventListener("click", e => {
        if (e.detail !== 0)
            return;
        const i = ticks.indexOf(e.target.closest?.(".outline-tick"));
        if (i >= 0)
            void jump(i);
    });
    /* ---------- 跳转 ---------- */
    let jumpSeq = 0;
    const anim = (() => {
        let frame = 0;
        return {
            stop() { cancelAnimationFrame(frame); frame = 0; },
            /* 每帧重新量目标位置、按剩余距离的一截逼近：途中顶部插入更早一页（阅读位置会被
             * 平移保持）也照样落到目标上，不会像浏览器的平滑滚动那样被打断 */
            to(log, target, done) {
                this.stop();
                let frames = 0;
                const step = () => {
                    const goal = Math.min(target(), log.scrollHeight - log.clientHeight);
                    const left = goal - log.scrollTop;
                    if (Math.abs(left) < 1 || ++frames > 48) {
                        log.scrollTop = goal;
                        frame = 0;
                        done();
                        return;
                    }
                    log.scrollTop += left * 0.22;
                    frame = requestAnimationFrame(step);
                };
                frame = requestAnimationFrame(step);
            },
        };
    })();
    function flash(el) {
        const msg = el.querySelector(".msg.user") || el;
        msg.classList.remove("outline-flash");
        void msg.offsetWidth;
        msg.classList.add("outline-flash");
        msg.addEventListener("animationend", () => msg.classList.remove("outline-flash"), { once: true });
    }
    function scrollTo(i, instant) {
        const el = marks[i]?.el;
        if (!el)
            return;
        const log = hooks.log();
        const target = () => Math.max(0, el.offsetTop - log.clientHeight * JUMP_TOP);
        // 落定后记下目标离视口顶多远，钉住到它被挪开为止（见 pinned）
        const ms = marks[i].ms;
        const settle = () => { if (pinned && pinned.ms === ms && el.isConnected)
            pinned.at = offsetOf(el); };
        if (instant || motionOff()) {
            anim.stop();
            log.scrollTop = target();
            settle();
        }
        else
            anim.to(log, target, settle);
        flash(el);
    }
    async function jump(i) {
        const m = marks[i];
        if (!m)
            return;
        const seq = ++jumpSeq, ms = m.ms;
        pinned = { ms, at: null };
        updateActive();
        if (!m.el?.isConnected)
            resolve();
        if (marks[i]?.el)
            return scrollTo(i, false);
        if (m.line < 0)
            return;
        const tick = ticks[i];
        tick?.classList.add("loading");
        try {
            // 从前一条消息起拉：目标上方留一轮上下文，才停得到和已加载时一样的位置
            const prev = marks[i - 1];
            await hooks.loadFrom(prev && prev.line >= 0 ? prev.line : m.line);
            resolve();
            // 服务端嫌从这条到最新的内容太多时退回最新一页：逐页往前拉到它为止
            while (seq === jumpSeq && !findLoaded(ms)) {
                if (!(await hooks.loadOlder()))
                    break;
                resolve();
            }
        }
        finally {
            tick?.classList.remove("loading");
        }
        if (seq !== jumpSeq)
            return;
        const j = findLoaded(ms);
        if (j < 0) {
            pinned = null;
            updateActive();
            return;
        }
        scrollTo(j, true);
        updateActive(true);
    }
    function findLoaded(ms) {
        const i = byMs.get(ms);
        return i !== undefined && marks[i].el ? i : -1;
    }
    /* ---------- 手机：对话目录 ---------- */
    let sheet = null, sheetList, sheetCount;
    function openSheet() {
        if (!marks.length)
            return;
        if (!sheet) {
            sheet = document.createElement("dialog");
            sheet.className = "dlg-outline";
            const head = document.createElement("div");
            head.className = "dlg-head";
            const title = document.createElement("h2");
            setTextRender(title, () => i18nText("对话目录"));
            sheetCount = document.createElement("span");
            sheetCount.className = "outline-count";
            const close = document.createElement("button");
            close.type = "button";
            close.className = "dlg-x";
            close.append(svgIcon("close", 16));
            setAttrRender(close, "aria-label", () => i18nText("关闭"));
            close.addEventListener("click", () => sheet.close());
            head.append(title, sheetCount, close);
            sheetList = document.createElement("div");
            sheetList.className = "dlg-body outline-list";
            sheet.append(head, sheetList);
            // 点遮罩关闭
            sheet.addEventListener("click", e => {
                if (e.target !== sheet)
                    return;
                const r = sheet.getBoundingClientRect();
                if (e.clientY < r.top || e.clientY > r.bottom || e.clientX < r.left || e.clientX > r.right)
                    sheet.close();
            });
            document.body.append(sheet);
        }
        setTextRender(sheetCount, () => i18nText("共 {n} 轮", { n: marks.length }));
        sheetList.replaceChildren(...marks.map((m, i) => {
            const b = document.createElement("button");
            b.type = "button";
            b.className = "outline-item" + (i === active ? " current" : "");
            if (i === active)
                b.setAttribute("aria-current", "true");
            const n = document.createElement("span");
            n.className = "outline-item-n";
            n.textContent = "#" + (i + 1);
            const body = document.createElement("span");
            body.className = "outline-item-body";
            const q = document.createElement("span");
            q.className = "outline-item-q";
            q.textContent = m.q;
            body.append(q);
            if (m.a) {
                const a = document.createElement("span");
                a.className = "outline-item-a";
                a.textContent = m.a;
                body.append(a);
            }
            b.append(n, body);
            b.addEventListener("click", () => { sheet.close(); void jump(i); });
            return b;
        }));
        sheet.showModal();
        const cur = sheetList.children[Math.max(0, active)];
        cur?.scrollIntoView({ block: "center" });
        cur?.focus({ preventScroll: true });
    }
    function reset() {
        request?.abort();
        gen++;
        clearTimeout(refreshTimer);
        jumpSeq++;
        anim.stop();
        thread = "";
        pinned = null;
        if (sheet?.open)
            sheet.close();
        setMarks([]);
    }
    return {
        /** 线程（重新）加载后调用：拉这个线程的刻度 */
        load,
        /** 一轮结束后稍等再拉一次，补上回答摘要（本地斜杠命令也会在这时被剔除） */
        refreshSoon() {
            if (!thread)
                return;
            clearTimeout(refreshTimer);
            refreshTimer = window.setTimeout(() => void load(thread), 600);
        },
        /** 刚发出的消息先补一根刻度，回答等 refreshSoon */
        add(ts, text) {
            const ms = Date.parse(ts || "");
            if (!thread || !Number.isFinite(ms) || byMs.has(ms))
                return;
            setMarks([...marks, { ms, line: -1, q: outlineQuestion(text || "", attachLabel), a: "", el: null }]);
        },
        /** 对话流里的消息增减了（整体重载、插入更早一页、新消息） */
        changed() {
            if (!mounted || !marks.length)
                return;
            resolve();
            layout();
        },
        reset,
    };
}
