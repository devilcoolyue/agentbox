import { t as i18nText, setText, setTextRender, setAttrRender } from "./i18n.js";
import { svgIcon, type IconSlot } from "./icons.js";
import { BUDGETS, EFFORT_LABELS } from "./reasoning.js";
import { cacheHitText, fmtSpeed, fmtTokens, outputSpeed } from "./usage-math.js";
/* usage：使用记录（消耗流水明细）。
 *
 * **一行是什么**：一次 provider 请求（一条 API 响应）。容器里的 CLI 一个回合内部
 * 每调一次工具就多打一次接口、每次重发整段上下文，所以同回合会连着好几行，共享
 * turn_id，类型下方标「第几次 / 共几次」。升级前的 Claude 对话按回合合并保存，
 * 那些旧行标「合并 N 次」。
 *
 * 因此「回合数」永远按 turn_id 去重；首字与总耗时是回合级指标，只记在回合第一次
 * 请求上，其余行显示 —，合计里也没有这两项。
 *
 * 金额一律以微美元整数在前后端之间传递，只在显示的最后一步除 1e6（同 quota.ts）。
 * 管理员看全部并多一列「用户」，普通用户只看得到自己的，服务端按 scope 下发。 */
"use strict";

import { setSelectValue } from "./select.js";

import { S, bus } from "./state.js";
import type { UsageEventRow, UsageEvents } from "./types.js";
import { $, toast, startDownload, fmtTime, keepScroll } from "./util.js";
import { DateRangePicker } from "./date-range.js";
import { api } from "./api.js";
import { showView } from "./shell.js";
import { fmtUSD } from "./quota.js";
import { agentIcon, agentName } from "./brand.js";
import { setTip } from "./tip.js";
import { ListMotion } from "./motion.js";
import { watchUsageLive } from "./usage-live.js";

const usageAgentName=(agent:string)=>agent==="decision"?i18nText("决策插件"):agentName(agent);
const usageAgentIcon=(agent:string,size:number)=>agent==="decision"?svgIcon("sliders",size):agentIcon(agent,size);

let pageSize = 20;

/* 当前视图状态。offset 单独放：改筛选要归零，翻页只动它。 */
let offset = 0;
let last: UsageEvents | null = null;
let activeCost: UsageEventRow | null = null;
let usageGeneration = 0;
let loading = false;
let loadPending = false;
let resetScroll = false;
let usageTimeZone = S.timeZone || "Asia/Shanghai";

/* 排序方向。只排时间一列：这张表是流水，除了「先看哪头」没别的排法有意义。
 * 默认倒序（最新在最前）——看消耗十有八九是想知道刚才烧了多少；正序留给
 * 从头核账的场合。服务端的默认值也是倒序，两边别各说各的。 */
let asc = false;
const orderParam = () => (asc ? "asc" : "desc");

/* ---------------- 筛选条件 ---------------- */

interface Filters {
  user: string;
  agent: string;
  model: string;
  kind: string;
  since: string; // 系统时区下的墙上时间 YYYY-MM-DDTHH:MM
  until: string;
}

function readFilters(): Filters {
  const range = dateRange.value();
  return {
    user: $<HTMLSelectElement>("uf-user").value,
    agent: $<HTMLSelectElement>("uf-agent").value,
    model: $<HTMLSelectElement>("uf-model").value,
    kind: $<HTMLSelectElement>("uf-kind").value,
    since: range.since,
    until: range.until,
  };
}

/* ---------------- 时间区间 ---------------- */

const pad = (n: number) => String(n).padStart(2, "0");
const dateRange = new DateRangePicker($<HTMLButtonElement>("uf-date-range"), () => usageTimeZone, reload, "today");

interface ClockParts { year: number; month: number; day: number; hour: number; minute: number; second: number }

const zoneFormatters = new Map<string, Intl.DateTimeFormat>();

/* Intl 负责把一个绝对时刻投影到系统配置的 IANA 时区。formatToParts 避免依赖
 * 不同浏览器的日期字符串顺序；h23 保证午夜是 00:xx 而不是 24:xx。 */
function clockParts(d: Date): ClockParts {
  let fmt = zoneFormatters.get(usageTimeZone);
  if (!fmt) {
    fmt = new Intl.DateTimeFormat("en-CA", {
      timeZone: usageTimeZone,
      year: "numeric", month: "2-digit", day: "2-digit",
      hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23",
    });
    zoneFormatters.set(usageTimeZone, fmt);
  }
  const values: Record<string, number> = {};
  for (const p of fmt.formatToParts(d)) {
    if (p.type !== "literal") values[p.type] = Number(p.value);
  }
  return {
    year: values.year!, month: values.month!, day: values.day!,
    hour: values.hour!, minute: values.minute!, second: values.second!,
  };
}

function fmtUsageTime(ms: number | string, withYear = false) {
  const p = clockParts(new Date(ms));
  const core = `${pad(p.month)}-${pad(p.day)} ${pad(p.hour)}:${pad(p.minute)}`;
  return withYear ? `${p.year}-${core}` : core;
}

/* 一次请求一行，同回合的请求相隔几秒，时间列要精确到秒。 */
function fmtUsageClock(ms: number) {
  const p = clockParts(new Date(ms));
  return `${pad(p.hour)}:${pad(p.minute)}:${pad(p.second)}`;
}

const ZONE_NAMES: Record<string, string> = {
  get "Asia/Shanghai"() { return i18nText("中国标准时间"); },
  get "Asia/Hong_Kong"() { return i18nText("香港时间"); },
  get "Asia/Tokyo"() { return i18nText("日本时间"); },
  get UTC() { return i18nText("协调世界时"); },
  get "America/Los_Angeles"() { return i18nText("美国太平洋时间"); },
  get "America/New_York"() { return i18nText("美国东部时间"); },
  get "Europe/London"() { return i18nText("英国时间"); },
};

function syncTimeZone() {
  setTextRender($("usage-zone-name"), () => ZONE_NAMES[usageTimeZone] || i18nText("系统时区"));
  $("usage-zone-id").textContent = usageTimeZone;
  dateRange.refresh();
}

/* 时间框给的是系统时区下的墙上时间，不在浏览器里转 ISO：服务端会用配置的 IANA
 * 时区解析。until 往后推一分钟，因为服务端上界是开区间；这里用 UTC getter 只做
 * 日历加法，不把输入误当浏览器本地时区。 */
function query(f: Filters, extra: Record<string, string> = {}) {
  const q = new URLSearchParams();
  const put = (k: string, v: string) => { if (v) q.set(k, v); };
  put("user", f.user);
  put("agent", f.agent);
  put("model", f.model);
  put("kind", f.kind);
  put("since", f.since);
  put("until", addWallMinutes(f.until, 1));
  for (const [k, v] of Object.entries(extra)) q.set(k, v);
  return q.toString();
}

function addWallMinutes(v: string, minutes: number) {
  if (!v) return "";
  const d = new Date(v + ":00Z");
  if (isNaN(d.getTime())) return "";
  d.setUTCMinutes(d.getUTCMinutes() + minutes);
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}` +
    `T${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
}

/* 下拉选项：服务端每次都把可选值算好带回来（且按可见范围裁过），这里只负责
 * 重建 <option> 并尽量保住当前选中项——选中的值已经不在可选集里时回到「全部」，
 * 否则筛选框显示着一个查不出任何东西的值。 */
function fillSelect(id: string, values: string[], label?: (v: string) => string) {
  const sel = $<HTMLSelectElement>(id);
  const cur = sel.value;
  sel.replaceChildren();
  const all = document.createElement("option");
  all.value = "";
  setText(all, "全部");
  sel.appendChild(all);
  for (const v of values) {
    const o = document.createElement("option");
    o.value = v;
    if (label) setTextRender(o, () => label(v));
    else o.textContent = v;
    sel.appendChild(o);
  }
  setSelectValue(sel, values.includes(cur) ? cur : "");
}

/* ---------------- 筛选条折叠 ---------------- */

/* 桌面首次展开；窄屏先显示条件摘要，为明细留空间。用户主动切换后记住偏好。 */
const COLLAPSE_KEY = "agentbox_usage_filters_v2";

function setFiltersOpen(open: boolean, remember = true) {
  $("usage-filters").classList.toggle("collapsed", !open);
  $("uf-toggle").setAttribute("aria-expanded", String(open));
  // 隐私模式下 localStorage 可能不可写：本次会话仍生效，只是记不住
  if (remember) try { localStorage.setItem(COLLAPSE_KEY, open ? "open" : "closed"); } catch (_) {}
}

function initFiltersOpen() {
  let saved: string | null = null;
  try { saved = localStorage.getItem(COLLAPSE_KEY); } catch (_) {}
  setFiltersOpen(saved ? saved === "open" : !window.matchMedia("(max-width: 760px)").matches, false);
}

/* 收起后光看一个「筛选」字样，认不出当前到底筛没筛——把生效的条件摘要挂在标题行上。 */
function renderFilterChip(f: Filters) {
  // Keep the applied filter values, but resolve authored labels at display time.
  // dateRange.label() reads applied values and never changes its open draft.
  setTextRender($("uf-active"), () => {
    const parts: string[] = [];
    if (f.user) parts.push(f.user);
    if (f.agent) parts.push(usageAgentName(f.agent));
    if (f.model) parts.push(f.model);
    if (f.kind) parts.push(KIND_LABEL[f.kind] || f.kind);
    if (f.since || f.until) parts.push(dateRange.label());
    return parts.length ? parts.join(" · ") : i18nText("全部记录");
  });
}

/* ---------------- 排序 ---------------- */

/* 表头「时间」旁边是上下两个三角，不是一个来回切的按钮：点哪个就是哪个方向，
 * 当前方向一直亮着。来回切的按钮得先猜「现在是哪种、点下去会变成哪种」。 */

function syncSort() {
  for (const [id, on] of [["usage-sort-asc", asc], ["usage-sort-desc", !asc]] as [string, boolean][]) {
    $(id).classList.toggle("on", on);
    $(id).setAttribute("aria-pressed", String(on));
  }
}

function setSort(next: boolean) {
  if (next === asc) return; // 点已经亮着的那个箭头不该白跑一趟请求
  asc = next;
  syncSort();
  // 换方向必须回第一页：offset 是按当前顺序数出来的，翻到第 3 页再倒过来，
  // 落到的是「从另一头数第 101 行」，看着像随机跳走了。
  reload();
}

/* ---------------- 渲染 ---------------- */

/** 思考强度的显示：档位用英文常量，思考预算附上 token 数（与对话页一致）。 */
function effortText(r: UsageEventRow): string {
  if (!r.effort) return "";
  const label = EFFORT_LABELS[r.effort] || r.effort;
  return r.effort_control === "budget" && BUDGETS[r.effort] ? `${label} (${BUDGETS[r.effort]})` : label;
}

const KIND_LABEL: Record<string, string> = { get chat() { return i18nText("对话"); }, get delegation() { return i18nText("委派跑腿"); }, get terminal() { return i18nText("终端"); }, get title() { return i18nText("起标题"); },get decision(){return i18nText("决策调用");},get suggestion(){return i18nText("下一步提示");}, get background() { return i18nText("后台请求"); }, get keepalive() { return i18nText("缓存保活"); } };
const KIND_HINT: Record<string, string> = {
  get delegation() { return i18nText("主 Agent 把子任务交给跑腿模型时，跑腿模型花的量；与主 Agent 的回合分开记账"); },
  get decision(){return i18nText("委派时由 Jev 判断交给哪个跑腿的那次调用");},
  get terminal() { return i18nText("用户在「终端」页签里手敲 CLI 花的量，事后从 CLI 自己的记录里补记；只记账不扣额度"); },
  get title() { return i18nText("服务端自动为新对话生成标题的那趟消耗，不是用户发起的"); },
  get suggestion(){return i18nText("对话回合结束后预测你下一句输入的那次调用；可在「系统设置 → 界面与提示」里关闭");},
  get background() { return i18nText("Codex 自己在后台发的请求：每次启动先发的预热、终端里为会话另开的小请求。对话和终端里都看不到，按 CLI 遥测补记；只记账不扣额度"); },
  get keepalive() { return i18nText("你离开时，服务端为保住对话的提示缓存发的请求：只读一遍缓存、不进对话记录，比回来后整段重写缓存便宜得多"); },
};
/* 类型列写名称，前面一枚图标帮着扫读「这笔钱是谁按下去的」；不套框，免得和计费
 * 标签抢眼。悬停给名称与说明。墨色深浅沿用原来的分档：对话是主体最重，终端次之，
 * 服务端自发的调用最轻——不加色相，旁边的 Token 与计费列已经用了好几种颜色。 */
const KIND_ICON: Record<string, IconSlot> = { chat: "message", delegation: "users", terminal: "tool", title: "type", suggestion: "bulb", decision: "network", background: "cpu", keepalive: "heart-pulse" };
const KIND_TONE: Record<string, string> = { chat: "chat", delegation: "chat", suggestion: "chat", terminal: "term", title: "title", decision: "title", background: "title", keepalive: "title" };

function kindBadge(kind: string) {
  const badge = document.createElement("span");
  badge.className = "u-kind" + (KIND_TONE[kind] ? " " + KIND_TONE[kind] : "");
  badge.dataset.kind = kind;
  const label = document.createElement("span");
  label.className = "u-kind-label";
  setTextRender(label, () => KIND_LABEL[kind] || kind);
  badge.append(svgIcon(KIND_ICON[kind] || "activity", 14), label);
  setTip(badge, () => (KIND_LABEL[kind] || kind) + (KIND_HINT[kind] ? "\n" + KIND_HINT[kind] : ""));
  return badge;
}

const workspaceName = (r: UsageEventRow) => r.session_name || r.session_id;
const BILLING: Record<string, { text: string; title: string }> = {
  // 与对话页每轮下方的说法一致：写用户看得懂的来源，不写「价目表」「CLI」
  provider: { get text() { return i18nText("客户端上报"); }, get title() { return i18nText("Claude Code 等客户端自己上报的估算成本，不代表订阅账号的实际扣款"); } },
  table: { get text() { return i18nText("按单价估算"); }, get title() { return i18nText("客户端没有上报金额，按系统设置里的模型单价折算"); } },
  none: { get text() { return i18nText("未定价"); }, get title() { return i18nText("客户端没有上报金额，这个模型也没有设置单价；这一行只记用量、不扣额度"); } },
};
const billingFor=(r:UsageEventRow)=>r.kind==="decision"?{get text(){return i18nText("决策计费");},get title(){return i18nText("按决策调用的用量及计费依据入账，不使用聊天模型价目表。");}}:BILLING[r.billing]||{text:r.billing,title:""};

/* 大数字加千分位；token 列四个桶都可能上十万，不分位读不出量级。 */
const num = (n: number) => n.toLocaleString("en-US");

/* 耗时：秒以下给毫秒，其余给一位小数的秒。0 表示没量到，显示破折号而不是「0s」
 * ——0 秒和「这条没有数据」是两件事。 */
function fmtDur(ms: number) {
  if (!ms) return "—";
  if (ms < 1000) return ms + " ms";
  return (ms / 1000).toFixed(1) + " s";
}

/* 每个格子都带上表头名（data-l）并把内容裹进 .u-v：窄屏下表格会拆成一行一张卡片，
 * 标签由 data-l 生成在左边，.u-v 负责把整格内容（可能有两行）作为一块推到右边。
 * 没有这层包裹，格子一变 flex，两行内容就会被拆成并排的两列。 */
function cellEl(label: string | (() => string), cls = "") {
  const td = document.createElement("td");
  if (cls) td.className = cls;
  if (typeof label === "function") setAttrRender(td, "data-l", label);
  else td.dataset.l = label;
  const v = document.createElement("div");
  v.className = "u-v";
  td.appendChild(v);
  return { td, v };
}

function cell(label: string | (() => string), text: string, cls = "") {
  const { td, v } = cellEl(label, cls);
  v.textContent = text;
  return td;
}

/* Token 格照 sub2api 的排法：第一行 ↓ 输入 ↑ 输出（完整数字），第二行缓存读取
 * （数据库图标）与缓存写入（铅笔图标）按 K/M/B 缩写，右边 ⓘ 悬停看完整数字。
 * 箭头只是图形记号，读屏与悬停提示里仍写明「输入 / 输出」。 */
function tokenPart(icon: IconSlot, cls: string, text: string, label: () => string) {
  const part = document.createElement("span");
  part.className = "u-tk " + cls;
  const value = document.createElement("span");
  value.textContent = text;
  part.append(svgIcon(icon, 13), value);
  setAttrRender(part, "aria-label", () => label() + " " + text);
  return part;
}

function tokenCell(r: UsageEventRow) {
  const { td, v } = cellEl("Token", "num u-tok");
  const lines = document.createElement("div");
  lines.className = "u-tk-lines";
  const io = document.createElement("div");
  io.className = "u-tk-line";
  io.append(tokenPart("arrow-down", "in", num(r.input_tokens), () => i18nText("输入")),
    tokenPart("arrow-up", "out", num(r.output_tokens), () => i18nText("输出")));
  lines.appendChild(io);
  if (r.cache_read_tokens || r.cache_write_tokens) {
    const cache = document.createElement("div");
    cache.className = "u-tk-line";
    if (r.cache_read_tokens) cache.appendChild(tokenPart("archive", "read", fmtTokens(r.cache_read_tokens), () => i18nText("缓存读取")));
    if (r.cache_write_tokens) cache.appendChild(tokenPart("edit", "write", fmtTokens(r.cache_write_tokens), () => i18nText("缓存写入")));
    lines.appendChild(cache);
  }
  const info = document.createElement("span");
  info.className = "u-tk-info";
  info.tabIndex = 0;
  info.appendChild(svgIcon("info", 13));
  setAttrRender(info, "aria-label", () => i18nText("Token 明细"));
  setTip(info, () => i18nText("输入 {p0}\n输出 {p1}\n", { p0: num(r.input_tokens), p1: num(r.output_tokens) }) +
    i18nText("缓存读取 {p0}\n缓存写入 {p1}\n", { p0: num(r.cache_read_tokens), p1: num(r.cache_write_tokens) }) +
    i18nText("合计 {p0}", { p0: num(r.total_tokens) }));
  v.append(lines, info);
  return td;
}

/* 明细行进场：换筛选、翻页后整页错落出现；「跟随当前时刻」轮询时只有新到的行进场 */
const rowMotion = new ListMotion("enter", "list-enter");

/* 骨架行：首次读取、换筛选、翻页时先铺几行占位，列数对上表头里显示着的列。
 * 骨架整块晚 150ms 才淡入（base.css），读得快时根本看不到。 */
function skeletonRows() {
  const cols = [...document.querySelectorAll<HTMLElement>(".usage-table thead th")]
    .filter((th) => !th.classList.contains("hidden")).length;
  const rows = Array.from({ length: 6 }, (_, i) => {
    const tr = document.createElement("tr");
    tr.className = "skeleton-row";
    tr.setAttribute("aria-hidden", "true");
    for (let c = 0; c < cols; c++) {
      const td = document.createElement("td");
      const bar = document.createElement("span");
      bar.className = "skeleton";
      bar.style.width = `${45 + ((i * 7 + c * 13) % 40)}%`; // 长短错开，像真的数据
      td.append(bar);
      tr.append(td);
    }
    return tr;
  });
  $("usage-rows").replaceChildren(...rows);
  $("usage-empty").classList.add("hidden");
}

function renderRows(data: UsageEvents) {
  const body = $("usage-rows");
  body.replaceChildren();
  const self = data.scope === "self";
  $("uf-user-wrap").classList.toggle("hidden", self);
  for (const th of document.querySelectorAll<HTMLElement>(".usage-table .col-user")) {
    th.classList.toggle("hidden", self);
  }

  for (const r of data.rows) {
    const tr = document.createElement("tr");

    const u = cell(() => i18nText("用户"), r.user, "col-user");
    u.classList.toggle("hidden", self);
    tr.appendChild(u);

    // 会话名 + 账号。会话被删掉后名字为空，退回显示 id：这行消耗真实发生过，
    // 不能因为会话没了就不显示。
    const { td: sess, v: sessv } = cellEl(() => i18nText("工作空间"), "u-cell-sess");
    const name = document.createElement("div");
    name.className = "u-sess";
    name.textContent = workspaceName(r);
    name.title = name.textContent;
    if (!r.session_name) setTip(name, () => i18nText("工作空间已删除"));
    const acct = document.createElement("div");
    acct.className = "u-sub";
    acct.textContent = r.account_label || r.account_id || "—";
    sessv.append(name, acct);
    tr.appendChild(sess);

    const { td: model, v: modelv } = cellEl(() => i18nText("模型 / 推理强度"));
    const mline = document.createElement("div");
    mline.className = "u-model";
    const modelName = document.createElement("span");
    setTextRender(modelName, () => r.model || usageAgentName(r.agent) + i18nText("（默认模型）"));
    setAttrRender(modelName, "title", () => r.model || usageAgentName(r.agent) + i18nText("（默认模型）"));
    mline.append(usageAgentIcon(r.agent, 15), modelName);
    modelv.appendChild(mline);
    if (r.provider || r.effort) {
      const p = document.createElement("div");
      p.className = "u-sub";
      if (r.provider) {
        const via = document.createElement("span");
        setTextRender(via, () => r.provider === "firstParty" ? i18nText("官方直连") : (r.provider || ""));
        p.appendChild(via);
      }
      if (r.effort) {
        const effort = document.createElement("span");
        effort.className = "u-effort";
        effort.append(svgIcon("sliders", 12), document.createTextNode(effortText(r)));
        setTip(effort, () => i18nText("这一回合发送时选择的思考强度"));
        p.appendChild(effort);
      }
      modelv.appendChild(p);
    }
    tr.appendChild(model);

    const { td: kind, v: kindv } = cellEl(() => i18nText("类型"), "u-cell-kind");
    kindv.appendChild(kindBadge(r.kind));
    // 同回合连着好几行：标出这是第几次请求；升级前合并保存的旧行标合并次数。
    if (r.turn_requests || r.merged_requests) {
      const seq = document.createElement("div");
      seq.className = "u-sub u-seq";
      if (r.turn_requests) {
        setTextRender(seq, () => i18nText("第 {p0}/{p1} 次", { p0: num(r.turn_seq || 0), p1: num(r.turn_requests || 0) }));
        setTip(seq, () => i18nText("这一回合里的第几次模型请求：CLI 每调用一次工具就再请求一次，每次都重新发送整段上下文"));
      } else {
        setTextRender(seq, () => i18nText("合并 {p0} 次", { p0: num(r.merged_requests || 0) }));
        setTip(seq, () => i18nText("升级前的记录按回合合并保存，这一行是 {p0} 次请求之和；已按整行入账，不再拆开", { p0: num(r.merged_requests || 0) }));
      }
      kindv.appendChild(seq);
    }
    tr.appendChild(kind);

    const b = billingFor(r);
    const bchip = document.createElement("span");
    bchip.className = "u-chip bill-" + r.billing;
    setTextRender(bchip, () => b.text);
    setTip(bchip, () => b.title);

    tr.appendChild(tokenCell(r));

    const hit = cell(() => i18nText("缓存命中率"), cacheHitText(r), "num u-hit");
    setTip(hit, () => i18nText("缓存读取 ÷（未缓存输入 + 缓存读取 + 缓存写入）；不含输出 token，无输入时显示 —"));
    tr.appendChild(hit);

    // 金额后面挂一个「?」：点开是这一行的分项算式（输入/输出/缓存各花了多少）。
    // 金额本身看不出为什么是这个数，尤其是缓存读取常常比输入贵不了几分钱、
    // 却占了大头。
    const { td: cost, v: costv } = cellEl(() => i18nText("费用"), "num u-cost");
    if (!r.cost_micro_usd) cost.classList.add("zero");
    const why = document.createElement("button");
    why.type = "button";
    why.className = "u-why";
    why.append(svgIcon("help", 14));
    setTip(why, () => i18nText("这笔钱是怎么算出来的"));
    setAttrRender(why, "aria-label", () => i18nText("费用明细"));
    why.addEventListener("click", () => openCost(r));
    costv.append(document.createTextNode(fmtUSD(r.cost_micro_usd)), why);
    const billing = document.createElement("div");
    billing.className = "u-billing";
    billing.appendChild(bchip);
    costv.appendChild(billing);
    tr.appendChild(cost);

    // 有 CLI 实测计时的行（任何种类）显示这一次请求的首字与耗时，见 requestLatency；
    // 没有的终端行显示日志估算，见 terminalLatency。
    //
    // 其余网页对话行：首字 + 总耗时，两个数同源（都走我们自己的表：容器就绪 → 首个输出 /
    // 进程退出），所以首字必然 ≤ 总耗时。provider 自报的模型侧耗时不上表——它不含 CLI 启动
    // 那两秒，跟首字不是一个口径，摆在一起会出现「首字 3.1s / 耗时 2.3s」这种看着不可能的行。
    // 两个数都是回合级的，只记在回合第一次请求上，悬停说明里点明，免得有人把一列加起来当总时长。
    const { td: lat, v: latv } = cellEl(() => i18nText("延迟"), "num u-lat");
    if (r.api_duration_ms) {
      requestLatency(r, lat, latv);
    } else if (r.kind === "background") {
      latv.textContent = "—";
      setTip(lat, () => i18nText("遥测里没有这次请求的发出时刻，算不出耗时"));
    } else if (r.kind === "terminal") {
      terminalLatency(r, lat, latv);
    } else if (!r.ttft_ms && !r.wall_ms && !r.duration_ms) {
      // 回合里后续的请求：耗时只在回合第一次请求上量一次。
      latv.textContent = "—";
      setTip(lat, () => i18nText("首字与总耗时按回合测量，记在这一回合的第一次请求上"));
    } else {
      const ttft = document.createElement("div");
      setTextRender(ttft, () => i18nText("首字 ") + fmtDur(r.ttft_ms));
      const dur = document.createElement("div");
      dur.className = "u-sub";
      // 老数据没量过墙钟，那种行退回 provider 报的模型侧耗时，并把标签换掉——
      // 宁可标明这是另一个口径，也不要把它冒充成总耗时。
      setTextRender(dur, () => r.wall_ms ? i18nText("总耗时 ") + fmtDur(r.wall_ms) : i18nText("模型 ") + fmtDur(r.duration_ms));
      latv.append(ttft, dur);
      setTip(lat, () => i18nText("首字与总耗时都从容器就绪开始算，含 CLI 启动，两个数同源。")
        + i18nText("老数据没量过总耗时，退回显示 provider 自报的模型侧耗时（标「模型」，不含启动）。")
        + i18nText("都是回合级指标：只记在回合的第一次请求上，总耗时是整个回合的耗时"));
    }
    tr.appendChild(lat);
    tr.appendChild(speedCell(r));

    const { td: time, v: timev } = cellEl(() => i18nText("时间"), "num u-time");
    // 同回合的请求相隔几秒，主行精确到秒。
    const stamp = fmtUsageTime(r.ts, true);
    const day = document.createElement("div");
    day.className = "u-sub";
    day.textContent = stamp.slice(0, 10).replaceAll("-", "/");
    timev.append(document.createTextNode(fmtUsageClock(r.ts)), day);
    tr.appendChild(time);
    body.appendChild(tr);
    rowMotion.play(tr, String(r.id));
  }

  $("usage-empty").classList.toggle("hidden", data.rows.length > 0);
}

/* CLI 遥测报的这一次请求：Claude Code 的 api_request 按 request id 对上这一行，首字 =
 * 发出请求 → 流的第一个事件，耗时 = 发出请求 → 流结束；Codex 按会话与 token 数对上，首字 =
 * 发出请求 → 第一个输出项，耗时 = 发出请求 → 完成事件。终端行与网页对话行都按它显示、
 * 不带「≈」；网页对话原有的回合级首字与总耗时挪进悬停说明。 */
function requestLatency(r: UsageEventRow, lat: HTMLElement, latv: HTMLElement) {
  const lines: (() => string)[] = [];
  if (r.api_ttft_ms) lines.push(() => i18nText("首字 ") + fmtDur(r.api_ttft_ms || 0));
  lines.push(() => i18nText("耗时 ") + fmtDur(r.api_duration_ms || 0));
  lines.forEach((render, i) => {
    const line = document.createElement("div");
    if (i) line.className = "u-sub";
    setTextRender(line, render);
    latv.appendChild(line);
  });
  setTip(lat, () => {
    let tip = r.agent === "codex"
      ? i18nText("Codex 遥测报的这一次请求：首字是从发出请求到第一个输出项（含思考），耗时是从发出请求到收到完成事件。")
      : i18nText("Claude Code 自己量的这一次请求：首字是从发出请求到收到流的第一个事件，耗时是从发出请求到流结束；失败重试只算最后一次。");
    if (r.kind !== "terminal" && r.wall_ms) {
      tip += r.ttft_ms
        ? i18nText("这一回合从容器就绪算起（含 CLI 启动与工具执行）：首字 {p0}，总耗时 {p1}", { p0: fmtDur(r.ttft_ms), p1: fmtDur(r.wall_ms) })
        : i18nText("这一回合从容器就绪算起（含 CLI 启动与工具执行）总耗时 {p0}", { p0: fmtDur(r.wall_ms) });
    }
    return tip;
  });
}

/* 终端行的延迟：每行是一次请求，耗时与首字是服务端按 CLI 日志的时间戳估的
 * （usage.termTiming），都带「≈」，与对话行那两个我们自己掐表的回合级数分开说。 */
function terminalLatency(r: UsageEventRow, lat: HTMLElement, latv: HTMLElement) {
  if (!r.wall_ms) {
    latv.textContent = "—";
    setTip(lat, () => i18nText("终端日志推不出这次请求的耗时：回复还没写完，或日志里找不到它从哪儿开始"));
    return;
  }
  const lines: (() => string)[] = [];
  if (r.ttft_ms) lines.push(() => i18nText("首字 ≈{p0}", { p0: fmtDur(r.ttft_ms) }));
  lines.push(() => i18nText("耗时 ≈{p0}", { p0: fmtDur(r.wall_ms) }));
  lines.forEach((render, i) => {
    const line = document.createElement("div");
    if (i) line.className = "u-sub";
    setTextRender(line, render);
    latv.appendChild(line);
  });
  setTip(lat, () => i18nText("按 CLI 日志的时间戳估算这一次请求：从发出请求到回复写完（较旧的 Claude Code 不记发出时刻，取上一条消息或工具结果写下的时刻）。")
    + i18nText("首字只在日志记下了开始输出的时刻时才有：Claude 的回复以思考开头，或 Codex 记了思考/回复项的开始时刻。"));
}

/* 输出速度 = 输出 ÷（耗时 − 首字），口径见 usage-math.ts 的 outputSpeed。CLI 实测的
 * 不带「≈」，按终端日志估的带「≈」；算不出的显示「—」，悬停说明为什么。 */
function speedCell(r: UsageEventRow) {
  const { td, v } = cellEl(() => i18nText("输出速度"), "num u-speed");
  const speed = outputSpeed(r);
  if (speed.tps === null) {
    const why = speed.why;
    v.textContent = "—";
    setTip(td, () => why === "turn" ? i18nText("网页对话的耗时覆盖整个回合（含工具执行与 CLI 启动），算不出单次请求的输出速度")
      : why === "untimed" ? i18nText("这次请求缺首字或耗时，算不出输出速度")
      : i18nText("输出太少或生成用时太短：几十个 token 常常一下子到齐，算出来的速度说明不了快慢"));
    return td;
  }
  // 单位另起一行：这一列只有数字那么宽，表格在 1280 宽的屏上才放得下。
  const unit = document.createElement("div");
  unit.className = "u-sub";
  unit.textContent = "tok/s";
  const basis = speed.basis;
  v.append(document.createTextNode((basis === "measured" ? "" : "≈") + fmtSpeed(speed.tps)), unit);
  setTip(td, () => basis === "measured" ? (r.agent === "codex"
      ? i18nText("输出 token ÷（耗时 − 首字），首字与耗时来自 Codex 的遥测；输出含思考的 token")
      : i18nText("输出 token ÷（耗时 − 首字），首字与耗时是 Claude Code 自己量的；输出含思考的 token"))
    : basis === "log" ? i18nText("输出 token ÷（耗时 − 首字），首字与耗时按 CLI 日志的时间戳估算；输出含思考的 token")
    : i18nText("输出 token ÷ 耗时（含首字等待，偏低），耗时按 CLI 日志的时间戳估算"));
  return td;
}

/* ---------------- 费用明细 ---------------- */

/* 「这一行为什么是这个数」：四个 token 桶各自 × 单价摊开，末尾对上实收金额。
 *
 * 拆得出分项的只有按价目表折算的行。provider 自报价的行（网页对话里的 claude）
 * 只给一个总额，我们手里没有它的分项——这种行拿价目表推一份参考拆分，并在脚注
 * 里说清「上面是推算、最后一行才是实收」，不能让人当成账单原文。
 *
 * 单价是**当前**价目表里的值，而实收是入账当时算的：改过价的历史行两边对不上，
 * 差出来就直说，不硬凑。 */

const costDlg = () => $<HTMLDialogElement>("dlg-cost");

/* 单价按需给小数位：$5 就写 $5，$0.075 也不能被抹成 $0.08。 */
const fmtRate = (v: number) => "$" + v.toLocaleString("en-US", { maximumFractionDigits: 6 });

function costRow(cells: [string, string, string, string], cls = "") {
  const tr = document.createElement("tr");
  if (cls) tr.className = cls;
  cells.forEach((text, i) => {
    const td = document.createElement("td");
    if (i) td.className = "num";
    td.textContent = text;
    tr.appendChild(td);
  });
  return tr;
}

function openCost(r: UsageEventRow) {
  activeCost = r;
  renderCost(r);
  costDlg().showModal();
}

// Repaint the displayed receipt only; the selected row and its price snapshot
// are kept even if polling loads another page while the dialog is open.
function renderCost(r: UsageEventRow) {
  const rate = r.rate;

  const head = $("cost-head");
  head.replaceChildren();
  const top = document.createElement("div");
  top.className = "cost-model";
  top.append(usageAgentIcon(r.agent, 14),
    document.createTextNode(r.model || usageAgentName(r.agent) + i18nText("（默认模型）")));
  const b = billingFor(r);
  const chip = document.createElement("span");
  chip.className = "u-chip bill-" + r.billing;
  chip.textContent = b.text;
  top.appendChild(chip);
  const sub = document.createElement("div");
  sub.className = "cost-sub";
  sub.textContent = [workspaceName(r), KIND_LABEL[r.kind] || r.kind, fmtUsageTime(r.ts)]
    .filter(Boolean)
    .join(" · ");
  head.append(top, sub);
  if (r.delegation_id) {
    const task = document.createElement("div");
    task.className = "cost-sub";
    task.textContent = i18nText("委派 ID：{p0}", { p0: r.delegation_id });
    head.appendChild(task);
  }

  const body = $("cost-rows");
  body.replaceChildren();
  // 单价的单位是「美元 / 百万 token」，要的结果是微美元，两个 1e6 正好约掉：
  // micro = token × 单价（同 quota.go 的 priceEvent）。
  let sum = 0;
  for (const [label, tok, rt] of [
    [i18nText("输入"), r.input_tokens, rate?.input],
    [i18nText("输出"), r.output_tokens, rate?.output],
    [i18nText("缓存读取"), r.cache_read_tokens, rate?.cache_read],
    [i18nText("缓存写入"), r.cache_write_tokens, rate?.cache_write],
  ] as [string, number, number | undefined][]) {
    const micro = rt === undefined ? 0 : Math.round(tok * rt);
    sum += micro;
    body.appendChild(costRow([label, num(tok),
      rt === undefined ? "—" : fmtRate(rt),
      rt === undefined ? "—" : fmtUSD(micro)]));
  }

  // 逐桶四舍五入与服务端整笔四舍五入能差出一两个微美元，不算「对不上」。
  const off = !rate || rate.basis !== "table" || Math.abs(sum - r.cost_micro_usd) > 2;
  if (rate && off) body.appendChild(costRow([i18nText("按单价合计"), num(r.total_tokens), "", fmtUSD(sum)], "cost-sum"));
  body.appendChild(costRow([off ? i18nText("实收费用") : i18nText("合计"),
    rate && off ? "" : num(r.total_tokens), "", fmtUSD(r.cost_micro_usd)], "cost-total"));

  const note = $("cost-note");
  note.replaceChildren();
  const lines: string[] = [];
  if(r.kind==="decision"){
    const p=document.createElement("p");setText(p,"这是委派时 Jev 判断交给哪个跑腿的调用费用。费用未确认时按单次请求的上限入账，不作为零费用。");note.append(p);return;
  }
  if (!rate) {
    lines.push(r.billing === "provider"
      ? i18nText("这一行的钱由 provider 在回合收尾时自报，只给总额不给分项；这条记录也没有可用的参考单价，无法拆分。")
      : i18nText("这条记录没有可用单价，只记用量、不扣额度。配置价目表会用于之后的新记录，不会追溯补扣。"));
  } else if (rate.basis === "reference") {
    lines.push(i18nText("这一行的钱由 provider 自报总额，拆不出分项：上表是照{p0}价目表「{p1}」推的参考值，与实收有出入很正常。", { p0: String(rate.snapshot ? i18nText("入账时保存的") : i18nText("当前")), p1: String(rate.key) }));
  } else {
    lines.push(i18nText("按{p0}价目表「{p1}」{p2}折算。", { p0: String(rate.snapshot ? i18nText("入账时保存的") : i18nText("当前")), p1: String(rate.key), p2: String(rate.key === r.model ? "" : i18nText("（兜底价）")) }));
    if (off) lines.push(rate.snapshot ? i18nText("拆分与实收有差异，请以已入账金额为准。") : i18nText("这条历史记录没有价格快照，展示当前参考价；实收金额以入账记录为准。"));
  }
  if (rate?.per_request) lines.push(i18nText("本行按消息 ID 去重后逐请求计价，再按模型汇总；上表按标准单价展示，逐请求舍入或长上下文档可能使合计有差异。长上下文阈值按各请求判断，不按整行累计输入判断。"));
  if (rate && !rate.snapshot) lines.push(i18nText("旧记录没有保存入账价格，当前单价仅供参考。"));
  if (rate?.catalog_version) lines.push(i18nText("价格目录版本：{p0}；{p1}。", { p0: String(rate.catalog_version), p1: String(rate.verified_at ? i18nText("核验于 ") + fmtTime(Date.parse(rate.verified_at)) : i18nText("来源为尚未重新核验的预置快照")) }));
  if (rate?.long) {
    lines.push(i18nText("输入 + 缓存读取超过 {p0} token，整个回合走的是长上下文档单价。", { p0: String(num(rate.long_context_over || 0)) }));
  }
  lines.push(i18nText("计价算法：查价顺序为 模型 ID → 去掉 -20251001 这类日期后缀再查 → agent 名兜底；") +
    i18nText("费用 = 各桶 token × 该桶单价 ÷ 100 万（单价单位是美元 / 百万 token），四舍五入到微美元；") +
    (rate?.per_request ? i18nText("配了长上下文档时，每个请求分别判断输入 + 缓存读取是否超过阈值。") : i18nText("配了长上下文档时，输入 + 缓存读取超过阈值的回合整体改用超阈值那档单价。")));
  for (const t of lines) {
    const p = document.createElement("p");
    p.textContent = t;
    note.appendChild(p);
  }
}

$("cost-close").addEventListener("click", () => costDlg().close());
costDlg().addEventListener("close", () => { if (!costDlg().open) activeCost = null; });
window.addEventListener("agentbox-language-change", () => {
  if (!activeCost || !costDlg().open) return;
  const body = $("cost-body"), scrollTop = body.scrollTop;
  renderCost(activeCost);
  body.scrollTop = scrollTop;
});

function renderSummary(data: UsageEvents) {
  const box = $("usage-summary");
  box.replaceChildren();
  const t = data.total;
  // token 合计动辄上千万，卡片里按 K/M/B 缩写，悬停给完整数字。
  const exact = (n: number, note?: () => string) => () => num(n) + (note ? " · " + note() : "");
  // 总量与每行的「合计」同一口径：四个桶相加。回合数退到悬停提示里。
  const tokens = t.input_tokens + t.output_tokens + t.cache_read_tokens + t.cache_write_tokens;
  const items: [string, string, (() => string)?][] = [
    ["总花费", fmtUSD(t.cost_micro_usd, 2), () => i18nText("筛选范围内所有行的费用之和")],
    ["总 Token", fmtTokens(tokens), exact(tokens, () => i18nText("输入 + 输出 + 缓存读取 + 缓存写入；{p0} 个回合，{p1} 条记录", { p0: String(num(t.turns)), p1: String(num(t.rows)) }))],
    ["输入", fmtTokens(t.input_tokens), exact(t.input_tokens, () => i18nText("未命中缓存的输入 token"))],
    ["输出", fmtTokens(t.output_tokens), exact(t.output_tokens)],
    ["缓存读取", fmtTokens(t.cache_read_tokens), exact(t.cache_read_tokens, () => i18nText("命中缓存的输入，计价远低于新输入"))],
    ["缓存写入", fmtTokens(t.cache_write_tokens), exact(t.cache_write_tokens)],
  ];
  for (const [label, value, tip] of items) {
    const card = document.createElement("div");
    card.className = "us-item";
    if (tip) setTip(card, tip);
    const l = document.createElement("div");
    l.className = "us-label";
    setText(l, label);
    const v = document.createElement("div");
    v.className = "us-value mono";
    v.textContent = value;
    card.append(l, v);
    box.appendChild(card);
  }
}

function renderPager(data: UsageEvents) {
  const total = data.total.rows;
  const from = total === 0 ? 0 : offset + 1;
  const to = offset + data.rows.length;
  setText($("usage-page"), "显示 {p0} 至 {p1} 条，共 {p2} 条", { p0: String(num(from)), p1: String(num(to)), p2: String(num(total)) });
  const current = Math.floor(offset / pageSize) + 1;
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const numbers = $("usage-pages");
  const restoreFocus = numbers.contains(document.activeElement);
  numbers.replaceChildren();
  // 首尾页与当前页附近始终可达，大量记录也不会撑开分页栏。
  const start = Math.max(1, Math.min(current - 1, pages - 2));
  const end = Math.min(pages, start + 2);
  let previous = 0;
  for (const page of [...new Set([1, ...Array.from({ length: end - start + 1 }, (_, i) => start + i), pages])]) {
    if (page - previous > 1) {
      const gap = document.createElement("span");
      gap.className = "usage-page-gap";
      gap.textContent = "…";
      numbers.append(gap);
    }
    const button = document.createElement("button");
    button.type = "button";
    button.className = "usage-page-button";
    button.textContent = String(page);
    setAttrRender(button, "aria-label", () => i18nText("第 {p0} 页", { p0: String(page) }));
    if (page === current) button.setAttribute("aria-current", "page");
    button.addEventListener("click", () => goToPage(page));
    numbers.append(button);
    previous = page;
  }
  if (restoreFocus) numbers.querySelector<HTMLButtonElement>('[aria-current="page"]')?.focus();
}

function syncPagerBusy() {
  $<HTMLButtonElement>("usage-prev").disabled = loading || offset <= 0;
  $<HTMLButtonElement>("usage-next").disabled = loading || !last || offset + pageSize >= last.total.rows;
  $<HTMLSelectElement>("usage-page-size").disabled = loading;
  for (const button of $("usage-pages").querySelectorAll("button")) button.disabled = loading;
  $("usage-table-scroll").setAttribute("aria-busy", String(loading));
}

function goToPage(page: number) {
  if (loading || !last) return;
  const next = (Math.max(1, Math.min(page, Math.max(1, Math.ceil(last.total.rows / pageSize)))) - 1) * pageSize;
  if (next === offset) return;
  offset = next;
  resetScroll = true;
  void load();
}

/* ---------------- 加载 ---------------- */

async function load() {
  if (loading) { loadPending = true; return; }
  loading = true;
  syncPagerBusy();
  // 没有能沿用的行（首次进入、换筛选、翻页）先铺骨架；轮询刷新保留现有行，读完原地更新
  const blank = !last || resetScroll;
  if (blank) { rowMotion.reset(); skeletonRows(); }
  const generation = usageGeneration;
  try {
    const f = readFilters();
    renderFilterChip(f);
    const data = await api<UsageEvents>(
      "/usage/events?" + query(f, { limit: String(pageSize), offset: String(offset), order: orderParam() }));
    // 确认日期范围时上一趟可能仍在加载；不能让旧结果覆盖新筛选和排序。
    if (generation !== usageGeneration || loadPending) return;
    // 数据减少或相对时间窗口移动后，回到仍有记录的最后一页。
    const maxOffset = Math.max(0, Math.ceil(data.total.rows / pageSize) - 1) * pageSize;
    if (offset > maxOffset) {
      offset = maxOffset;
      resetScroll = true;
      loadPending = true;
      return;
    }
    last = data;
    const sync = data.sync;
    setTextRender($("usage-sub"), () => sync ? (sync.last_scan_at ? i18nText("终端全量扫描：{p0}{p1}{p2}", { p0: String(fmtTime(sync.last_scan_at)), p1: String(sync.errors ? i18nText(" · 部分文件失败，后台将重试") : ""), p2: String(sync.scanning ? i18nText(" · 同步中") : "") }) : i18nText("终端用量正在后台同步")) : i18nText("使用记录"));
    usageTimeZone = data.timezone || S.timeZone || "Asia/Shanghai";
    S.timeZone = usageTimeZone;
    syncTimeZone();
    // 服务端回声一份它实际用的顺序：万一这趟请求被丢掉了（上一趟还在跑），
    // 箭头跟着真拿到的顺序回正，不会出现「箭头指着正序、表还是倒序」。
    asc = data.order === "asc";
    syncSort();
    // 轮询与推送都走这里原地重画；换筛选、翻页的回顶在下面单独做
    keepScroll($("usage-rows"), () => {
      fillSelect("uf-user", data.facets.users);
      fillSelect("uf-agent", data.facets.agents, usageAgentName);
      fillSelect("uf-model", data.facets.models);
      renderSummary(data);
      renderRows(data);
      renderPager(data);
    });
    if (resetScroll) {
      $("usage-table-scroll").scrollTop = 0;
      resetScroll = false;
    }
  } catch (e) {
    if (generation === usageGeneration) {
      if (blank && !loadPending) $("usage-rows").replaceChildren(); // 骨架别一直留着假装还在读
      toast(i18nText("读取使用记录失败：") + (e as Error).message, true);
    }
  } finally {
    loading = false;
    syncPagerBusy();
    if (loadPending) { loadPending = false; void load(); }
  }
}

/* 改筛选条件要回到第一页：留在第 5 页上换条件，多半直接落进空页。 */
function reload() {
  offset = 0;
  resetScroll = true;
  void load();
}

export async function openUsageView() {
  showView("usage");
  usageTimeZone = S.timeZone || "Asia/Shanghai";
  syncTimeZone();
  setTextRender($("usage-sub"), () => S.role === "admin"
    ? i18nText("查看所有用户的用量与费用，每行对应一次模型请求。")
    : i18nText("查看你的用量与费用，每行对应一次模型请求。"));
  await load();
}

/* ---------------- 导出 ---------------- */

/* CSV 导出的是**当前筛选的全部行**，不是当前这一页——导出用来做离线核对，
 * 只给当前页没有意义。上限就是服务端的单页上限，超了会提示缩小范围。
 * 行序跟着页面上的排序走：导出的顺序和刚才看到的一致，才对得上。 */
async function exportCSV() {
  const btn = $<HTMLButtonElement>("uf-export");
  btn.disabled = true;
  try {
    const f = readFilters();
    const data = await api<UsageEvents>(
      "/usage/events?" + query(f, { limit: "500", offset: "0", order: orderParam() }));
    usageTimeZone = data.timezone || usageTimeZone;
    syncTimeZone();
    if (data.total.rows > data.rows.length) {
      // 截断是从哪头截的，得跟着排序方向说，否则「最近 500 行」是句假话
      toast(i18nText("只导出了{p0}的 {p1} 行（共 {p2} 行），", { p0: String(asc ? i18nText("最早") : i18nText("最近")), p1: String(data.rows.length), p2: String(data.total.rows) }) +
        i18nText("请缩小时间范围后分批导出"), true);
    }
    const head = [i18nText("时间（{p0}）", { p0: String(usageTimeZone) }), i18nText("用户"), i18nText("工作空间"), i18nText("工作空间ID"), i18nText("账号"), "Agent", i18nText("模型"), i18nText("思考强度"), i18nText("类型"), i18nText("计费"),
      i18nText("输入"), i18nText("输出"), i18nText("缓存读取"), i18nText("缓存写入"), i18nText("缓存命中率"), i18nText("合计Token"), i18nText("回合内序号"), i18nText("合并请求数"), i18nText("费用USD"),
      i18nText("首字ms"), i18nText("总耗时ms"), i18nText("模型耗时ms"), i18nText("请求首字ms"), i18nText("请求耗时ms"), i18nText("输出速度tok/s"), i18nText("回合ID"), i18nText("委派ID")];
    const lines = [head.join(",")];
    for (const r of data.rows) {
      lines.push([
        fmtUsageTime(r.ts, true),
        r.user, workspaceName(r), r.session_id, r.account_label || "",
        r.agent, r.model || "", effortText(r), KIND_LABEL[r.kind] || r.kind, billingFor(r).text,
        r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_write_tokens, cacheHitText(r), r.total_tokens,
        // 「3/27」：这一回合第 3 次请求、共 27 次；回合只有一次请求时留空。
        r.turn_requests ? `${r.turn_seq}/${r.turn_requests}` : "",
        r.merged_requests || "",
        // 导出给人算账，这里才把微美元换成美元；六位小数才装得下一次便宜回合。
        (r.cost_micro_usd / 1e6).toFixed(6),
        r.ttft_ms, r.wall_ms, r.duration_ms, r.api_ttft_ms || "", r.api_duration_ms || "", csvSpeed(r), r.turn_id, r.delegation_id || "",
      ].map(csvCell).join(","));
    }
    // BOM：没有它 Excel 会把 UTF-8 中文认成乱码。
    const blob = new Blob(["﻿" + lines.join("\r\n")], { type: "text/csv;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    startDownload(url);
    setTimeout(() => URL.revokeObjectURL(url), 5000);
  } catch (e) {
    toast(i18nText("导出失败：") + (e as Error).message, true);
  } finally {
    btn.disabled = false;
  }
}

/* 导出给人算账，速度留一位小数、不带「≈」（实测与否看「请求耗时ms」有没有值）；算不出留空。 */
function csvSpeed(r: UsageEventRow) {
  const { tps } = outputSpeed(r);
  return tps === null ? "" : tps.toFixed(1);
}

/* CSV 转义：逗号/引号/换行都要包起来，引号本身翻倍。 */
function csvCell(v: string | number) {
  const s = String(v);
  return /[",\r\n]/.test(s) ? '"' + s.replaceAll('"', '""') + '"' : s;
}

/* ---------------- 事件挂载 ---------------- */

for (const id of ["uf-user", "uf-agent", "uf-model", "uf-kind"]) $(id).addEventListener("change", reload);

initFiltersOpen();
syncSort();
$("usage-sort-asc").addEventListener("click", () => setSort(true));
$("usage-sort-desc").addEventListener("click", () => setSort(false));
$("uf-toggle").addEventListener("click", () => {
  setFiltersOpen($("usage-filters").classList.contains("collapsed"));
});
$("uf-refresh").addEventListener("click", reload);
$("uf-reset").addEventListener("click", () => {
  for (const id of ["uf-user", "uf-agent", "uf-model", "uf-kind"]) setSelectValue($<HTMLSelectElement>(id), "");
  dateRange.reset();
  reload();
});
$("uf-export").addEventListener("click", () => void exportCSV());
$("usage-prev").addEventListener("click", () => goToPage(Math.floor(offset / pageSize)));
$("usage-next").addEventListener("click", () => goToPage(Math.floor(offset / pageSize) + 2));
$("usage-page-size").addEventListener("change", () => {
  pageSize = Number($<HTMLSelectElement>("usage-page-size").value);
  reload();
});

bus.addEventListener("open-usage", () => void openUsageView());
bus.addEventListener("timezone-updated", () => {
  usageTimeZone = S.timeZone || "Asia/Shanghai";
  syncTimeZone();
  if (S.view === "usage") reload();
});

// 「跟随当前时刻」在使用记录页每 30 秒重新取值；固定历史区间不轮询。
setInterval(() => {
  if (S.view === "usage" && !document.hidden && dateRange.value().followNow) void load();
}, 30000);

/* 用量落库的推送（usage-live.ts）：使用记录页开着就连上，收到变更按当前筛选重读，
 * 和轮询一样原地更新、保留滚动位置。连着来的通知至少隔一秒才重读一次；页面在
 * 后台时只记下，切回来再读。轮询照旧保留：它还负责「跟随当前时刻」的时间窗前移。 */
const LIVE_MIN_GAP_MS = 1000;
let liveTimer: ReturnType<typeof setTimeout> | null = null;
let liveAt = 0;
let liveMissed = false;

function liveReload() {
  if (S.view !== "usage") return;
  if (document.hidden) { liveMissed = true; return; }
  if (liveTimer) return;
  liveTimer = setTimeout(() => {
    liveTimer = null;
    liveAt = Date.now();
    if (S.view === "usage") void load();
  }, Math.max(0, liveAt + LIVE_MIN_GAP_MS - Date.now()));
}
bus.addEventListener("view-changed", () => watchUsageLive("usage", S.view === "usage"));
bus.addEventListener("usage-changed", liveReload);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && liveMissed) { liveMissed = false; liveReload(); }
});

// Discard account-owned display snapshots on logout/page teardown. Language
// changes never trigger API reads, mutate filters, or reopen a closed dialog.
function clearUsageDisplay() {
  usageGeneration++;
  if (liveTimer) { clearTimeout(liveTimer); liveTimer = null; }
  liveMissed = false;
  last = null;
  activeCost = null;
  loadPending = false;
  rowMotion.reset();
  if (costDlg().open) costDlg().close();
  for (const id of ["usage-rows", "usage-summary", "usage-pages", "cost-head", "cost-rows", "cost-note"]) $(id).replaceChildren();
  for (const id of ["usage-page", "uf-active", "usage-sub"]) $(id).textContent = "";
}
bus.addEventListener("signed-out", clearUsageDisplay);
window.addEventListener("pagehide", clearUsageDisplay);
