/** The visible stroke and its travelling dot share one polyline in CSS pixels.
 * No stretched viewBox, and no endpoint-only tween across a return path. */
const svg = (tag, attrs) => { const e = document.createElementNS("http://www.w3.org/2000/svg", tag); for (const [key, value] of Object.entries(attrs))
    e.setAttribute(key, value); return e; };
export function createTraceWire(from, to, width, laneCount = 5) {
    const element = document.createElement("span");
    element.className = "collaboration-trace-link";
    element.setAttribute("aria-hidden", "true");
    const wire = svg("svg", { class: "collaboration-trace-wire", focusable: "false" });
    const path = svg("path", { class: "collaboration-trace-path" }), origin = svg("circle", { r: "3.4" }), arrow = svg("path", { class: "collaboration-trace-arrow" });
    // HTML transforms stay in CSS coordinates across browsers. WebKit applies
    // CSS zoom twice to a CSS-transformed SVG group, even with matrix() values.
    const packet = document.createElement("span"), aura = document.createElement("span"), dot = document.createElement("span");
    packet.className = "collaboration-trace-packet";
    aura.className = "collaboration-trace-aura";
    dot.className = "collaboration-trace-dot";
    packet.append(aura, dot);
    wire.append(path, origin, arrow);
    element.append(wire, packet);
    let mode = "none", key = "", animation, frames = [], duration = 1100, allowed = false, paused = false, currentWidth = 0;
    function cancel() { animation?.cancel(); animation = undefined; }
    function start(iterations, progress = 0) {
        cancel();
        animation = packet.animate(frames, { duration, iterations, easing: "linear", fill: "none" });
        if (progress)
            animation.currentTime = progress * duration;
        if (paused)
            animation.pause();
    }
    function resize(value) {
        if (!Number.isFinite(value) || value <= 0 || Math.abs(value - currentWidth) < .01)
            return;
        const progress = animation && animation.currentTime !== null ? Number(animation.currentTime) / duration : 0, wasMoving = animation?.playState === "running" || animation?.playState === "paused";
        currentWidth = value;
        const a = (from + .5) * value / laneCount, b = (to + .5) * value / laneCount, self = from === to;
        const points = self ? [{ x: a, y: 25 }, { x: a + 32, y: 25 }, { x: a + 32, y: 40 }, { x: a, y: 40 }] : [{ x: a, y: 35 }, { x: b, y: 35 }];
        const lengths = points.slice(1).map((p, i) => Math.hypot(p.x - points[i].x, p.y - points[i].y)), length = lengths.reduce((a, b) => a + b, 0);
        const first = points[0], last = points.at(-1);
        path.setAttribute("d", points.map((p, i) => `${i ? "L" : "M"} ${p.x} ${p.y}`).join(" "));
        origin.setAttribute("cx", String(first.x));
        origin.setAttribute("cy", String(first.y));
        const sign = self ? -1 : Math.sign(b - a);
        arrow.setAttribute("d", `M ${last.x - sign * 5} ${last.y - 3.5} L ${last.x} ${last.y} L ${last.x - sign * 5} ${last.y + 3.5}`);
        const position = (fraction) => { let distance = length * fraction; for (let i = 0; i < lengths.length; i++) {
            if (distance <= lengths[i] || i === lengths.length - 1) {
                const t = distance / lengths[i];
                return { x: points[i].x + (points[i + 1].x - points[i].x) * t, y: points[i].y + (points[i + 1].y - points[i].y) * t };
            }
            distance -= lengths[i];
        } return last; };
        let covered = 0;
        const offsets = new Set([0, .05, .92, 1]);
        for (const segment of lengths) {
            covered += segment;
            offsets.add(covered / length);
        }
        frames = [...offsets].sort((a, b) => a - b).map(offset => { const p = position(offset); return { offset, transform: `translate(${p.x}px, ${p.y}px)`, opacity: offset === 0 || offset === 1 ? 0 : 1 }; });
        duration = Math.round(Math.max(1100, Math.min(1800, length / 500 * 1000)));
        if (wasMoving && allowed)
            start(mode === "live" ? Infinity : 1, mode === "live" ? progress % 1 : Math.min(progress, .999999));
    }
    function sync(next, nextKey, enabled, isPaused = false) {
        allowed = enabled;
        if (!enabled) {
            cancel();
            mode = "none";
            key = "";
            return;
        }
        if (next === "none") {
            if (mode !== "none")
                cancel();
            mode = "none";
            key = "";
            return;
        }
        paused = isPaused;
        if (mode !== next || key !== nextKey || !animation) {
            mode = next;
            key = nextKey;
            if (!paused)
                start(mode === "live" ? Infinity : 1);
            return;
        }
        if (paused && animation.playState === "running")
            animation.pause();
        else if (!paused && animation.playState === "paused")
            animation.play();
    }
    function hover() { if (allowed && mode === "none") {
        paused = false;
        start(1);
    } }
    function stop() { allowed = false; mode = "none"; key = ""; cancel(); }
    resize(width);
    return { element, resize, sync, hover, stop, dispose: stop, get duration() { return duration; } };
}
