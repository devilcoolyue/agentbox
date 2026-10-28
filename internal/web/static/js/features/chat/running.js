/* 并行对话：同一空间的几个对话可以同时运行（服务端 chat.go 的 chatTurn，最多 3 个）。
 * 这里记着哪些对话正在跑、从什么时候开始、在等什么——来自连接时的 status 快照和带 thread 的
 * status 事件，供对话列表、切换栏标记，以及切回某个对话时恢复执行状态与计时。 */
/** 与服务端 maxParallelChats 一致：超过时新消息排队，空出位置后自动开始 */
export const MAX_PARALLEL_CHATS = 3;
const running = new Map();
const listeners = new Set();
function changed() {
    for (const fn of listeners)
        fn();
}
export function runningThread(id) {
    return running.get(id);
}
export function runningCount() {
    return running.size;
}
export function runningIDs() {
    return [...running.keys()];
}
export function onRunningChange(fn) {
    listeners.add(fn);
    return () => listeners.delete(fn);
}
/** 连接（含重连）时服务端给的完整快照：以它为准，替换本地记的 */
export function setRunningSnapshot(threads) {
    running.clear();
    const now = Date.now();
    for (const [id, st] of Object.entries(threads)) {
        running.set(id, { startedAt: now - Math.max(0, st?.elapsed_ms || 0), phase: st?.phase });
    }
    changed();
}
/** 某个对话的 status 事件：running 记下（保留最早的开始时间），其余状态即结束 */
export function noteThreadState(id, state, phase) {
    if (state === "running") {
        const prev = running.get(id);
        running.set(id, { startedAt: prev?.startedAt || Date.now(), phase: phase !== undefined ? phase : prev?.phase });
    }
    else if (!running.delete(id)) {
        return;
    }
    changed();
}
/** 换空间、退出登录：清空 */
export function resetRunning() {
    if (!running.size)
        return;
    running.clear();
    changed();
}
