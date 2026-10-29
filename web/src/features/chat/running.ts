/* 并行对话：同一空间的几个对话可以同时运行（服务端 chat.go 的 chatTurn，上限见 maxParallelChats）。
 * 这里记着哪些对话正在跑、从什么时候开始、在等什么——来自连接时的 status 快照和带 thread 的
 * status 事件，供对话列表、切换栏标记，以及切回某个对话时恢复执行状态与计时。 */

export interface RunningThread {
  /** 回合开始的本机时间（由服务端给的已进行毫秒数折算，不受两端时钟差影响） */
  startedAt: number;
  /** "starting" 是在启动工作空间，"" 或没有是已在执行 */
  phase?: string;
}

/** 服务端的同时运行上限（系统设置 max_parallel_chats）：连接时的 status 与改设置后的 chat_limit
 * 带来；超过时新消息排队，空出位置后自动开始。旧服务端不发，按 3 */
let maxParallel = 3;

export function maxParallelChats(): number {
  return maxParallel;
}

export function setMaxParallelChats(n: unknown) {
  if (typeof n === "number" && Number.isInteger(n) && n > 0) maxParallel = n;
}

const running = new Map<string, RunningThread>();
const listeners = new Set<() => void>();

function changed() {
  for (const fn of listeners) fn();
}

export function runningThread(id: string): RunningThread | undefined {
  return running.get(id);
}

export function runningCount(): number {
  return running.size;
}

export function runningIDs(): string[] {
  return [...running.keys()];
}

export function onRunningChange(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** 连接（含重连）时服务端给的完整快照：以它为准，替换本地记的 */
export function setRunningSnapshot(threads: Record<string, { elapsed_ms?: number; phase?: string }>) {
  running.clear();
  const now = Date.now();
  for (const [id, st] of Object.entries(threads)) {
    running.set(id, { startedAt: now - Math.max(0, st?.elapsed_ms || 0), phase: st?.phase });
  }
  changed();
}

/** 某个对话的 status 事件：running 记下（保留最早的开始时间），其余状态即结束 */
export function noteThreadState(id: string, state: string, phase?: string) {
  if (state === "running") {
    const prev = running.get(id);
    running.set(id, { startedAt: prev?.startedAt || Date.now(), phase: phase !== undefined ? phase : prev?.phase });
  } else if (!running.delete(id)) {
    return;
  }
  changed();
}

/** 换空间、退出登录：清空 */
export function resetRunning() {
  if (!running.size) return;
  running.clear();
  changed();
}
