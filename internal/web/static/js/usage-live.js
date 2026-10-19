/* 用量变更推送（/api/usage/live）。
 *
 * 使用记录页与终端顶栏「本空间已花」开着时才连：服务端在用量落库（对话结算、终端
 * 补记）后约 0.2 秒推一条只带空间 ID 的通知，这里转成 bus 上的 "usage-changed"，
 * 页面收到后照常走 /usage/events 重读。做法同 cc-switch 的 usage-log-recorded。
 * 原有轮询保留作兜底：推送连不上（代理掐断长连接、旧服务端没有这个接口）时照旧。 */
import { S, emit } from "./state.js";
import { wsURL } from "./api.js";
const owners = new Set();
let active = false;
let socket = null;
let retry = null;
let backoff = 1000;
let connectedBefore = false;
/** 推送此刻连着：轮询可以省掉。 */
export function usageLiveConnected() {
    return socket?.readyState === WebSocket.OPEN;
}
/** 某个界面开始/不再需要推送；没有任何界面需要时断开。 */
export function watchUsageLive(owner, on) {
    if (on)
        owners.add(owner);
    else
        owners.delete(owner);
    sync();
}
function wanted() {
    return active && owners.size > 0 && !!S.token;
}
function sync() {
    if (!wanted())
        close();
    else if (!socket && !retry)
        connect();
}
function connect() {
    let ws;
    try {
        ws = new WebSocket(wsURL("/usage/live"));
    }
    catch {
        schedule();
        return;
    }
    socket = ws;
    ws.onopen = () => {
        backoff = 1000;
        // 断线期间落库的变更收不到通知：重连上以后当作「哪里都可能变了」补一次。
        if (connectedBefore)
            changed(null);
        connectedBefore = true;
    };
    ws.onmessage = e => {
        let msg;
        try {
            msg = JSON.parse(String(e.data));
        }
        catch {
            return;
        }
        if (msg?.type === "usage")
            changed(Array.isArray(msg.sessions) ? msg.sessions : null);
    };
    ws.onclose = e => {
        if (socket !== ws)
            return;
        socket = null;
        // 4000–4999 是服务端有意关的（登录失效），不重连；接口失效会由普通请求的 401 处理。
        if (e.code >= 4000 && e.code < 5000)
            return;
        schedule();
    };
}
function schedule() {
    if (!wanted() || retry)
        return;
    retry = setTimeout(() => { retry = null; sync(); }, backoff);
    backoff = Math.min(backoff * 2, 30000);
}
function close() {
    if (retry) {
        clearTimeout(retry);
        retry = null;
    }
    const ws = socket;
    socket = null;
    connectedBefore = false;
    backoff = 1000;
    if (!ws)
        return;
    ws.onopen = ws.onmessage = ws.onclose = null;
    ws.close();
}
function changed(sessions) {
    emit("usage-changed", { sessions });
}
/** 登录期间的生命周期：退出登录时断开并忘掉所有界面的订阅。 */
export function initUsageLive() {
    active = true;
    sync();
    return () => {
        active = false;
        owners.clear();
        close();
    };
}
