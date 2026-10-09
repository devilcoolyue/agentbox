/* motion：脚本挂载、摘除的节点（「⋯」菜单、提示条）的进出场。纯 CSS 管得到的
 * （弹窗、弹层、页面切换）都在样式里用 @starting-style 写，不走这里。
 * 时长与曲线读 base.css 的令牌；「减少动态效果」时直接跳过动画、同步收尾，
 * 调用方不必分两条路。 */

const reduced = matchMedia("(prefers-reduced-motion: reduce)");

function token(name: string) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}
function ms(value: string) {
  const n = parseFloat(value);
  if (!Number.isFinite(n)) return 0;
  return value.endsWith("ms") ? n : n * 1000;
}

/** 不做动画：用户要求减少动态效果，或浏览器没有 Web Animations。 */
export function motionOff() {
  return reduced.matches || typeof Element.prototype.animate !== "function";
}

/** 进场：从上下偏移 fromY 处淡入到原位。duration 取令牌名：菜单用快档，整块页面内容用常规档。 */
export function enter(el: HTMLElement, fromY = -4, duration = "--dur-fast") {
  if (motionOff()) return;
  el.animate(
    [{ opacity: 0, translate: `0 ${fromY}px` }, { opacity: 1, translate: "0 0" }],
    { duration: ms(token(duration)), easing: token("--ease-out") },
  );
}

/* 列表条目的进场：按 key 记住每条开始播的时刻。这些列表每次刷新都整列重建 DOM，
 * 同一 key 的新节点接着播剩下那一截，播完的不再播——轮询重绘不会让整列重闪，
 * 刚出现的那一条也不会被紧跟着的重绘掐断。同一批新出现的条目依次错开 30ms，最多错 8 个。
 * 动画本身写在 CSS 的 className 上（见 base.css 的 .enter / shell.css 的 .flash）。 */
export class ListMotion {
  private started = new Map<string, number>();
  private batchAt = -Infinity;
  private batchSize = 0;

  constructor(private className: string, private animationName: string, private lifetime = 600) {}

  /** 节点已经插进文档后调用；第一次见到的 key 开始播，见过的接着播或不播。 */
  play(el: HTMLElement, key: string) {
    if (motionOff()) return;
    const now = document.timeline.currentTime;
    if (typeof now !== "number") return;
    let start = this.started.get(key);
    if (start === undefined) {
      if (now - this.batchAt > 100) { this.batchAt = now; this.batchSize = 0; }
      start = now + Math.min(this.batchSize++, 8) * 30;
      this.started.set(key, start);
    }
    if (now - start > this.lifetime) return;
    el.classList.add(this.className);
    for (const a of el.getAnimations()) {
      if (a instanceof CSSAnimation && a.animationName === this.animationName) a.startTime = start;
    }
  }

  /** 这些条目已经在场：之后 play 到它们也不播（首屏已有的条目只在之后新增时才提示）。 */
  settle(keys: Iterable<string>) {
    for (const key of keys) if (!this.started.has(key)) this.started.set(key, -Infinity);
  }

  /** 列表换了一批内容（离开页面、翻页、换筛选）：下一次整列重新错落进场。 */
  reset() {
    this.started.clear();
    this.batchAt = -Infinity;
  }
}

/** 退场：淡出并朝 toY 方向挪一点，播完才调 done（通常是移除节点）。
 * 途中节点设为 inert，不再接点击与焦点。返回的动画被 cancel() 时不调 done，
 * 复用的单例节点（提示条）靠这一点把「正在退场」改回「重新显示」。 */
export function leave(el: HTMLElement, done: () => void, toY = -4): Animation | null {
  if (motionOff() || !el.isConnected) { done(); return null; }
  el.inert = true;
  const anim = el.animate(
    [{ opacity: 1 }, { opacity: 0, translate: `0 ${toY}px` }],
    { duration: ms(token("--dur-fast")), easing: token("--ease-in"), fill: "forwards" },
  );
  anim.onfinish = done;
  return anim;
}
