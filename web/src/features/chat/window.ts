import type { History } from "../../types.js";
import { setTextRender, t as i18nText } from "../../i18n.js";
import { spinEl } from "../../util.js";
import { createOlderHistory, type PageCursor } from "./older.js";
import { forgetQuestionCards } from "./questions.js";
import { joinTurn } from "./turns.js";

/* 历史窗口：刻度导航跳往还没加载的较早消息时，不再从它一路重拉到最新（长线程里那是
 * 几千条，手机上要闪好几秒），而是另开一个容器只放那一段：目标消息之前一页 + 从它起的一页。
 * 实时对话流（#chat-log）原样藏在后面照常接收新消息，两个容器同一时刻只显示一个。
 * - 往上滚：窗口自己的 older 逐页往前拉；
 * - 往下滚：逐页往后拉（?after=&until=实时对话流的起点）。正好接上时把窗口整段并进实时对话流
 *   顶部（older.absorb，和插入更早一页同一个动作），换回实时对话流，视口停在原处；
 * - 「滚到最新」、发送消息、切线程或整体重载：直接收起窗口，回到实时对话流。
 * 两边内容不重叠（窗口止于实时对话流的起点），合并后就是一条连续的对话流。 */
export function createHistoryWindow(hooks: {
  live: () => HTMLElement;
  session: () => string | undefined;
  read: (path: string, signal: AbortSignal) => Promise<History>;
  timeout?: () => number;
  /** 把一页渲染进游离容器（与 older 共用） */
  render: (value: History) => HTMLElement;
  /** 实时对话流已加载内容的起点 */
  liveCursor: () => PageCursor | null;
  /** 把窗口整段并进实时对话流顶部；起点对不上时返回 false */
  absorb: (box: HTMLElement, range: PageCursor & { end: number }) => boolean;
  /** 显示的容器换了（打开、收起、并入） */
  switched: () => void;
  /** 窗口里插入了更早或更晚的一页 */
  changed: () => void;
}) {
  const win = document.createElement("div");
  win.className = "chat-log chat-window";
  win.hidden = true;

  let range: (PageCursor & { end: number }) | null = null;
  let seq = 0, busy = false;
  let request: AbortController | undefined;

  const older = createOlderHistory({
    log: () => win,
    session: hooks.session,
    read: hooks.read,
    timeout: hooks.timeout,
    render: hooks.render,
    changed: hooks.changed,
  });

  /* 底部加载条：与顶部的 older 对称，滚到附近自动往后拉一页 */
  const bar = document.createElement("div");
  bar.className = "chat-older chat-newer";
  const spin = spinEl();
  const text = document.createElement("span");
  text.setAttribute("role", "status");
  const retry = document.createElement("button");
  retry.type = "button";
  retry.className = "btn btn-sm";
  setTextRender(retry, () => i18nText("重试"));
  bar.append(spin, text, retry);
  retry.addEventListener("click", () => void newer());
  function show(state: "idle" | "loading" | "error") {
    spin.hidden = state === "error";
    retry.hidden = state !== "error";
    bar.classList.toggle("error", state === "error");
    setTextRender(text, () => state === "error" ? i18nText("后面的对话加载失败") : i18nText("正在加载后面的对话…"));
  }
  let observer: IntersectionObserver | undefined;
  function watch() {
    observer ??= new IntersectionObserver(items => {
      if (items.some(item => item.isIntersecting)) void newer();
    }, { root: win, rootMargin: "0px 0px 800px 0px" });
    observer.unobserve(bar);
    observer.observe(bar);
  }

  /** 读一页；超时与作废同 older（ChatHistory 一次只管一个请求，这里要并发两个） */
  async function read(path: string, signal: AbortSignal) {
    const timer = setTimeout(() => request?.abort(), hooks.timeout?.() ?? 120_000);
    try { return await hooks.read(path, signal); } finally { clearTimeout(timer); }
  }
  const page = (r: PageCursor, query: string) =>
    `/sessions/${hooks.session()}/history?thread=${encodeURIComponent(r.thread)}&${query}`;

  function setOpen(open: boolean) {
    if (open && !win.isConnected) hooks.live().after(win);
    win.hidden = !open;
    hooks.live().classList.toggle("behind", open);
    hooks.switched();
  }

  function cancel() {
    seq++;
    busy = false;
    request?.abort();
    request = undefined;
    older.cancel();
    observer?.disconnect();
    observer = undefined;
  }

  function close() {
    cancel();
    if (!range) return;
    range = null;
    forgetQuestionCards(win);
    win.replaceChildren();
    setOpen(false);
  }

  /** 窗口已接上实时对话流的起点：整段并过去，换回实时对话流，视口停在原处 */
  function merge(box: HTMLElement, r: PageCursor & { end: number }, scrollTop: number | null) {
    if (!hooks.absorb(box, r)) return false;
    cancel();
    range = null;
    win.replaceChildren();
    if (!win.hidden) setOpen(false);
    // 两边的顶部加载条同在或同不在、内容一样，滚动位置可以直接沿用
    if (scrollTop !== null) hooks.live().scrollTop = scrollTop;
    return true;
  }

  /** 往后拉一页；拉到实时对话流的起点就并过去 */
  async function newer() {
    const r = range;
    if (!r || busy) return;
    const live = hooks.liveCursor();
    if (!live || live.thread !== r.thread) return;
    if (r.end >= live.start) { mergeWindow(); return; }
    busy = true;
    const mine = seq, ctl = request = new AbortController();
    show("loading");
    try {
      const value = await read(page(r, `after=${r.end}&until=${live.start}`), ctl.signal);
      if (mine !== seq || range !== r) return;
      if (value.thread_id !== r.thread || value.start !== r.end || typeof value.end !== "number" || value.end <= r.end) throw new Error("history page mismatch");
      const box = hooks.render(value);
      joinTurn(bar.previousElementSibling, box.firstElementChild);
      bar.before(...box.children);
      r.end = value.end;
      hooks.changed();
      if (r.end >= live.start) mergeWindow();
      else { show("idle"); watch(); }
    } catch (_) {
      if (mine === seq && range === r) show("error");
    } finally {
      if (mine === seq) busy = false;
    }
  }

  function mergeWindow() {
    const r = range;
    if (!r) return;
    const top = win.scrollTop;
    const box = document.createElement("div");
    box.append(...[...win.children].filter(el => !el.classList.contains("chat-older")));
    const start = older.cursor()?.start ?? r.start;
    if (!merge(box, { thread: r.thread, start, end: r.end }, top)) {
      // 实时对话流期间重拉过、起点变了：内容放回去，接着按新的起点往后拉
      bar.before(...box.children);
      show("idle");
      watch();
    }
  }

  return {
    el: win,
    isOpen: () => !!range,
    /** 打开以 line（一条用户消息的历史游标）为起点的窗口。line 已在实时对话流的范围里、
     * 或请求失败时返回 false；拉到的一段正好接上实时对话流时直接并进去，不开窗口。 */
    async open(line: number): Promise<boolean> {
      const session = hooks.session(), live = hooks.liveCursor();
      if (!session || !live || line < 0 || line >= live.start) return false;
      // 只作废在途的请求：拉失败时已开着的窗口照常可用
      seq++;
      busy = false;
      request?.abort();
      const mine = seq, ctl = request = new AbortController();
      const r = { thread: live.thread, start: line };
      try {
        const [after, before] = await Promise.all([
          read(page(r, `after=${line}&until=${live.start}`), ctl.signal),
          line > 0 ? read(page(r, `before=${line}`), ctl.signal) : Promise.resolve(null),
        ]);
        if (mine !== seq || session !== hooks.session()) return false;
        if (after.thread_id !== r.thread || after.start !== line || typeof after.end !== "number") throw new Error("history page mismatch");
        if (before && (before.thread_id !== r.thread || typeof before.start !== "number" || before.start >= line)) throw new Error("history page mismatch");
        const box = hooks.render(after);
        let start = line;
        if (before?.entries.length) {
          const head = hooks.render(before);
          joinTurn(head.lastElementChild, box.firstElementChild);
          box.prepend(...head.children);
          start = before.start!;
        }
        const next = { thread: r.thread, start, end: after.end };
        // 目标离已加载的内容只有一两页：直接并进实时对话流，与往上滚加载更早一页无异
        if (after.end >= live.start && merge(box, next, null)) return true;
        forgetQuestionCards(win);
        older.cancel();
        win.replaceChildren(...box.children, bar);
        range = next;
        older.reset({ thread: r.thread, start, more: start > 0 });
        setOpen(true);
        show("idle");
        watch();
        return true;
      } catch (_) {
        return false;
      } finally {
        if (request === ctl) request = undefined;
      }
    },
    close,
  };
}
