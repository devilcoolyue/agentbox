/* tunnel：内网反向隧道 —— 侧栏状态入口 + 状态/接入指引弹窗。
 * 状态随 data-updated 节流刷新（≥15s 一次），弹窗打开时立即刷新一次。
 * 普通用户在功能未启用时看不到入口；管理员始终可见（含「前往设置」引导）。 */
"use strict";

import { S, bus, emit } from "./state.js";
import { $, toast, fmtUptime } from "./util.js";
import { api } from "./api.js";

let st = null; // GET /api/tunnel/status 缓存
let lastFetch = 0;

async function refreshStatus(force) {
  if (!force && Date.now() - lastFetch < 15000) return;
  lastFetch = Date.now();
  try {
    st = await api("/tunnel/status");
  } catch (_) {
    return; // 网络抖动保持现状
  }
  renderSideItem();
  if ($("dlg-tunnel").open) renderDialog();
}

/* ---- 侧栏入口 ---- */

function renderSideItem() {
  const btn = $("btn-tunnel");
  if (!st || (!st.enabled && S.role !== "admin")) {
    btn.classList.add("hidden");
    return;
  }
  btn.classList.remove("hidden");
  const dot = $("tunnel-dot");
  dot.classList.toggle("on", !!st.connected);
  $("tunnel-text").textContent = !st.enabled ? "未启用" : (st.connected ? "在线" : "离线");
}

/* ---- 弹窗 ---- */

function renderDialog() {
  if (!st) return;
  const conn = !!st.connected;
  $("tun-st-dot").classList.toggle("on", conn);
  $("tun-st-title").textContent = !st.enabled ? "功能未启用" : (conn ? "隧道在线" : "等待接入");
  let sub = "";
  if (conn) {
    sub = "来自 " + (st.remote || "?") + " · 已连接 " + fmtUptime(Date.now() - st.since);
  } else if (st.enabled) {
    sub = "SOCKS5 代理 " + st.proxy + (st.proxy_up ? " 就绪" : " 未监听");
  }
  $("tun-st-sub").textContent = sub;

  // 端口映射列表
  const mapsBox = $("tun-maps");
  mapsBox.replaceChildren();
  const maps = st.maps || [];
  mapsBox.classList.toggle("hidden", !maps.length);
  for (const m of maps) {
    const row = document.createElement("div");
    row.className = "tun-map mono";
    row.textContent = m.listen + "  →  " + m.target;
    mapsBox.appendChild(row);
  }

  // 管理员：在线用户一览
  const adm = $("tun-admin-online");
  const online = st.online_users;
  adm.classList.toggle("hidden", !(S.role === "admin" && online));
  if (online) adm.textContent = "在线隧道用户：" + (online.length ? online.join("、") : "无");

  $("tun-off-admin").classList.toggle("hidden", st.enabled);
  $("tun-guide").classList.toggle("hidden", !st.enabled);
  if (st.enabled) $("tun-cmd").textContent = cmdText();
}

function cmdText() {
  return "ABOX_PASSWORD=你的登录密码 ./abox-link \\\n" +
    "  --server " + location.origin + " \\\n" +
    "  --user " + S.user + " \\\n" +
    "  --allow 192.168.1.0/24 --allow db.corp.local:5432 \\\n" +
    "  --map 3306=10.0.1.5:3306";
}

/* 平台名映射：文件名里带 os-arch，展示成人话 */
function platformLabel(name) {
  const n = name.toLowerCase();
  if (n.includes("windows")) return "Windows";
  if (n.includes("darwin")) return n.includes("arm64") ? "macOS（Apple 芯片）" : "macOS（Intel）";
  if (n.includes("linux")) return n.includes("arm64") ? "Linux（arm64）" : "Linux";
  return name;
}

async function loadClients() {
  let list = [];
  try { list = await api("/tunnel/clients"); } catch (_) {}
  const box = $("tun-dl");
  box.replaceChildren();
  $("tun-dl-empty").classList.toggle("hidden", !!list.length);
  for (const c of list) {
    const a = document.createElement("a");
    a.className = "btn btn-sm tun-dl-btn";
    a.textContent = platformLabel(c.name);
    a.title = c.name + " · " + (c.size / 1048576).toFixed(1) + " MB";
    a.href = "/api/tunnel/clients/" + encodeURIComponent(c.name) +
      "?token=" + encodeURIComponent(S.token);
    a.setAttribute("download", c.name);
    box.appendChild(a);
  }
}

$("btn-tunnel").addEventListener("click", () => {
  $("dlg-tunnel").showModal();
  renderDialog();
  refreshStatus(true);
  loadClients();
});
$("tun-close").addEventListener("click", () => $("dlg-tunnel").close());

$("tun-goto-settings").addEventListener("click", () => {
  $("dlg-tunnel").close();
  S.sec = "security";
  emit("open-settings");
});

$("tun-copy").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText($("tun-cmd").textContent);
    toast("命令已复制，替换密码与放行规则后在你的电脑上运行");
  } catch (_) {
    toast("复制失败，请手动选中复制", true);
  }
});

/* 弹窗打开期间 5s 一刷，让「等待接入 → 在线」立刻可见 */
setInterval(() => {
  if ($("dlg-tunnel").open) refreshStatus(true);
}, 5000);

bus.addEventListener("data-updated", () => refreshStatus(false));
