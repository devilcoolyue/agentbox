/* 用量与系统通告的变更推送（/api/usage/live）。
 *
 * 有界面需要时才连（系统通告横幅登录后一直需要，所以实际上登录期间常连）：服务端
 * 在用量落库（对话结算、终端补记）后约 0.2 秒推一条只带空间 ID 的通知，这里转成
 * bus 上的 "usage-changed"，页面收到后照常走 /usage/events 重读，做法同 cc-switch
 * 的 usage-log-recorded；系统通告变了推 {"type":"announcement"}，转成
 * "announcement-changed"。原有轮询保留作兜底：推送连不上（代理掐断长连接、旧服务端
 * 没有这个接口）时照旧。 */
import { S, emit } from "./state.js";
import { wsURL } from "./api.js";
import type { UsageLiveNotice } from "./types.js";

/** "usage-changed" 的 detail；sessions 为 null 表示哪个空间都可能变了
 *（服务端没逐个列出，或推送断过一阵、重连后补的这一次）。 */
export interface UsageChange { sessions: string[] | null }

const owners = new Set<string>();
let active = false;
let socket: WebSocket | null = null;
let retry: ReturnType<typeof setTimeout> | null = null;
let backoff = 1000;
let connectedBefore = false;

/** 推送此刻连着：轮询可以省掉。 */
export function usageLiveConnected() {
  return socket?.readyState === WebSocket.OPEN;
}

/** 某个界面开始/不再需要推送；没有任何界面需要时断开。 */
export function watchUsageLive(owner: string, on: boolean) {
  if (on) owners.add(owner);
  else owners.delete(owner);
  sync();
}

function wanted() {
  return active && owners.size > 0 && !!S.token;
}

function sync() {
  if (!wanted()) close();
  else if (!socket && !retry) connect();
}

function connect() {
  let ws: WebSocket;
  try { ws = new WebSocket(wsURL("/usage/live")); } catch { schedule(); return; }
  socket = ws;
  ws.onopen = () => {
    backoff = 1000;
    // 断线期间的变更收不到通知：重连上以后当作「哪里都可能变了」补一次。服务重启
    // （发版）后通告多半变了，第一次连上也补一次通告。
    if (connectedBefore) changed(null);
    emit("announcement-changed");
    connectedBefore = true;
  };
  ws.onmessage = e => {
    let msg: UsageLiveNotice;
    try { msg = JSON.parse(String(e.data)) as UsageLiveNotice; } catch { return; }
    if (msg?.type === "usage") changed(Array.isArray(msg.sessions) ? msg.sessions : null);
    else if (msg?.type === "announcement") emit("announcement-changed");
  };
  ws.onclose = e => {
    if (socket !== ws) return;
    socket = null;
    // 4000–4999 是服务端有意关的（登录失效），不重连；接口失效会由普通请求的 401 处理。
    if (e.code >= 4000 && e.code < 5000) return;
    schedule();
  };
}

function schedule() {
  if (!wanted() || retry) return;
  retry = setTimeout(() => { retry = null; sync(); }, backoff);
  backoff = Math.min(backoff * 2, 30000);
}

function close() {
  if (retry) { clearTimeout(retry); retry = null; }
  const ws = socket;
  socket = null;
  connectedBefore = false;
  backoff = 1000;
  if (!ws) return;
  ws.onopen = ws.onmessage = ws.onclose = null;
  ws.close();
}

function changed(sessions: string[] | null) {
  emit("usage-changed", { sessions } satisfies UsageChange);
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
