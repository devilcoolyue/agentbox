/* abox-link 控制台前端：轮询 /api/state 渲染整页，改动先存在本地 draft，
 * 点「保存并应用」才提交。日志按 seq 增量拉取。 */
"use strict";

const $ = (id) => document.getElementById(id);

/* 面板 API：X-Abox-Panel 头是防跨站的凭据——浏览器不会让外站页面带上它。 */
async function api(path, body) {
  const res = await fetch("/api" + path, {
    method: body === undefined ? "GET" : "POST",
    headers: { "X-Abox-Panel": "1", "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

let state = null;   // 服务端权威状态
let draft = null;   // 用户正在编辑的副本；null 表示无未保存改动
let logSeq = 0;
let rulesKey = null; // 当前画出来的规则行签名，值没变就不重建

/* ---- 渲染 ---- */

const ST = {
  online:      { cls: "on",   title: "隧道在线",   btn: "停止" },
  connecting:  { cls: "busy", title: "正在连接…",  btn: "停止" },
  auth_failed: { cls: "bad",  title: "凭证已失效", btn: "启动" },
  stopped:     { cls: "",     title: "未连接",     btn: "启动" },
};

function render() {
  if (!state) return;
  const paired = state.paired;

  $("card-pair").classList.toggle("hidden", paired);
  $("card-status").classList.toggle("hidden", !paired);
  $("panel-body").classList.toggle("hidden", !paired);
  $("hdr-acct").textContent = paired ? state.user + " @ " + hostOf(state.server) : "";

  const st = state.status || { state: "stopped" };
  const look = ST[st.state] || ST.stopped;
  $("hdr-dot").className = "dot " + look.cls;
  $("st-dot").className = "dot big " + look.cls;
  $("st-title").textContent = look.title;
  $("st-sub").textContent = subtitle(st);
  $("btn-toggle").textContent = look.btn;

  renderRules();
  renderOptions();
  $("savebar").classList.toggle("hidden", !draft);
}

function subtitle(st) {
  if (st.state === "online") {
    const parts = ["已连接 " + uptime(Date.now() - st.since)];
    if (typeof st.ping_ms === "number" && st.ping_ms >= 0) {
      parts.push("延迟 " + fmtLatency(st.ping_ms));
    }
    for (const m of st.maps || []) {
      parts.push("映射 " + m.port + (m.ok ? "" : "（失败：" + m.detail + "）"));
    }
    return parts.join(" · ");
  }
  if (st.detail) return st.detail;
  return state.server ? hostOf(state.server) : "";
}

/* draft 里存的是编辑中的值，没有 draft 就直接看 state */
function view() {
  return draft || {
    allow: state.allow.slice(),
    maps: state.maps.slice(),
    auto_connect: state.auto_connect,
    insecure: state.insecure,
  };
}

/* 开始编辑：把当前值快照成 draft，并重画（增删行需要） */
function edit(mutate) {
  draft = draft || view();
  mutate(draft);
  render();
}

/* 输入框里的改动：只更新 draft，不重画行——重画会毁掉正在输入的那个输入框 */
function editValue(mutate) {
  draft = draft || view();
  mutate(draft);
  rulesKey = keyOf(draft); // 画面已经是最新值了，别让下一轮轮询把行重建掉
  $("savebar").classList.remove("hidden");
}

function keyOf(v) {
  return JSON.stringify([v.allow, v.maps]);
}

function renderRules() {
  const v = view();

  // 轮询每 2 秒调一次 render()，这里必须只在规则真的变了时才重建 DOM，
  // 否则正在编辑的输入框会被换掉，焦点和光标位置跟着丢。
  const key = keyOf(v);
  if (key === rulesKey) return;
  rulesKey = key;

  const allowBox = $("allow-list");
  allowBox.replaceChildren();
  v.allow.forEach((rule, i) => {
    allowBox.appendChild(ruleRow(rule, "192.168.1.0/24", (val) => {
      editValue((d) => { d.allow[i] = val; });
    }, () => {
      edit((d) => { d.allow.splice(i, 1); });
    }));
  });

  const mapBox = $("map-list");
  mapBox.replaceChildren();
  v.maps.forEach((spec, i) => {
    const [port, target] = splitMap(spec);
    mapBox.appendChild(mapRow(port, target, (p, t) => {
      editValue((d) => { d.maps[i] = p + "=" + t; });
    }, () => {
      edit((d) => { d.maps.splice(i, 1); });
    }));
  });
}

/* 每敲一个字符就同步进 draft（不重绘），这样轮询刷新不会吞掉还没失焦的输入 */
function input(value, placeholder, onCommit) {
  const el = document.createElement("input");
  el.type = "text";
  el.value = value;
  el.placeholder = placeholder;
  el.spellcheck = false;
  el.addEventListener("input", () => onCommit(el.value.trim()));
  el.addEventListener("keydown", (e) => { if (e.key === "Enter") el.blur(); });
  return el;
}

function removeBtn(onClick) {
  const b = document.createElement("button");
  b.type = "button";
  b.textContent = "✕";
  b.title = "删除";
  b.addEventListener("click", onClick);
  return b;
}

function ruleRow(value, placeholder, onCommit, onRemove) {
  const row = document.createElement("div");
  row.className = "rule";
  row.append(input(value, placeholder, onCommit), removeBtn(onRemove));
  return row;
}

function mapRow(port, target, onCommit, onRemove) {
  const row = document.createElement("div");
  row.className = "rule";
  const p = input(port, "3306", () => onCommit(p.value.trim(), t.value.trim()));
  const t = input(target, "10.0.1.5:3306", () => onCommit(p.value.trim(), t.value.trim()));
  p.style.maxWidth = "110px";
  const sep = document.createElement("span");
  sep.className = "sep";
  sep.textContent = "→";
  row.append(p, sep, t, removeBtn(onRemove));
  return row;
}

function renderOptions() {
  const v = view();
  $("opt-auto").checked = v.auto_connect;
  $("opt-insecure").checked = v.insecure;

  const boot = state.autostart || {};
  $("opt-boot").checked = !!boot.installed;
  $("opt-boot").disabled = !boot.supported;
  $("boot-detail").textContent = boot.detail || "";
}

/* ---- 日志 ---- */

async function pollLogs() {
  let data;
  try {
    data = await api("/logs?since=" + logSeq);
  } catch (_) {
    return;
  }
  if (!data.lines.length) return;
  const box = $("log");
  const stuck = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  for (const line of data.lines) {
    const div = document.createElement("div");
    const t = document.createElement("span");
    t.className = "t";
    t.textContent = new Date(line.time).toLocaleTimeString("zh-CN", { hour12: false });
    div.append(t, document.createTextNode(line.text));
    box.appendChild(div);
  }
  while (box.childElementCount > 500) box.removeChild(box.firstChild);
  if (stuck) box.scrollTop = box.scrollHeight;
  logSeq = data.latest;
}

/* ---- 事件 ---- */

$("btn-pair").addEventListener("click", async () => {
  const code = $("pair-code").value.trim();
  $("pair-err").textContent = "";
  if (!code) { $("pair-err").textContent = "请先粘贴配对码"; return; }
  await withBusy($("btn-pair"), async () => {
    try {
      state = await api("/pair", { code, insecure: $("pair-insecure").checked });
      $("pair-code").value = "";
      render();
      toast("接入成功，接下来添加放行规则");
    } catch (e) {
      $("pair-err").textContent = e.message;
    }
  });
});

$("btn-toggle").addEventListener("click", async () => {
  const running = state.running;
  await withBusy($("btn-toggle"), async () => {
    try {
      state = await api(running ? "/stop" : "/start", {});
      render();
    } catch (e) {
      toast(e.message, true);
    }
  });
});

$("btn-add-allow").addEventListener("click", () => edit((d) => d.allow.push("")));
$("btn-add-map").addEventListener("click", () => edit((d) => d.maps.push("=")));

$("opt-auto").addEventListener("change", (e) => edit((d) => { d.auto_connect = e.target.checked; }));
$("opt-insecure").addEventListener("change", (e) => edit((d) => { d.insecure = e.target.checked; }));

/* 开机自启是立即生效的系统操作，不走 draft */
$("opt-boot").addEventListener("change", async (e) => {
  const enable = e.target.checked;
  try {
    state = await api("/autostart", { enable });
    render();
    toast(enable ? "已设置开机自启" : "已取消开机自启");
  } catch (err) {
    toast(err.message, true);
    await refresh();
  }
});

$("btn-save").addEventListener("click", async () => {
  const v = view();
  // 空行是用户加了输入框却没填，静默丢掉即可
  const payload = {
    allow: v.allow.filter((s) => s.trim()),
    maps: v.maps.filter((s) => s.trim() && s !== "="),
    auto_connect: v.auto_connect,
    insecure: v.insecure,
  };
  await withBusy($("btn-save"), async () => {
    try {
      state = await api("/config", payload);
      draft = null;
      render();
      toast(state.running ? "已保存，隧道按新规则重连中" : "已保存");
    } catch (e) {
      toast(e.message, true);
    }
  });
});

$("btn-revert").addEventListener("click", () => { draft = null; render(); });

$("btn-unpair").addEventListener("click", async () => {
  if (!confirm("解除绑定后需要重新配对才能连接，放行规则会保留。继续？")) return;
  try {
    state = await api("/unpair", {});
    draft = null;
    render();
  } catch (e) {
    toast(e.message, true);
  }
});

/* ---- 工具 ---- */

function hostOf(url) {
  try { return new URL(url).host; } catch (_) { return url || ""; }
}

function splitMap(spec) {
  const i = (spec || "").indexOf("=");
  return i < 0 ? [spec, ""] : [spec.slice(0, i), spec.slice(i + 1)];
}

/* 延迟展示：<1ms 收成「<1」，个位数保留一位小数，其余取整 */
function fmtLatency(ms) {
  if (ms < 1) return "<1 ms";
  if (ms < 10) return ms.toFixed(1) + " ms";
  return Math.round(ms) + " ms";
}

function uptime(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 60) return s + " 秒";
  if (s < 3600) return Math.floor(s / 60) + " 分钟";
  if (s < 86400) return Math.floor(s / 3600) + " 小时 " + Math.floor((s % 3600) / 60) + " 分";
  return Math.floor(s / 86400) + " 天 " + Math.floor((s % 86400) / 3600) + " 小时";
}

async function withBusy(btn, fn) {
  btn.disabled = true;
  try { await fn(); } finally { btn.disabled = false; }
}

let toastTimer = 0;
function toast(msg, bad) {
  const el = $("toast");
  el.textContent = msg;
  el.className = "toast" + (bad ? " bad" : "");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.add("hidden"), 3200);
}

async function refresh() {
  try {
    state = await api("/state");
    render();
  } catch (_) { /* 面板进程没了，下一轮再试 */ }
}

/* 在线时刷得勤一点，让「连接中 → 在线」和运行时长跟得上 */
setInterval(() => {
  refresh();
  pollLogs();
}, 2000);

await refresh();
await pollLogs();
