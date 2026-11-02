/* 终端页的人为操作上报：管理员「登录与活跃」面板的最后活跃时间就来自这里。
 *
 * 只认浏览器发出的真实事件（isTrusted）。不能用 xterm 的 onData 判断：程序查询光标
 * 位置、设备属性时 xterm 也从 onData 自动应答，焦点和鼠标上报同样走这条路，Agent
 * 自己跑着就会被当成有人在操作。
 *
 * 限流：第一次操作立刻上报，之后最多每 INTERVAL 一次，窗口内的操作在窗口结束时补报，
 * 所以记录的时间最多比实际晚一个窗口。没连上（send 返回 false）就不补，下一次操作再试。 */

const EVENTS = ["pointerdown", "keydown", "input", "paste", "drop"];
const INTERVAL = 15_000;

interface ActivitySource {
  addEventListener(type: string, listener: (event: Event) => void, options: AddEventListenerOptions): void;
}

export function watchTermActivity(root: ActivitySource, send: () => boolean) {
  let last = -Infinity;
  let timer: ReturnType<typeof setTimeout> | null = null;
  const flush = () => {
    timer = null;
    if (send()) last = Date.now();
  };
  const onEvent = (event: Event) => {
    if (!event.isTrusted || timer) return;
    const wait = last + INTERVAL - Date.now();
    if (wait <= 0) flush();
    else timer = setTimeout(flush, wait);
  };
  // 捕获阶段：xterm 处理按键时会阻止冒泡。
  for (const type of EVENTS) root.addEventListener(type, onEvent, { capture: true, passive: true });
}
