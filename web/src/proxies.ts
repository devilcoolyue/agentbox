/* proxies：系统设置 → IP 代理。出口代理池的维护（增删改 / 连通性探测 /
 * 批量导入导出），以及账号编辑弹窗里那个「绑定代理」下拉选择器。
 *
 * 账号一旦绑定代理，该账号的一切官方请求都从这个出口走：容器里 agent 发的，
 * 和服务端自己发的 OAuth / Key 探测。所以这里的「停用」是硬开关，不是「绕过
 * 它走直连」——直连等于把服务器真实 IP 交出去，正是绑代理要避免的事。 */
"use strict";

import { actionButton, buttonLabel, svgIcon } from "./icons.js";

import { setSelectValue } from "./select.js";

import type { Proxy, ProxyImport, ProxyList, ProxyTest } from "./types.js";
import { $, btnBusy, btnDone, toast, askConfirm, fmtLatency } from "./util.js";
import { api } from "./api.js";
import { refreshAll } from "./data.js";
import { setTip } from "./tip.js";

/* 代理池缓存：列表页与账号弹窗的下拉共用一份，避免每次开弹窗都打一次接口。 */
let cache: Proxy[] = [];
let bridge: Omit<ProxyList, "proxies"> | null = null;

/* 最近一次探测结果（proxy id -> 结果），刷新列表时保留，用于显示延迟/出口 IP。
 * 只活在内存里：探测是「此刻通不通」，重启后端后再显示旧值只会误导。 */
const probes = new Map<string, ProxyTest>();

export function proxyCache() { return cache; }

export async function loadProxies() {
  const res = await api<ProxyList>("/proxies");
  cache = res.proxies;
  bridge = res;
  paintCount();
  return cache;
}

/* 左侧导航上的条数。进设置页就得是真数，不能等点开 IP 代理才从 0 变过来——
 * 管理员会把那个 0 当成「池子是空的」。 */
function paintCount() {
  $("proxy-count").textContent = String(cache.length);
}

/* 只为点亮计数拉一次列表，表格留到真正点进来时再渲染。 */
export async function refreshProxyCount() {
  try {
    await loadProxies();
  } catch (_) { /* 计数不值得为它弹错误提示，点进分区时会再报一次 */ }
}

/* ---------------- 列表 ---------------- */

export async function openProxiesSection() {
  // 转圈块是居中的一整块，旧表格留在下面会把它顶成「加载中 + 一屏数据」的怪样子，
  // 先收起来（监控页也是这个套路）。
  $("proxy-table-wrap").classList.add("hidden");
  $("proxy-empty").classList.add("hidden");
  $("proxy-loading").classList.remove("hidden");
  try {
    await loadProxies();
  } catch (e) {
    toast("读取代理列表失败：" + (e as Error).message, true);
    if (cache.length) renderProxies(); // 有上一份就还原回去，别让刷新失败清空视野
    return;
  } finally {
    $("proxy-loading").classList.add("hidden");
  }
  renderProxies();
}

function renderBridgeState() {
  if (!bridge) return;
  const el = $("proxy-bridge-state");
  el.replaceChildren();
  const dot = document.createElement("span");
  const on = bridge.bridge_up;
  dot.className = "mon-dot" + (on ? " on" : "");
  const text = !cache.length
    ? "代理池为空，桥接未启动"
    : on
      ? "桥接监听中 · 容器经 " + bridge.bridge_host + " 出网"
      : "桥接未监听，绑定了代理的账号会请求失败" + (bridge.bridge_error ? "：" + bridge.bridge_error : "");
  el.append(dot, document.createTextNode(text));
  el.classList.toggle("bad", !!cache.length && !on);
}

export function renderProxies() {
  renderBridgeState();
  const tb = $("proxy-tbody");
  tb.replaceChildren();
  $("proxy-empty").classList.toggle("hidden", cache.length > 0);
  $("proxy-table-wrap").classList.toggle("hidden", cache.length === 0);
  for (const p of cache) tb.appendChild(proxyRow(p));
}

function cell(text: string, cls?: string) {
  const td = document.createElement("td");
  td.textContent = text;
  if (cls) td.className = cls;
  return td;
}

function proxyRow(p: Proxy) {
  const tr = document.createElement("tr");
  if (p.disabled) tr.className = "off";

  const name = document.createElement("td");
  name.className = "px-name";
  const dot = document.createElement("span");
  dot.className = "mon-dot" + (p.disabled ? "" : " on");
  setTip(dot, p.disabled ? "已停用" : "正常");
  name.append(dot, document.createTextNode(p.name || p.host));

  const scheme = document.createElement("td");
  const chip = document.createElement("span");
  chip.className = "px-scheme";
  chip.textContent = p.scheme.toUpperCase();
  scheme.appendChild(chip);

  const addr = document.createElement("td");
  addr.className = "num";
  const addrBtn = document.createElement("button");
  addrBtn.type = "button";
  addrBtn.className = "px-copy mono";
  actionButton(addrBtn, p.host + ":" + p.port, "copy", "复制代理地址");
  addrBtn.setAttribute("aria-label", "复制代理地址");
  setTip(addrBtn, "点击复制");
  addrBtn.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(p.host + ":" + p.port);
      toast("地址已复制");
    } catch (_) {
      toast("复制失败，请手动选中", true);
    }
  });
  addr.appendChild(addrBtn);

  const auth = cell(p.username ? p.username + (p.has_pass ? " · ••••" : "") : "—", "px-auth");

  const acct = cell(p.accounts ? p.accounts + " 个账号" : "未绑定", "px-acct");
  if (!p.accounts) acct.classList.add("dim");

  const probe = document.createElement("td");
  probe.className = "px-probe";
  const res = probes.get(p.id);
  if (!res) {
    probe.textContent = "—";
    probe.classList.add("dim");
  } else if (res.ok) {
    probe.textContent = fmtLatency(res.latency_ms);
    probe.classList.add("good");
    setTip(probe, "出口 IP " + res.exit_ip + "（" + res.endpoint + "）");
  } else {
    probe.textContent = "不通";
    probe.classList.add("bad");
    setTip(probe, res.error || "");
  }

  const acts = document.createElement("td");
  acts.className = "px-actions"; // 只有三个动作：全部带字，删除放最后
  const test = document.createElement("button");
  test.className = "btn btn-sm btn-ghost";
  buttonLabel(test, "测试", "activity");
  test.addEventListener("click", () => runProbe(p, test));
  const edit = document.createElement("button");
  edit.className = "btn btn-sm btn-ghost";
  actionButton(edit, "编辑", "rename", "编辑代理");
  edit.addEventListener("click", () => openProxyDlg(p));
  const del = document.createElement("button");
  del.className = "btn btn-sm btn-ghost btn-danger";
  actionButton(del, "删除", "trash", "删除代理");
  del.addEventListener("click", () => removeProxy(p));
  acts.append(test, edit, del);

  tr.append(name, scheme, addr, auth, acct, probe, acts);
  return tr;
}

/* 探测：拿一次出口 IP 回显，量的是「经代理到公网」的整条链路。 */
async function runProbe(p: Proxy, btn: HTMLButtonElement) {
  btnBusy(btn, "探测中");
  try {
    const res = await api<ProxyTest>("/proxies/test", {
      method: "POST", body: JSON.stringify({ id: p.id }),
    });
    probes.set(p.id, res);
    if (res.ok) toast(`${p.name || p.host} 连通 · ${fmtLatency(res.latency_ms)} · 出口 ${res.exit_ip}`);
    else toast(`${p.name || p.host} 不通：${res.error}`, true);
  } catch (e) {
    probes.set(p.id, { ok: false, latency_ms: 0, error: (e as Error).message });
    toast("探测失败：" + (e as Error).message, true);
  } finally {
    btnDone(btn);
    renderProxies();
  }
}

async function removeProxy(p: Proxy) {
  const bound = p.accounts > 0;
  const ok = await askConfirm("删除代理「" + (p.name || p.host) + "」？", {
    title: "删除代理",
    hint: bound
      ? `仍有 ${p.accounts} 个账号绑定它。删除后这些账号会解除绑定，改用服务器自身的 IP 直连官方接口。`
      : "该代理未被任何账号使用。",
    okLabel: "删除", icon: "trash", danger: true,
  });
  if (!ok) return;
  try {
    await api("/proxies/" + p.id + (bound ? "?force=1" : ""), { method: "DELETE" });
    probes.delete(p.id);
    toast("代理已删除");
    await openProxiesSection();
    refreshAll(); // 账号行上的代理标签跟着更新
  } catch (e) {
    toast("删除失败：" + (e as Error).message, true);
  }
}

/* ---------------- 新增 / 编辑弹窗 ---------------- */

let editProxy: Proxy | null = null;

function proxyDlgMsg(text: string, isErr?: boolean) {
  const m = $("proxy-msg");
  m.textContent = text || "";
  m.classList.toggle("hidden", !text);
  m.classList.toggle("err", !!isErr);
}

export function openProxyDlg(p: Proxy | null) {
  editProxy = p;
  $("proxy-dlg-title").textContent = p ? "编辑代理" : "添加代理";
  $<HTMLInputElement>("proxy-name").value = p ? p.name : "";
  setSelectValue($<HTMLSelectElement>("proxy-scheme"), p ? p.scheme : "socks5");
  $<HTMLInputElement>("proxy-host").value = p ? p.host : "";
  $<HTMLInputElement>("proxy-port").value = p ? String(p.port) : "";
  $<HTMLInputElement>("proxy-user").value = p && p.username ? p.username : "";
  const pass = $<HTMLInputElement>("proxy-pass");
  pass.value = "";
  // 密码不下发，编辑时留空即保持原样——占位符把这件事说清楚，免得管理员
  // 以为密码丢了又重敲一遍。
  pass.placeholder = p && p.has_pass ? "已保存，留空表示不修改" : "可选";
  setSelectValue($<HTMLSelectElement>("proxy-state"), p && p.disabled ? "off" : "on");
  proxyDlgMsg("");
  $<HTMLDialogElement>("dlg-proxy").showModal();
}

function proxyDlgBody() {
  return {
    name: $<HTMLInputElement>("proxy-name").value.trim(),
    scheme: $<HTMLSelectElement>("proxy-scheme").value,
    host: $<HTMLInputElement>("proxy-host").value.trim(),
    port: Number($<HTMLInputElement>("proxy-port").value),
    username: $<HTMLInputElement>("proxy-user").value.trim(),
    password: $<HTMLInputElement>("proxy-pass").value,
    disabled: $<HTMLSelectElement>("proxy-state").value === "off",
  };
}

$("proxy-cancel").addEventListener("click", () => $<HTMLDialogElement>("dlg-proxy").close());

$("proxy-test").addEventListener("click", async () => {
  const body = proxyDlgBody();
  if (!body.host || !body.port) { proxyDlgMsg("请先填写主机和端口", true); return; }
  btnBusy($("proxy-test"), "探测中…");
  try {
    const res = await api<ProxyTest>("/proxies/test", {
      method: "POST", body: JSON.stringify({ id: editProxy ? editProxy.id : "", ...body }),
    });
    if (res.ok) {
      if (editProxy) { probes.set(editProxy.id, res); renderProxies(); }
      proxyDlgMsg(`连通 · ${fmtLatency(res.latency_ms)} · 出口 IP ${res.exit_ip}`);
    } else {
      proxyDlgMsg("不通：" + res.error, true);
    }
  } catch (e) {
    proxyDlgMsg("探测失败：" + (e as Error).message, true);
  }
  btnDone($("proxy-test"));
});

$("proxy-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const body = proxyDlgBody();
  if (!body.host) { proxyDlgMsg("请填写主机", true); return; }
  if (!(body.port >= 1 && body.port <= 65535)) { proxyDlgMsg("端口需在 1-65535 之间", true); return; }
  btnBusy($("proxy-ok"), "保存中…");
  try {
    if (editProxy) {
      await api("/proxies/" + editProxy.id, { method: "PATCH", body: JSON.stringify(body) });
    } else {
      await api("/proxies", { method: "POST", body: JSON.stringify(body) });
    }
    $<HTMLDialogElement>("dlg-proxy").close();
    toast(editProxy ? "代理已更新" : "代理已添加");
    await openProxiesSection();
    refreshAll();
  } catch (err) {
    proxyDlgMsg((err as Error).message, true);
  } finally {
    btnDone($("proxy-ok"));
  }
});

$("btn-proxy-add").addEventListener("click", () => openProxyDlg(null));
$("btn-proxy-reload").addEventListener("click", () => openProxiesSection());

/* ---------------- 批量导入 / 导出 ---------------- */

$("btn-proxy-import").addEventListener("click", () => {
  $<HTMLTextAreaElement>("proxy-import-text").value = "";
  $("proxy-import-msg").classList.add("hidden");
  $<HTMLDialogElement>("dlg-proxy-import").showModal();
});
$("proxy-import-cancel").addEventListener("click", () => $<HTMLDialogElement>("dlg-proxy-import").close());

$("proxy-import-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const text = $<HTMLTextAreaElement>("proxy-import-text").value;
  if (!text.trim()) return;
  btnBusy($("proxy-import-ok"), "导入中…");
  try {
    const res = await api<ProxyImport>("/proxies/import", {
      method: "POST", body: JSON.stringify({ text }),
    });
    const parts = [`已导入 ${res.added} 条`];
    if (res.duplicates) parts.push(`跳过重复 ${res.duplicates} 条`);
    if (res.errors.length) parts.push(`${res.errors.length} 行无法解析`);
    if (res.errors.length) {
      const msg = $("proxy-import-msg");
      msg.textContent = parts.join("，") + "：" + res.errors.join("；");
      msg.classList.remove("hidden");
    } else {
      $<HTMLDialogElement>("dlg-proxy-import").close();
    }
    toast(parts.join("，"), !res.added);
    await openProxiesSection();
  } catch (err) {
    const msg = $("proxy-import-msg");
    msg.textContent = "导入失败：" + (err as Error).message;
    msg.classList.remove("hidden");
  } finally {
    btnDone($("proxy-import-ok"));
  }
});

/* 导出走响应体而不是带 token 的下载直链：导出文本里有代理密码明文。 */
$("btn-proxy-export").addEventListener("click", async () => {
  btnBusy($("btn-proxy-export"), "导出中…");
  try {
    const res = await api<{ text: string }>("/proxies/export");
    const url = URL.createObjectURL(new Blob([res.text], { type: "text/plain" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = "agentbox-proxies-" + new Date().toISOString().slice(0, 10) + ".txt";
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
    toast("已导出 " + cache.length + " 条（含密码明文，注意保管）");
  } catch (e) {
    toast("导出失败：" + (e as Error).message, true);
  } finally {
    btnDone($("btn-proxy-export"));
  }
});

/* ---------------- 账号弹窗里的代理选择器 ---------------- */

/* 可搜索的下拉：代理多起来之后（一个账号池配几十个出口是常态）纯 <select>
 * 翻不动，也看不出哪个是通的。每行右边直接给一个探测按钮，选之前能先验。 */
export interface ProxyPicker {
  /** 设为某个代理 id（""=无代理），并刷新触发器文案 */
  set(id: string): void;
  /** 当前选中的 id，""=无代理 */
  get(): string;
}

export function mountProxyPicker(root: HTMLElement): ProxyPicker {
  let value = "";
  root.classList.add("proxy-pick");
  root.replaceChildren();

  const trigger = document.createElement("button");
  trigger.type = "button";
  trigger.className = "pp-trigger";
  trigger.appendChild(svgIcon("network", 15));
  trigger.setAttribute("aria-label", "选择出口代理");
  const label = document.createElement("span");
  label.className = "pp-label";
  const caret = document.createElement("span");
  caret.className = "pp-caret";
  caret.textContent = "▾";
  trigger.append(label, caret);

  const menu = document.createElement("div");
  menu.className = "pp-menu hidden";
  const searchWrap = document.createElement("div");
  searchWrap.className = "pp-search";
  const search = document.createElement("input");
  search.type = "search";
  search.placeholder = "搜索代理…";
  search.autocomplete = "off";
  searchWrap.appendChild(search);
  const list = document.createElement("div");
  list.className = "pp-list";
  menu.append(searchWrap, list);
  root.append(trigger, menu);

  const labelOf = (id: string) => {
    if (!id) return "无代理（直连服务器出口 IP）";
    const p = cache.find((x) => x.id === id);
    if (!p) return "已绑定但代理不存在（" + id + "）";
    return (p.name || p.host) + "（" + p.url + "）" + (p.disabled ? " · 已停用" : "");
  };

  const paint = () => {
    label.textContent = labelOf(value);
    label.classList.toggle("none", !value);
  };

  const close = () => {
    menu.classList.add("hidden");
    trigger.classList.remove("open");
  };

  const renderList = () => {
    const q = search.value.trim().toLowerCase();
    list.replaceChildren();
    list.appendChild(optionRow("", "无代理", "账号请求直接从服务器 IP 发出", null));
    for (const p of cache) {
      const hay = (p.name + " " + p.host + " " + p.port + " " + p.scheme).toLowerCase();
      if (q && !hay.includes(q)) continue;
      list.appendChild(optionRow(p.id, p.name || p.host, p.url + (p.disabled ? " · 已停用" : ""), p));
    }
  };

  const optionRow = (id: string, title: string, sub: string, p: Proxy | null) => {
    const row = document.createElement("div");
    row.className = "pp-opt" + (id === value ? " on" : "") + (p && p.disabled ? " off" : "");
    const box = document.createElement("div");
    box.className = "pp-opt-body";
    box.append(
      Object.assign(document.createElement("b"), { textContent: title }),
      Object.assign(document.createElement("span"), { textContent: sub }),
    );
    row.appendChild(box);
    if (p) {
      const probe = probes.get(p.id);
      if (probe) {
        const badge = document.createElement("span");
        badge.className = "pp-probe " + (probe.ok ? "good" : "bad");
        badge.textContent = probe.ok ? fmtLatency(probe.latency_ms) : "不通";
        if (probe.ok) setTip(badge, "出口 IP " + probe.exit_ip);
        row.appendChild(badge);
      }
      const test = document.createElement("button");
      test.type = "button";
      test.className = "pp-test";
      buttonLabel(test, "测试", "activity");
      setTip(test, "测试该代理连通性");
      test.addEventListener("click", (e) => {
        e.stopPropagation(); // 别顺手把这一行选中了
        runProbe(p, test).then(renderList);
      });
      row.appendChild(test);
    }
    if (id === value) {
      row.appendChild(Object.assign(document.createElement("span"), {
        className: "pp-check", textContent: "✓",
      }));
    }
    row.addEventListener("click", () => { value = id; paint(); close(); });
    return row;
  };

  trigger.addEventListener("click", () => {
    const opening = menu.classList.contains("hidden");
    menu.classList.toggle("hidden", !opening);
    trigger.classList.toggle("open", opening);
    if (opening) {
      search.value = "";
      renderList();
      search.focus();
      // 弹窗本身是可滚动容器，而菜单是绝对定位的：代理字段靠近弹窗底部时菜单
      // 会落在可视区之外。展开后把它滚进来，否则看起来像「点了没反应」。
      menu.scrollIntoView({ block: "nearest" });
    }
  });
  search.addEventListener("input", renderList);
  // 点弹窗里别处就收起来；用 mousedown 而不是 click，免得和选项的 click 打架
  document.addEventListener("mousedown", (e) => {
    if (!root.contains(e.target as Node)) close();
  });

  paint();
  return {
    set(id: string) { value = id || ""; paint(); close(); },
    get() { return value; },
  };
}
