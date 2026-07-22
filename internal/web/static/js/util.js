/* util：DOM / 格式化 / 加载态 / 气泡 / 灯箱等无业务依赖的小工具。 */
"use strict";

export const $ = (id) => document.getElementById(id);

/* 全站统一移动端断点，与 css/base.css 的约定一致 */
const mq = window.matchMedia("(max-width: 760px)");
export const isMobile = () => mq.matches;
export const onMobileChange = (fn) => mq.addEventListener("change", fn);

/* ---- 加载态 ---- */

export function spinEl() {
  const s = document.createElement("span");
  s.className = "spin";
  return s;
}

export function withSpin(text) {
  const f = document.createDocumentFragment();
  f.append(spinEl(), document.createTextNode(text));
  return f;
}

/* 按钮进入/退出加载态：转圈 + 禁用，防止重复提交 */
const btnSaved = new WeakMap(); // btn -> 原始子节点，含图标等元素，不能退化成纯文本

export function btnBusy(btn, label) {
  if (btn.classList.contains("loading")) return;
  btnSaved.set(btn, [...btn.childNodes]);
  btn.disabled = true;
  btn.classList.add("loading");
  btn.replaceChildren(spinEl(), document.createTextNode(label));
}

export function btnDone(btn) {
  if (!btn.classList.contains("loading")) return;
  btn.classList.remove("loading");
  btn.replaceChildren(...(btnSaved.get(btn) || []));
  btnSaved.delete(btn);
  btn.disabled = false;
}

/* 工作区生命周期遮罩：启动/停止时罩住头部按钮以下的整块工作区，
 * 毛玻璃 + 绿/红点缀，给窄屏（按钮藏在 ⋯ 菜单里）一个明确的进行中反馈。 */
const WB_BUSY_LABEL = { start: "正在启动", stop: "正在停止" };
export function wbBusy(kind) {
  const el = $("wb-busy");
  if (!el) return;
  $("wb-busy-label").textContent = WB_BUSY_LABEL[kind] || "处理中";
  el.classList.remove("start", "stop");
  el.classList.add(kind, "show");
}
export function wbIdle() {
  $("wb-busy")?.classList.remove("show");
}

/* ---- 格式化 ---- */

export function fmtTime(ms) {
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function fmtSize(n) {
  if (typeof n !== "number") return "";
  if (n < 1024) return n + " B";
  if (n < 1 << 20) return (n / 1024).toFixed(1) + " KB";
  return (n / (1 << 20)).toFixed(1) + " MB";
}

export function fmtUptime(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d} 天 ${h} 小时`;
  if (h > 0) return `${h} 小时 ${m} 分钟`;
  return `${m} 分钟`;
}

/* 延迟展示：<1ms 收成「<1」，个位数保留一位小数，其余取整 */
export function fmtLatency(ms) {
  if (ms < 1) return "<1 ms";
  if (ms < 10) return ms.toFixed(1) + " ms";
  return Math.round(ms) + " ms";
}

export function insertAtCursor(t, text) {
  const start = t.selectionStart ?? t.value.length;
  t.setRangeText(text, start, t.selectionEnd ?? start, "end");
  t.focus();
}

/* ---- 操作反馈气泡 ---- */

let toastTimer = null;
export function toast(msg, isErr) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.toggle("err", !!isErr);
  t.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove("show"), isErr ? 4200 : 2600);
}

/* ---- 图片灯箱（对话缩略图 / 终端路径预览共用） ---- */

export function openLightbox(url, title) {
  $("lb-img").src = url;
  $("lb-name").textContent = title || "";
  $("dlg-img").showModal();
}

$("lb-close").addEventListener("click", () => $("dlg-img").close());
$("dlg-img").addEventListener("click", (e) => {
  if (e.target === $("dlg-img") || e.target === $("lb-img")) $("dlg-img").close();
});
