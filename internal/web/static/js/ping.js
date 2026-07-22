/* ping：测量到服务器的往返延迟，点亮侧栏底部的连接指示灯。
 * 打不带鉴权的 /api/ping（no-store + 时间戳，避免 CF/浏览器缓存让读数失真），
 * 每 5s 一次；后台标签页跳过测量。绿/黄/红 = 良好 / 偏高 / 糟糕或断开。 */
"use strict";

import { $, fmtLatency } from "./util.js";

const INTERVAL = 5000;
const TIMEOUT = 8000;
let timer = 0;

async function measure() {
  if (document.hidden) return; // 后台标签不必刷，回到前台下一拍即恢复
  const ctrl = new AbortController();
  const to = setTimeout(() => ctrl.abort(), TIMEOUT);
  const t0 = performance.now();
  try {
    const res = await fetch("/api/ping?t=" + Date.now(), { cache: "no-store", signal: ctrl.signal });
    if (!res.ok) throw new Error("bad");
    render(performance.now() - t0, true);
  } catch (_) {
    render(0, false);
  } finally {
    clearTimeout(to);
  }
}

function render(ms, ok) {
  const dot = $("conn-dot"), text = $("conn-text");
  if (!dot || !text) return;
  dot.className = "t-dot " + (!ok ? "bad" : ms < 120 ? "good" : ms < 350 ? "warn" : "bad");
  text.textContent = ok ? "连接 " + fmtLatency(ms) : "连接 已断开";
}

/* 登录进入主界面后启动；重复调用只保留一个循环。 */
export function startPing() {
  if (timer) return;
  measure();
  timer = setInterval(measure, INTERVAL);
}
