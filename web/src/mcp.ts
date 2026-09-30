import { actionButton } from "./icons.js";
import { hideTip } from "./tip.js";
import { api } from "./api.js";
import { S, bus } from "./state.js";
import { $, askConfirm, toast } from "./util.js";
import { setSelectValue } from "./select.js";
import type { MCPCheck, MCPDefinition, MCPItem, MCPView } from "./types.js";

const KEEP = "__AGENTBOX_KEEP_SECRET__";
const MASK = "••••••";
const status: Record<string, string> = {
 configured: "已保存", pending: "待应用", pending_delete: "待清理（下次使用时移除）", applied: "已写入用户配置", conflict: "配置冲突",
 unmanaged: "终端自装", disabled: "未继承", connected: "连接成功", authentication_required: "需要授权",
 missing_command: "容器内未找到命令", network_error: "网络或 HTTP 错误", protocol_error: "协议或响应错误",
 timeout: "检测超时", cancelled: "检测已取消",
};
const sources = {user: "用户配置", session: "空间覆盖", native: "终端自装"};
function node<K extends keyof HTMLElementTagNameMap>(tag: K, text = "", cls = "") {
 const e = document.createElement(tag); e.textContent = text; e.className = cls; return e;
}
function secretJSON(values?: Record<string, string>) {
 return JSON.stringify(Object.fromEntries(Object.entries(values || {}).map(([k, v]) => [k, v === KEEP ? MASK : v])), null, 2);
}
function jsonMap(value: string): Record<string, string> {
 const obj: unknown = JSON.parse(value || "{}");
 if (!obj || typeof obj !== "object" || Array.isArray(obj) || Object.values(obj).some(v => typeof v !== "string")) throw new Error("凭证必须是 JSON 对象，键和值均为字符串");
 return Object.fromEntries(Object.entries(obj).map(([k, v]) => [k, v === MASK ? KEEP : v]));
}

export function initMCP() {
 const lifetime = new AbortController();
 const opts = {signal: lifetime.signal};
 let request: AbortController | null = null;
 let checkRequest: AbortController | null = null;
 let generation = 0;
 let loaded = "";
 let data: MCPView | null = null;
 let selected: MCPItem | null = null;
 let busy = false;
 const checks = new Map<string, MCPCheck>();
 const scope = () => $<HTMLSelectElement>("mcp-scope").value;
 const endpoint = () => scope() === "user" ? "/mcp" : `/sessions/${encodeURIComponent(S.current!.id)}/mcp`;
 const key = () => `${S.current?.id}/${scope()}`;
 const current = (g: number) => g === generation && !lifetime.signal.aborted && S.tab === "mcp";
 const editor = $<HTMLDialogElement>("mcp-editor");
 const clearEditor = () => {
  selected = null; $<HTMLFormElement>("mcp-form").reset();
  $("mcp-form-error").textContent = ""; $("mcp-form-error").classList.add("hidden");
 };
 const closeEditor = () => { if (editor.open) editor.close(); clearEditor(); };
 function button(text: string, icon: string, fn: () => Promise<void>, compact = false) {
  const b = node("button", "", "btn btn-sm"); b.type = "button";
  actionButton(b, compact ? "" : text, icon, text);
  if (icon === "trash") b.classList.add("btn-danger");
  b.addEventListener("click", () => { void action(fn); }); return b;
 }
 async function action(fn: () => Promise<void>) {
  if (busy) return; busy = true; $("mcp-content").setAttribute("aria-busy", "true");
  try { await fn(); } catch (e) {
   if (!lifetime.signal.aborted && !(e instanceof DOMException && e.name === "AbortError")) {
    if (editor.open) { $("mcp-form-error").textContent = (e as Error).message; $("mcp-form-error").classList.remove("hidden"); }
    else toast((e as Error).message, true);
   }
  } finally { busy = false; $("mcp-content").removeAttribute("aria-busy"); $<HTMLButtonElement>("mcp-save").disabled = false; }
 }
 async function send(path: string, method: string, body: unknown) {
  const result = await api(path, {method, headers: {"Content-Type":"application/json"}, body: JSON.stringify(body), signal: lifetime.signal});
  checks.clear(); return result;
 }
 async function load() {
  if (S.tab !== "mcp" || S.current?.agent !== "claude") return;
  const g = ++generation; loaded = key(); closeEditor(); data = null;
  request?.abort(); request = new AbortController();
  $("mcp-list").replaceChildren(node("p", "正在读取 MCP 配置…"));
  try {
   const result = await api<MCPView>(endpoint(), {signal: request.signal});
   if (!current(g)) return;
   data = result; render();
  } catch (e) { if (current(g)) $("mcp-list").replaceChildren(node("p", (e as Error).message, "mcp-error")); }
 }
 function showType() {
  const http = $<HTMLSelectElement>("mcp-type").value === "http";
  $("mcp-stdio-fields").classList.toggle("hidden", http); $("mcp-http-fields").classList.toggle("hidden", !http);
  $("mcp-secret-label").textContent = http ? "请求头" : "环境变量";
  $<HTMLInputElement>("mcp-url").required = http;
  $<HTMLInputElement>("mcp-command").required = !http;
 }
 function edit(item?: MCPItem) {
  if (!data) return; selected = item || null;
  const d = item?.config || {type:"stdio"};
  $("mcp-form-error").classList.add("hidden");
  $("mcp-editor-context").textContent = scope() === "user" ? "我的 MCP · 所有 Claude 空间默认继承" : "当前空间 · " + (S.current?.name || "");
  $("mcp-editor-title").textContent = item ? `编辑 ${item.name}` : "新增 MCP";
  $<HTMLInputElement>("mcp-name").value = item?.name || "";
  $<HTMLInputElement>("mcp-name").disabled = !!item;
  setSelectValue($<HTMLSelectElement>("mcp-type"), d.type || "stdio");
  $<HTMLInputElement>("mcp-command").value = d.command || "";
  $<HTMLTextAreaElement>("mcp-args").value = JSON.stringify(d.args || [], null, 2);
  $<HTMLInputElement>("mcp-url").value = d.url || "";
  $<HTMLTextAreaElement>("mcp-secrets").value = secretJSON(d.type === "http" ? d.headers : d.env);
  showType();
  hideTip();
  if (!editor.open) editor.showModal();
  $<HTMLInputElement>(item ? (d.type === "http" ? "mcp-url" : "mcp-command") : "mcp-name").focus();
 }
 function render() {
  const list = $("mcp-list"); list.replaceChildren();
  if (!data) return;
  $("mcp-scope-hint").textContent = scope() === "user" ? "自己的 Claude 空间默认继承这些配置；同名自装配置需要在各空间确认接管。" : "空间覆盖优先；删除覆盖可恢复继承。已写入配置不代表终端进程已重新加载。";
  if (data.project_names.length) list.append(node("p", `项目配置：${data.project_names.join("、")}。项目批准和插件 MCP 请在终端管理；同名配置可能影响最终加载结果。`, "mcp-notice"));
  if (data.external?.length) list.append(node("p", data.external.map(e => `${e.source === "local" ? "本地项目 MCP" : "已安装插件"}：${e.name}`).join("；") + "。这些来源由 CLI 管理，插件不一定提供 MCP，同名工具请在 CLI 核对。", "mcp-notice"));
  if (!data.items.length) list.append(node("p", "还没有 MCP。新增一个服务，或导入标准 mcpServers JSON 配置。", "mcp-empty"));
  for (const item of data.items) {
   const row = node("article", "", "mcp-row");
   const heading = node("div", "", "mcp-row-heading");
   const title = node("div", "", "mcp-row-title");
   title.append(node("strong", item.name), node("span", item.config.type || "继承", "mcp-transport"));
   const meta = node("div", "", "mcp-meta");
   const state = node("span", `${item.disabled ? "已停用 · " : ""}${status[item.status] || item.status}`, "mcp-state");
   state.dataset.state = item.disabled ? "disabled" : item.status;
   meta.append(node("span", sources[item.source]), state);
   heading.append(title, meta); row.append(heading);
   const actions = node("div", "", "mcp-actions");
   const base = endpoint(), id = S.current!.id, revision = data.revision, g = generation;
   const path = `${base}/${encodeURIComponent(item.name)}`;
   const refresh = async () => { if (current(g)) await load(); };
   if (item.status === "conflict" || item.source === "native") {
    const details = node("details"); details.append(node("summary", "查看配置差异（凭证已隐藏）"));
    details.append(node("pre", `网页配置\n${JSON.stringify(item.config, null, 2).replaceAll(KEEP, MASK)}\n终端配置\n${JSON.stringify(item.native, null, 2).replaceAll(KEEP, MASK)}`)); row.append(details);
    if (item.native && ["stdio","http"].includes(item.native.type)) actions.append(button("导入并接管终端配置", "download", async () => {
     if (!await askConfirm(`将 ${item.name} 的当前终端配置导入为空间覆盖，之后由网页管理。凭证会在服务端保留。`)) return;
     await send(`${path}/adopt`, "POST", {revision, native_revision:item.native_revision}); await refresh();
    }));
    if (item.status === "conflict") actions.append(button("保留自装并解除管理", "undo", async () => {
     if (!await askConfirm(`保留 ${item.name} 的终端配置，并在本空间禁用同名继承项？`)) return;
     await send(`${path}/adopt`, "POST", {revision, native_revision:item.native_revision, release:true}); await refresh();
    }));
   } else if(item.status !== "pending_delete") {
    const tools = node("div", "", "mcp-row-tools");
    actions.append(tools);
    tools.append(button(scope() === "session" && item.source === "user" ? "编辑空间覆盖" : "编辑", "edit", async () => edit(item), true));
    const toggle = button(item.disabled ? "启用" : "停用", "play", async () => {
     if (item.disabled && !item.config.command && !item.config.url) await send(path,"DELETE",{revision});
     else await send(path,"PUT",{revision,entry:{config:item.config,disabled:!item.disabled}});
     await refresh();
    });
    toggle.className = "mcp-toggle";
    toggle.setAttribute("role", "switch");
    toggle.setAttribute("aria-checked", String(!item.disabled));
    toggle.setAttribute("aria-label", `启用 ${item.name}`);
    toggle.replaceChildren(node("span", "", "mcp-toggle-track"), node("span", item.disabled ? "已停用" : "已启用"));
    actions.append(toggle);
    let remove: HTMLButtonElement | undefined;
    if (scope() === "user" || item.source === "session") remove = button(scope() === "session" ? "删除覆盖 / 恢复继承" : "删除", "trash", async () => {
     if (!await askConfirm(`删除 ${item.name} 的${scope() === "user" ? "用户配置" : "空间覆盖"}？`, {danger:true})) return;
     await send(path,"DELETE",{revision}); await refresh();
    }, true);
    if (scope() === "session" && !item.disabled) {
     tools.append(button("复制到我的 MCP", "copy", async () => {
      const user = await api<MCPView>("/mcp",{signal:lifetime.signal});
      await send(`${path}/copy`,"POST",{revision:user.revision}); toast("已复制到我的 MCP");
     }, true));
     const test = button("测试连接", "activity", async () => {
      toast("正在容器内检测 MCP，最多等待 30 秒");
      checkRequest?.abort(); checkRequest = new AbortController();
      const result = await api<MCPCheck>(`/sessions/${encodeURIComponent(id)}/mcp/${encodeURIComponent(item.name)}/check`,{method:"POST",signal:checkRequest.signal});
      checks.set(`${id}/${item.name}`,result); if (current(g)) render();
     });
     test.classList.add("mcp-test"); actions.prepend(test);
    }
    if (remove) tools.append(remove);
   }
   row.append(actions);
   const check = checks.get(`${id}/${item.name}`);
   if (check) {
    row.append(node("p", `上次检测：${status[check.status] || "检测失败"} · ${new Date(check.checked_at).toLocaleString()}`, check.status === "connected" ? "mcp-meta" : "mcp-error"));
    if (check.status === "authentication_required") row.append(node("p", `如服务使用 OAuth，请在终端运行 claude mcp login ${item.name}。本页检测暂不复用 CLI 的 OAuth 凭证。`));
    if (check.tools?.length) {
     const tools = node("details"); tools.append(node("summary", `${check.tools.length} 个工具${check.truncated ? "（已截断）" : ""}`));
     for (const tool of check.tools) tools.append(node("p", `${tool.name} — ${tool.description}`)); row.append(tools);
    }
   }
   list.append(row);
  }
 }
 $("mcp-add").addEventListener("click", () => edit(), opts);
 $("mcp-refresh").addEventListener("click", () => { void load(); }, opts);
 $("mcp-cancel").addEventListener("click", closeEditor, opts);
 $("mcp-close").addEventListener("click", closeEditor, opts);
 editor.addEventListener("close", () => { if (!editor.open) clearEditor(); }, opts);
 $("mcp-scope").addEventListener("change", () => { void load(); }, opts);
 $("mcp-type").addEventListener("change", showType, opts);
 $("mcp-form").addEventListener("submit", e => {
  e.preventDefault(); if (!data || busy) return;
  $("mcp-form-error").classList.add("hidden");
  $<HTMLButtonElement>("mcp-save").disabled = true;
  const base = endpoint(), revision = data.revision, g = generation;
  void action(async () => {
   const type = $<HTMLSelectElement>("mcp-type").value as "stdio" | "http";
   const name = $<HTMLInputElement>("mcp-name").value.trim();
   const secrets = jsonMap($<HTMLTextAreaElement>("mcp-secrets").value);
   let config: MCPDefinition;
   if (type === "http") config = {type,url:$<HTMLInputElement>("mcp-url").value.trim(),headers:secrets};
   else {
    const args: unknown = JSON.parse($<HTMLTextAreaElement>("mcp-args").value || "[]");
    if (!Array.isArray(args) || args.some(a => typeof a !== "string")) throw new Error("参数必须是字符串 JSON 数组");
    config = {type,command:$<HTMLInputElement>("mcp-command").value.trim(),args,env:secrets};
   }
   await send(`${base}/${encodeURIComponent(name)}`,"PUT",{revision,entry:{config,disabled:selected?.disabled || false}});
   toast("已保存，下次聊天或终端连接时应用；已运行的 Claude 需要重启");
   if (current(g)) await load();
  });
 }, opts);
 $("mcp-import").addEventListener("click", () => $<HTMLInputElement>("mcp-import-file").click(), opts);
 $("mcp-import-file").addEventListener("change", () => {
  const input = $<HTMLInputElement>("mcp-import-file"), file = input.files?.[0]; input.value = "";
  if (!file || !data) return;
  const base = endpoint(), revision = data.revision, g = generation;
  void action(async () => {
   if (file.size > 100*1024) throw new Error("导入文件不能超过 100 KB");
   const parsed = JSON.parse(await file.text()) as {mcpServers?: Record<string,MCPDefinition>};
   if (!parsed.mcpServers || typeof parsed.mcpServers !== "object" || Array.isArray(parsed.mcpServers)) throw new Error("文件需要包含 mcpServers 对象");
   const names = Object.keys(parsed.mcpServers);
   if (!await askConfirm(`将导入 ${names.length} 个 MCP：\n${names.join("、")}\n已有同名配置不会覆盖。凭证随配置保存。`,{title:"预览导入"})) return;
   await send(`${base}/import`,"POST",{revision,mcpServers:parsed.mcpServers}); if (current(g)) await load();
  });
 }, opts);
 bus.addEventListener("navigation-changed", () => {
  if (S.tab !== "mcp" || S.view !== "work") { generation++; loaded=""; request?.abort();checkRequest?.abort();closeEditor();return; }
  if (loaded !== key()) void load();
 }, opts);
 return () => { lifetime.abort(); request?.abort(); checkRequest?.abort(); generation++; checks.clear(); closeEditor(); $("mcp-list").replaceChildren(); };
}
