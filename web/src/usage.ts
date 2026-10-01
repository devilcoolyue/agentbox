import { svgIcon } from "./icons.js";
import { cacheHitText } from "./usage-math.js";
/* usage：使用记录（消耗流水明细）。
 *
 * **一行是什么**：一个回合 × 一个模型，不是一次 API 调用。容器里的 CLI 直接打
 * provider 官方接口，我们不在链路上，只拿得到 CLI 在回合收尾汇总报的那份账。
 * 一个回合内部可能真打了十几次接口，这里合成一行；claude 会按模型再拆开
 * （子 agent 起标题用的 haiku 就是这么单独成行的），同回合各行共享 turn_id。
 *
 * 因此「回合数」永远按 turn_id 去重，而耗时/首字延迟是回合级指标、在同回合各行
 * 上重复出现——它们只能取一份看，跨行求和没有意义，合计里也就没有这两项。
 *
 * 金额一律以微美元整数在前后端之间传递，只在显示的最后一步除 1e6（同 quota.ts）。
 * 管理员看全部并多一列「用户」，普通用户只看得到自己的，服务端按 scope 下发。 */
"use strict";

import { setSelectValue } from "./select.js";

import { S, bus } from "./state.js";
import type { UsageEventRow, UsageEvents } from "./types.js";
import { $, toast, startDownload, fmtTime } from "./util.js";
import { DateRangePicker } from "./date-range.js";
import { api } from "./api.js";
import { showView } from "./shell.js";
import { fmtUSD } from "./quota.js";
import { agentIcon, agentName } from "./brand.js";
import { setTip } from "./tip.js";

let pageSize = 20;

/* 当前视图状态。offset 单独放：改筛选要归零，翻页只动它。 */
let offset = 0;
let last: UsageEvents | null = null;
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

interface ClockParts { year: number; month: number; day: number; hour: number; minute: number }

const zoneFormatters = new Map<string, Intl.DateTimeFormat>();

/* Intl 负责把一个绝对时刻投影到系统配置的 IANA 时区。formatToParts 避免依赖
 * 不同浏览器的日期字符串顺序；h23 保证午夜是 00:xx 而不是 24:xx。 */
function clockParts(d: Date): ClockParts {
  let fmt = zoneFormatters.get(usageTimeZone);
  if (!fmt) {
    fmt = new Intl.DateTimeFormat("en-CA", {
      timeZone: usageTimeZone,
      year: "numeric", month: "2-digit", day: "2-digit",
      hour: "2-digit", minute: "2-digit", hourCycle: "h23",
    });
    zoneFormatters.set(usageTimeZone, fmt);
  }
  const values: Record<string, number> = {};
  for (const p of fmt.formatToParts(d)) {
    if (p.type !== "literal") values[p.type] = Number(p.value);
  }
  return {
    year: values.year!, month: values.month!, day: values.day!,
    hour: values.hour!, minute: values.minute!,
  };
}

function fmtUsageTime(ms: number | string, withYear = false) {
  const p = clockParts(new Date(ms));
  const core = `${pad(p.month)}-${pad(p.day)} ${pad(p.hour)}:${pad(p.minute)}`;
  return withYear ? `${p.year}-${core}` : core;
}

const ZONE_NAMES: Record<string, string> = {
  "Asia/Shanghai": "中国标准时间",
  "Asia/Hong_Kong": "香港时间",
  "Asia/Tokyo": "日本时间",
  UTC: "协调世界时",
  "America/Los_Angeles": "美国太平洋时间",
  "America/New_York": "美国东部时间",
  "Europe/London": "英国时间",
};

function syncTimeZone() {
  $("usage-zone-name").textContent = ZONE_NAMES[usageTimeZone] || "系统时区";
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
  all.textContent = "全部";
  sel.appendChild(all);
  for (const v of values) {
    const o = document.createElement("option");
    o.value = v;
    o.textContent = label ? label(v) : v;
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
  const parts: string[] = [];
  if (f.user) parts.push(f.user);
  if (f.agent) parts.push(agentName(f.agent));
  if (f.model) parts.push(f.model);
  if (f.kind) parts.push(KIND_LABEL[f.kind] || f.kind);
  if (f.since || f.until) parts.push(dateRange.label());
  $("uf-active").textContent = parts.length ? parts.join(" · ") : "全部记录";
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

const KIND_LABEL: Record<string, string> = { chat: "对话", terminal: "终端", title: "起标题" };
const KIND_HINT: Record<string, string> = {
  terminal: "用户在「终端」页签里手敲 CLI 花的量，事后从 CLI 自己的记录里补记；只记账不扣额度",
  title: "服务端自动为新对话生成标题的那趟消耗，不是用户发起的",
};
const BILLING: Record<string, { text: string; title: string }> = {
  // 与对话页每轮下方的说法一致：写用户看得懂的来源，不写「价目表」「CLI」
  provider: { text: "客户端上报", title: "Claude Code 等客户端自己上报的估算成本，不代表订阅账号的实际扣款" },
  table: { text: "按单价估算", title: "客户端没有上报金额，按系统设置里的模型单价折算" },
  none: { text: "未定价", title: "客户端没有上报金额，这个模型也没有设置单价；这一行只记用量、不扣额度" },
};

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
function cellEl(label: string, cls = "") {
  const td = document.createElement("td");
  if (cls) td.className = cls;
  td.dataset.l = label;
  const v = document.createElement("div");
  v.className = "u-v";
  td.appendChild(v);
  return { td, v };
}

function cell(label: string, text: string, cls = "") {
  const { td, v } = cellEl(label, cls);
  v.textContent = text;
  return td;
}

function tokenCell(r: UsageEventRow) {
  const { td, v } = cellEl("Token", "num u-tok");
  // 主行给「入 / 出」，缓存读单独一行——缓存读经常是输入的十几倍，
  // 混在一起看不出这个回合到底喂了多少新内容。全站统一写字不画箭头：
  // 箭头方向在「发给模型」和「模型返回」两种视角下正好相反，曾经两页各画一套。
  const main = document.createElement("div");
  const io = document.createElement("span");
  io.className = "u-io";
  io.append(
    Object.assign(document.createElement("i"), { className: "u-arrow", textContent: "入" }),
    document.createTextNode(num(r.input_tokens)),
    Object.assign(document.createElement("i"), { className: "u-arrow", textContent: "出" }),
    document.createTextNode(num(r.output_tokens)),
  );
  main.appendChild(io);
  const cache = document.createElement("div");
  cache.className = "u-cache";
  cache.textContent = "缓存 " + num(r.cache_read_tokens) +
    (r.cache_write_tokens ? " / 写 " + num(r.cache_write_tokens) : "");
  v.append(main, cache);
  td.title =
    `输入 ${num(r.input_tokens)}\n输出 ${num(r.output_tokens)}\n` +
    `缓存读取 ${num(r.cache_read_tokens)}\n缓存写入 ${num(r.cache_write_tokens)}\n` +
    `合计 ${num(r.total_tokens)}`;
  return td;
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

    const u = cell("用户", r.user, "col-user");
    u.classList.toggle("hidden", self);
    tr.appendChild(u);

    // 会话名 + 账号。会话被删掉后名字为空，退回显示 id：这行消耗真实发生过，
    // 不能因为会话没了就不显示。
    const { td: sess, v: sessv } = cellEl("工作空间", "u-cell-sess");
    const name = document.createElement("div");
    name.className = "u-sess";
    name.textContent = r.session_name || r.session_id;
    name.title = name.textContent;
    if (!r.session_name) setTip(name, "工作空间已删除");
    const acct = document.createElement("div");
    acct.className = "u-sub";
    acct.textContent = r.account_label || r.account_id || "—";
    sessv.append(name, acct);
    tr.appendChild(sess);

    const { td: model, v: modelv } = cellEl("模型");
    const mline = document.createElement("div");
    mline.className = "u-model";
    const modelName = document.createElement("span");
    modelName.textContent = r.model || agentName(r.agent) + "（默认模型）";
    modelName.title = modelName.textContent;
    mline.append(agentIcon(r.agent, 15), modelName);
    modelv.appendChild(mline);
    if (r.provider) {
      const p = document.createElement("div");
      p.className = "u-sub";
      p.textContent = r.provider === "firstParty" ? "官方直连" : r.provider;
      modelv.appendChild(p);
    }
    tr.appendChild(model);

    const { td: kind, v: kindv } = cellEl("类型");
    const chip = document.createElement("span");
    // 终端和起标题各有各的颜色：这一列的用处就是一眼看出「这笔钱是谁按下去的」。
    chip.className = "u-chip" + (r.kind === "title" ? " title" : r.kind === "terminal" ? " term" : r.kind === "chat" ? " chat" : "");
    chip.textContent = KIND_LABEL[r.kind] || r.kind;
    if (KIND_HINT[r.kind]) setTip(chip, KIND_HINT[r.kind]);
    kindv.appendChild(chip);
    tr.appendChild(kind);

    const b = BILLING[r.billing] || { text: r.billing, title: "" };
    const bchip = document.createElement("span");
    bchip.className = "u-chip bill-" + r.billing;
    bchip.textContent = b.text;
    setTip(bchip, b.title);

    tr.appendChild(tokenCell(r));

    const hit = cell("缓存命中率", cacheHitText(r), "num u-hit");
    setTip(hit, "缓存读取 ÷（未缓存输入 + 缓存读取 + 缓存写入）；不含输出 token，无输入时显示 —");
    tr.appendChild(hit);

    // 金额后面挂一个「?」：点开是这一行的分项算式（输入/输出/缓存各花了多少）。
    // 金额本身看不出为什么是这个数，尤其是缓存读取常常比输入贵不了几分钱、
    // 却占了大头。
    const { td: cost, v: costv } = cellEl("费用", "num u-cost");
    if (!r.cost_micro_usd) cost.classList.add("zero");
    const why = document.createElement("button");
    why.type = "button";
    why.className = "u-why";
    why.append(svgIcon("help", 14));
    setTip(why, "这笔钱是怎么算出来的");
    why.setAttribute("aria-label", "费用明细");
    why.addEventListener("click", () => openCost(r));
    costv.append(document.createTextNode(fmtUSD(r.cost_micro_usd)), why);
    const billing = document.createElement("div");
    billing.className = "u-billing";
    billing.appendChild(bchip);
    costv.appendChild(billing);
    tr.appendChild(cost);

    // 首字 + 总耗时，两个数同源（都走我们自己的表：容器就绪 → 首个输出 / 进程退出），
    // 所以首字必然 ≤ 总耗时。provider 自报的模型侧耗时不上表——它不含 CLI 启动那两秒，
    // 跟首字不是一个口径，摆在一起会出现「首字 3.1s / 耗时 2.3s」这种看着不可能的行。
    //
    // 两个数都是回合级的，同回合拆成多行时每行重复——所以标题里点明，免得有人
    // 把一列加起来当总时长。
    const { td: lat, v: latv } = cellEl("延迟", "num u-lat");
    const ttft = document.createElement("div");
    ttft.textContent = "首字 " + fmtDur(r.ttft_ms);
    const dur = document.createElement("div");
    dur.className = "u-sub";
    // 老数据没量过墙钟，那种行退回 provider 报的模型侧耗时，并把标签换掉——
    // 宁可标明这是另一个口径，也不要把它冒充成总耗时。
    dur.textContent = r.wall_ms ? "总耗时 " + fmtDur(r.wall_ms) : "模型 " + fmtDur(r.duration_ms);
    latv.append(ttft, dur);
    setTip(lat, "首字与总耗时都从容器就绪开始算，含 CLI 启动，两个数同源。"
      + "老数据没量过总耗时，退回显示 provider 自报的模型侧耗时（标「模型」，不含启动）。"
      + "都是回合级指标：同一回合拆成多行时每行都是这个值，不要跨行求和");
    tr.appendChild(lat);

    const { td: time, v: timev } = cellEl("时间", "num u-time");
    const stamp = fmtUsageTime(r.ts, true);
    const day = document.createElement("div");
    day.className = "u-sub";
    day.textContent = stamp.slice(0, 10).replaceAll("-", "/");
    timev.append(document.createTextNode(stamp.slice(11)), day);
    tr.appendChild(time);
    body.appendChild(tr);
  }

  $("usage-empty").classList.toggle("hidden", data.rows.length > 0);
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
  const rate = r.rate;

  const head = $("cost-head");
  head.replaceChildren();
  const top = document.createElement("div");
  top.className = "cost-model";
  top.append(agentIcon(r.agent, 14),
    document.createTextNode(r.model || agentName(r.agent) + "（默认模型）"));
  const b = BILLING[r.billing] || { text: r.billing, title: "" };
  const chip = document.createElement("span");
  chip.className = "u-chip bill-" + r.billing;
  chip.textContent = b.text;
  top.appendChild(chip);
  const sub = document.createElement("div");
  sub.className = "cost-sub";
  sub.textContent = [r.session_name || r.session_id, KIND_LABEL[r.kind] || r.kind, fmtUsageTime(r.ts)]
    .join(" · ");
  head.append(top, sub);

  const body = $("cost-rows");
  body.replaceChildren();
  // 单价的单位是「美元 / 百万 token」，要的结果是微美元，两个 1e6 正好约掉：
  // micro = token × 单价（同 quota.go 的 priceEvent）。
  let sum = 0;
  for (const [label, tok, rt] of [
    ["输入", r.input_tokens, rate?.input],
    ["输出", r.output_tokens, rate?.output],
    ["缓存读取", r.cache_read_tokens, rate?.cache_read],
    ["缓存写入", r.cache_write_tokens, rate?.cache_write],
  ] as [string, number, number | undefined][]) {
    const micro = rt === undefined ? 0 : Math.round(tok * rt);
    sum += micro;
    body.appendChild(costRow([label, num(tok),
      rt === undefined ? "—" : fmtRate(rt),
      rt === undefined ? "—" : fmtUSD(micro)]));
  }

  // 逐桶四舍五入与服务端整笔四舍五入能差出一两个微美元，不算「对不上」。
  const off = !rate || rate.basis !== "table" || Math.abs(sum - r.cost_micro_usd) > 2;
  if (rate && off) body.appendChild(costRow(["按单价合计", num(r.total_tokens), "", fmtUSD(sum)], "cost-sum"));
  body.appendChild(costRow([off ? "实收费用" : "合计",
    rate && off ? "" : num(r.total_tokens), "", fmtUSD(r.cost_micro_usd)], "cost-total"));

  const note = $("cost-note");
  note.replaceChildren();
  const lines: string[] = [];
  if (!rate) {
    lines.push(r.billing === "provider"
      ? "这一行的钱由 provider 在回合收尾时自报，只给总额不给分项；这条记录也没有可用的参考单价，无法拆分。"
      : "这条记录没有可用单价，只记用量、不扣额度。配置价目表会用于之后的新记录，不会追溯补扣。");
  } else if (rate.basis === "reference") {
    lines.push(`这一行的钱由 provider 自报总额，拆不出分项：上表是照${rate.snapshot ? "入账时保存的" : "当前"}价目表「${rate.key}」推的参考值，与实收有出入很正常。`);
  } else {
    lines.push(`按${rate.snapshot ? "入账时保存的" : "当前"}价目表「${rate.key}」${rate.key === r.model ? "" : "（兜底价）"}折算。`);
    if (off) lines.push(rate.snapshot ? "拆分与实收有差异，请以已入账金额为准。" : "这条历史记录没有价格快照，展示当前参考价；实收金额以入账记录为准。");
  }
  if (rate?.per_request) lines.push("本行按消息 ID 去重后逐请求计价，再按模型汇总；上表按标准单价展示，逐请求舍入或长上下文档可能使合计有差异。长上下文阈值按各请求判断，不按整行累计输入判断。");
  if (rate && !rate.snapshot) lines.push("旧记录没有保存入账价格，当前单价仅供参考。");
  if (rate?.catalog_version) lines.push(`价格目录版本：${rate.catalog_version}；${rate.verified_at ? "核验于 " + fmtTime(Date.parse(rate.verified_at)) : "来源为尚未重新核验的预置快照"}。`);
  if (rate?.long) {
    lines.push(`输入 + 缓存读取超过 ${num(rate.long_context_over || 0)} token，整个回合走的是长上下文档单价。`);
  }
  lines.push("计价算法：查价顺序为 模型 ID → 去掉 -20251001 这类日期后缀再查 → agent 名兜底；" +
    "费用 = 各桶 token × 该桶单价 ÷ 100 万（单价单位是美元 / 百万 token），四舍五入到微美元；" +
    (rate?.per_request ? "配了长上下文档时，每个请求分别判断输入 + 缓存读取是否超过阈值。" : "配了长上下文档时，输入 + 缓存读取超过阈值的回合整体改用超阈值那档单价。"));
  for (const t of lines) {
    const p = document.createElement("p");
    p.textContent = t;
    note.appendChild(p);
  }

  costDlg().showModal();
}

$("cost-close").addEventListener("click", () => costDlg().close());

function renderSummary(data: UsageEvents) {
  const box = $("usage-summary");
  box.replaceChildren();
  const t = data.total;
  const items: [string, string, string?][] = [
    ["总花费", fmtUSD(t.cost_micro_usd, 2), "筛选范围内所有行的费用之和"],
    ["回合", num(t.turns), `按 turn_id 去重；共 ${num(t.rows)} 行明细`],
    ["输入", num(t.input_tokens), "未命中缓存的输入 token"],
    ["输出", num(t.output_tokens), ""],
    ["缓存读取", num(t.cache_read_tokens), "命中缓存的输入，计价远低于新输入"],
    ["缓存写入", num(t.cache_write_tokens), ""],
  ];
  for (const [label, value, tip] of items) {
    const card = document.createElement("div");
    card.className = "us-item";
    if (tip) setTip(card, tip);
    const l = document.createElement("div");
    l.className = "us-label";
    l.textContent = label;
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
  $("usage-page").textContent = `显示 ${num(from)} 至 ${num(to)} 条，共 ${num(total)} 条`;
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
    button.setAttribute("aria-label", `第 ${page} 页`);
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
  $("usage-loading").classList.remove("hidden");
  try {
    const f = readFilters();
    renderFilterChip(f);
    const data = await api<UsageEvents>(
      "/usage/events?" + query(f, { limit: String(pageSize), offset: String(offset), order: orderParam() }));
    // 确认日期范围时上一趟可能仍在加载；不能让旧结果覆盖新筛选和排序。
    if (loadPending) return;
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
    $("usage-sub").textContent = sync ? (sync.last_scan_at ? `终端全量扫描：${fmtTime(sync.last_scan_at)}${sync.errors ? " · 部分文件失败，后台将重试" : ""}${sync.scanning ? " · 同步中" : ""}` : "终端用量正在后台同步") : "使用记录";
    usageTimeZone = data.timezone || S.timeZone || "Asia/Shanghai";
    S.timeZone = usageTimeZone;
    syncTimeZone();
    // 服务端回声一份它实际用的顺序：万一这趟请求被丢掉了（上一趟还在跑），
    // 箭头跟着真拿到的顺序回正，不会出现「箭头指着正序、表还是倒序」。
    asc = data.order === "asc";
    syncSort();
    fillSelect("uf-user", data.facets.users);
    fillSelect("uf-agent", data.facets.agents, agentName);
    fillSelect("uf-model", data.facets.models);
    renderSummary(data);
    renderRows(data);
    renderPager(data);
    if (resetScroll) {
      $("usage-table-scroll").scrollTop = 0;
      resetScroll = false;
    }
  } catch (e) {
    toast("读取使用记录失败：" + (e as Error).message, true);
  } finally {
    loading = false;
    syncPagerBusy();
    $("usage-loading").classList.add("hidden");
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
  $("usage-sub").textContent = S.role === "admin"
    ? "查看所有用户的用量与费用，每行对应一个回合中的一个模型。"
    : "查看你的用量与费用，每行对应一个回合中的一个模型。";
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
      toast(`只导出了${asc ? "最早" : "最近"}的 ${data.rows.length} 行（共 ${data.total.rows} 行），` +
        "请缩小时间范围后分批导出", true);
    }
    const head = [`时间（${usageTimeZone}）`, "用户", "工作空间", "工作空间ID", "账号", "Agent", "模型", "类型", "计费",
      "输入", "输出", "缓存读取", "缓存写入", "缓存命中率", "合计Token", "费用USD",
      "首字ms", "总耗时ms", "模型耗时ms", "回合ID"];
    const lines = [head.join(",")];
    for (const r of data.rows) {
      lines.push([
        fmtUsageTime(r.ts, true),
        r.user, r.session_name || "", r.session_id, r.account_label || "",
        r.agent, r.model || "", KIND_LABEL[r.kind] || r.kind, (BILLING[r.billing] || { text: r.billing }).text,
        r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_write_tokens, cacheHitText(r), r.total_tokens,
        // 导出给人算账，这里才把微美元换成美元；六位小数才装得下一次便宜回合。
        (r.cost_micro_usd / 1e6).toFixed(6),
        r.ttft_ms, r.wall_ms, r.duration_ms, r.turn_id,
      ].map(csvCell).join(","));
    }
    // BOM：没有它 Excel 会把 UTF-8 中文认成乱码。
    const blob = new Blob(["﻿" + lines.join("\r\n")], { type: "text/csv;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    startDownload(url);
    setTimeout(() => URL.revokeObjectURL(url), 5000);
  } catch (e) {
    toast("导出失败：" + (e as Error).message, true);
  } finally {
    btn.disabled = false;
  }
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
