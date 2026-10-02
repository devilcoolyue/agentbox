/* term-touch：手机上单指上下滑动终端，翻看之前的输出；按住不动则交给 longPress（长按粘贴）。
 *
 * xterm 6 没有触摸滚动（自带的手势类没接到视口上），手指拖动落到浏览器手里，滚走的是整个
 * 页面。电脑上滚轮能翻，是 xterm 把滚轮编码成鼠标滚轮上报发给 tmux（tmux.conf 开了 mouse），
 * tmux 进历史模式往回翻；所以这里把滑动换算成同样的滚轮事件派发给 xterm，走它自己的滚轮
 * 处理：鼠标上报、备用屏里发方向键都与桌面一致。只有普通屏（没套 tmux 的老镜像）直接滚
 * xterm 自己的回滚区，一行对一行跟手。
 *
 * 终端上的单指拖动一律不交给浏览器；页面被放大时除外（留给平移），双指照常缩放。滑动过、
 * 长按过的那次触摸抬手时吞掉点击，免得顺手弹出键盘、给 tmux 发一下鼠标点击。 */
"use strict";
const SLOP = 8; // 挪动不超过这个距离仍算点按
const LONG_PRESS = 500; // 按住多久算长按，与系统长按菜单的手感一致
const MOUSE_STEP_ROWS = 3; // 鼠标上报时手指每滑过几行发一次滚轮。tmux 历史模式一次翻 5 行，
// 按 5 行一发才完全跟手，但起步要拖出 5 行才有反应，太迟钝
const SAMPLE_MS = 100; // 用抬手前这段时间的移动估算速度
const FLING_MIN = 0.35; // 抬手速度（px/ms）超过它才惯性滚动
const FLING_MAX = 4; // 甩得再快也按这个速度起步，一甩最多滑出一千多像素
const FLING_STOP = 0.03;
const FLING_DECAY = 0.95; // 每 16.7ms 剩下的速度比例
const reducedMotion = matchMedia("(prefers-reduced-motion: reduce)");
export function attachTouchScroll(term, opts = {}) {
    const host = term.element;
    const screen = host?.querySelector(".xterm-screen");
    if (!host || !screen)
        return;
    let finger = null; // 正在跟踪的那根手指
    let startX = 0, startY = 0, lastX = 0, lastY = 0;
    let axis = ""; // 认定的手势；横向与长按都不滚，只是整个过程不让页面动
    let hold;
    let swallow = false; // 抬手时吞掉点击：滑动过，或这一下是按停惯性
    let pending = 0; // 还没换算成滚动的像素，正数往新内容方向翻
    let samples = [];
    let fling = 0;
    const mouseReports = () => term.modes.mouseTrackingMode !== "none";
    const rowHeight = () => screen.clientHeight / term.rows || 16;
    /* 派发到终端画面上、坐标夹在画面内：xterm 用它换算上报的行列，落到画面外的上报会被丢掉 */
    function wheel(dir) {
        const r = screen.getBoundingClientRect();
        const clientX = Math.min(Math.max(lastX, r.left + 1), r.right - 1);
        const clientY = Math.min(Math.max(lastY, r.top + 1), r.bottom - 1);
        screen.dispatchEvent(new WheelEvent("wheel", {
            deltaY: dir, deltaMode: WheelEvent.DOM_DELTA_LINE, clientX, clientY, bubbles: true, cancelable: true,
        }));
    }
    function consume() {
        const viaWheel = mouseReports() || term.buffer.active.type === "alternate";
        const step = rowHeight() * (mouseReports() ? MOUSE_STEP_ROWS : 1);
        while (Math.abs(pending) >= step) {
            const dir = Math.sign(pending);
            pending -= dir * step;
            if (viaWheel)
                wheel(dir);
            else
                term.scrollLines(dir);
        }
    }
    function stopFling() {
        if (!fling)
            return false;
        cancelAnimationFrame(fling);
        fling = 0;
        return true;
    }
    function startFling(v) {
        let prev = performance.now();
        const frame = (now) => {
            const dt = Math.min(now - prev, 50);
            prev = now;
            pending += v * dt;
            v *= Math.pow(FLING_DECAY, dt / 16.7);
            consume();
            fling = Math.abs(v) > FLING_STOP && host.isConnected ? requestAnimationFrame(frame) : 0;
        };
        fling = requestAnimationFrame(frame);
    }
    const tracked = (list) => [...list].find((t) => t.identifier === finger);
    const cancelHold = () => { clearTimeout(hold); hold = undefined; };
    host.addEventListener("touchstart", (e) => {
        swallow = stopFling() || !!opts.swallowTap?.();
        finger = null;
        cancelHold();
        const zoomed = (window.visualViewport?.scale ?? 1) > 1.01;
        if (e.touches.length !== 1 || zoomed)
            return;
        const t = e.touches[0];
        finger = t.identifier;
        if (opts.longPress) {
            hold = setTimeout(() => {
                hold = undefined;
                if (finger !== t.identifier || axis)
                    return;
                axis = "hold";
                swallow = true;
                opts.longPress(startX, startY);
            }, LONG_PRESS);
        }
        startX = lastX = t.clientX;
        startY = lastY = t.clientY;
        axis = "";
        pending = 0;
        samples = [{ t: e.timeStamp, y: t.clientY }];
    }, { passive: true });
    host.addEventListener("touchmove", (e) => {
        if (finger === null)
            return;
        if (e.touches.length > 1) {
            finger = null;
            cancelHold();
            return;
        } // 第二根手指落下：交还浏览器缩放
        const t = tracked(e.changedTouches);
        if (!t)
            return;
        if (e.cancelable)
            e.preventDefault();
        const dx = t.clientX - startX, dy = t.clientY - startY;
        if (!axis) {
            if (Math.abs(dx) <= SLOP && Math.abs(dy) <= SLOP)
                return;
            swallow = true;
            cancelHold();
            axis = Math.abs(dx) > Math.abs(dy) ? "x" : "y";
        }
        if (axis !== "y")
            return;
        pending += lastY - t.clientY;
        lastX = t.clientX;
        lastY = t.clientY;
        samples.push({ t: e.timeStamp, y: t.clientY });
        while (samples.length > 2 && e.timeStamp - samples[0].t > SAMPLE_MS)
            samples.shift();
        consume();
    }, { passive: false });
    const end = (e) => {
        if (swallow && e.type === "touchend" && e.cancelable)
            e.preventDefault();
        if (finger === null || !tracked(e.changedTouches))
            return;
        finger = null;
        cancelHold();
        if (axis !== "y" || e.type !== "touchend" || reducedMotion.matches)
            return;
        const first = samples[0], last = samples[samples.length - 1];
        // 停住再抬手不算甩动：最后一次移动离抬手太久就没有速度
        if (e.timeStamp - last.t > SAMPLE_MS / 2 || last.t <= first.t)
            return;
        const v = (first.y - last.y) / (last.t - first.t);
        if (Math.abs(v) > FLING_MIN)
            startFling(Math.sign(v) * Math.min(Math.abs(v), FLING_MAX));
    };
    host.addEventListener("touchend", end, { passive: false });
    host.addEventListener("touchcancel", end);
    /* Android 长按还会发 contextmenu：xterm 收到会当成右键，把输入框挪到手指下等系统菜单。
     * 触摸来的一律拦下，长按由上面的计时器处理；鼠标右键照旧 */
    host.addEventListener("contextmenu", (e) => {
        if (finger === null && e.pointerType !== "touch")
            return;
        e.preventDefault();
        e.stopPropagation();
    }, true);
}
