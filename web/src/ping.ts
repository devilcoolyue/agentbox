/* ping：测量到服务器的往返延迟，点亮侧栏底部的连接指示灯。
 * 打不带鉴权的 /api/ping（no-store + 时间戳，避免 CF/浏览器缓存让读数失真），
 * 每 5s 一次；后台标签页跳过测量。绿/黄/红 = 良好 / 偏高 / 糟糕或断开。 */
"use strict";

import { $, fmtLatency } from "./util.js";
import { refreshAll } from "./data.js";
import { setTip } from "./tip.js";

const INTERVAL = 5000;
const TIMEOUT = 8000;
/* 连续失败到这个次数才亮横幅：一次超时多半只是网络抖动，立刻弹横幅反而吵。 */
const OFFLINE_AFTER = 2;
let timer = 0;
let fails = 0;

async function measure() {
  if (document.hidden) return; // 后台标签不必刷，回到前台下一拍即恢复
  const ctrl = new AbortController();
  const to = setTimeout(() => ctrl.abort(), TIMEOUT);
  const t0 = performance.now();
  try {
    const res = await fetch("/api/ping?t=" + Date.now(), { cache: "no-store", signal: ctrl.signal });
    if (!res.ok) throw new Error("bad");
    const wasOffline = fails >= OFFLINE_AFTER;
    fails = 0;
    render(performance.now() - t0, true);
    setOffline(false);
    // 断线期间的状态变化（会话被停、被别的端删除）没被 8 秒轮询拿到，补一次
    if (wasOffline) refreshAll();
  } catch (_) {
    fails++;
    render(0, false);
    setOffline(fails >= OFFLINE_AFTER);
  } finally {
    clearTimeout(to);
  }
}

function setOffline(on: boolean) {
  const el = $("offline-banner");
  if (el) el.classList.toggle("hidden", !on);
}

function render(ms: number, ok: boolean) {
  const dot = $("conn-dot"), text = $("conn-text");
  if (!dot || !text) return;
  dot.className = "t-dot " + (!ok ? "bad" : ms < 120 ? "good" : ms < 350 ? "warn" : "bad");
  text.textContent = ok ? "连接 " + fmtLatency(ms) : "连接 已断开";
  setTip($("conn-latency"), ok ? "到服务器的连接延迟：" + fmtLatency(ms) : "与服务器的连接已断开");
  $("conn-latency").setAttribute("aria-label", text.textContent);
}

/* 从后台切回前台：轮询这段时间是停摆的，立刻测一次并刷新数据，
 * 免得用户看着一屏 8 秒前（或几分钟前）的旧状态。 */
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && timer) { measure(); refreshAll(); }
});

/* 登录进入主界面后启动；重复调用只保留一个循环。 */
export function startPing() {
  if (timer) return;
  measure();
  timer = setInterval(measure, INTERVAL);
}
