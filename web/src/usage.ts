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

import { S, bus } from "./state.js";
import type { UsageEventRow, UsageEvents } from "./types.js";
import { $, toast, fmtTime, startDownload } from "./util.js";
import { api } from "./api.js";
import { showView } from "./shell.js";
import { fmtUSD } from "./quota.js";
import { agentIcon, agentName } from "./brand.js";

const PAGE = 50;

/* 当前视图状态。offset 单独放：改筛选要归零，翻页只动它。 */
let offset = 0;
let last: UsageEvents | null = null;
let loading = false;

/* ---------------- 筛选条件 ---------------- */

interface Filters {
  user: string;
  agent: string;
  model: string;
  kind: string;
  since: string; // <input type=date> 的 YYYY-MM-DD
  until: string;
}

function readFilters(): Filters {
  return {
    user: $<HTMLSelectElement>("uf-user").value,
    agent: $<HTMLSelectElement>("uf-agent").value,
    model: $<HTMLSelectElement>("uf-model").value,
    kind: $<HTMLSelectElement>("uf-kind").value,
    since: $<HTMLInputElement>("uf-since").value,
    until: $<HTMLInputElement>("uf-until").value,
  };
}

/* 日期框给的是本地日历日，服务端要 RFC3339 时间点。since 取当天 00:00，
 * until 取次日 00:00——服务端的上界是开区间，不加这一天会把当天整个排除掉，
 * 「今天到今天」查出来是空的。 */
function query(f: Filters, extra: Record<string, string> = {}) {
  const q = new URLSearchParams();
  const put = (k: string, v: string) => { if (v) q.set(k, v); };
  put("user", f.user);
  put("agent", f.agent);
  put("model", f.model);
  put("kind", f.kind);
  if (f.since) q.set("since", new Date(f.since + "T00:00:00").toISOString());
  if (f.until) {
    const d = new Date(f.until + "T00:00:00");
    d.setDate(d.getDate() + 1);
    q.set("until", d.toISOString());
  }
  for (const [k, v] of Object.entries(extra)) q.set(k, v);
  return q.toString();
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
  sel.value = values.includes(cur) ? cur : "";
}

/* ---------------- 渲染 ---------------- */

const KIND_LABEL: Record<string, string> = { chat: "对话", terminal: "终端", title: "起标题" };
const KIND_HINT: Record<string, string> = {
  terminal: "用户在「终端」页签里手敲 CLI 花的量，事后从 CLI 自己的记录里补记；只记账不扣额度",
  title: "服务端自动为新对话生成标题的那趟消耗，不是用户发起的",
};
const BILLING: Record<string, { text: string; title: string }> = {
  provider: { text: "官方报价", title: "费用由 provider 在回合收尾时自己报出，我们原样记账" },
  table: { text: "价目表", title: "provider 不报价，按系统设置里的价目表按 token 折算" },
  none: { text: "未定价", title: "provider 不报价且没有配置价目表，这一行只记用量、不扣额度" },
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

function cell(text: string, cls = "") {
  const td = document.createElement("td");
  if (cls) td.className = cls;
  td.textContent = text;
  return td;
}

function tokenCell(r: UsageEventRow) {
  const td = document.createElement("td");
  td.className = "num u-tok";
  // 主行给「输入 ↓ / 输出 ↑」，缓存读单独一行——缓存读经常是输入的十几倍，
  // 混在一起看不出这个回合到底喂了多少新内容。
  const main = document.createElement("div");
  const io = document.createElement("span");
  io.className = "u-io";
  io.append(
    Object.assign(document.createElement("i"), { className: "u-arrow", textContent: "↓" }),
    document.createTextNode(num(r.input_tokens)),
    Object.assign(document.createElement("i"), { className: "u-arrow up", textContent: "↑" }),
    document.createTextNode(num(r.output_tokens)),
  );
  main.appendChild(io);
  const cache = document.createElement("div");
  cache.className = "u-cache";
  cache.textContent = "缓存 " + num(r.cache_read_tokens) +
    (r.cache_write_tokens ? " / 写 " + num(r.cache_write_tokens) : "");
  td.append(main, cache);
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

    const u = cell(r.user, "col-user");
    u.classList.toggle("hidden", self);
    tr.appendChild(u);

    // 会话名 + 账号。会话被删掉后名字为空，退回显示 id：这行消耗真实发生过，
    // 不能因为会话没了就不显示。
    const sess = document.createElement("td");
    const name = document.createElement("div");
    name.className = "u-sess";
    name.textContent = r.session_name || r.session_id;
    if (!r.session_name) name.title = "会话已删除";
    const acct = document.createElement("div");
    acct.className = "u-sub";
    acct.textContent = r.account_label || r.account_id || "—";
    sess.append(name, acct);
    tr.appendChild(sess);

    const model = document.createElement("td");
    const mline = document.createElement("div");
    mline.className = "u-model";
    mline.append(agentIcon(r.agent, 13), document.createTextNode(r.model || agentName(r.agent) + "（默认模型）"));
    model.appendChild(mline);
    if (r.provider) {
      const p = document.createElement("div");
      p.className = "u-sub";
      p.textContent = r.provider === "firstParty" ? "官方直连" : r.provider;
      model.appendChild(p);
    }
    tr.appendChild(model);

    const kind = document.createElement("td");
    const chip = document.createElement("span");
    // 终端和起标题各有各的颜色：这一列的用处就是一眼看出「这笔钱是谁按下去的」。
    chip.className = "u-chip" + (r.kind === "title" ? " title" : r.kind === "terminal" ? " term" : "");
    chip.textContent = KIND_LABEL[r.kind] || r.kind;
    if (KIND_HINT[r.kind]) chip.title = KIND_HINT[r.kind];
    kind.appendChild(chip);
    tr.appendChild(kind);

    const bill = document.createElement("td");
    const b = BILLING[r.billing] || { text: r.billing, title: "" };
    const bchip = document.createElement("span");
    bchip.className = "u-chip bill-" + r.billing;
    bchip.textContent = b.text;
    bchip.title = b.title;
    bill.appendChild(bchip);
    tr.appendChild(bill);

    tr.appendChild(tokenCell(r));

    const cost = cell(fmtUSD(r.cost_micro_usd), "num u-cost");
    if (!r.cost_micro_usd) cost.classList.add("zero");
    tr.appendChild(cost);

    // 首字 / 总耗时都是回合级的，同回合各行重复——所以标题里点明，免得有人
    // 把一列加起来当总时长。
    //
    // 主行的两个数都走我们自己的表（容器就绪 → 首个输出 / 进程退出），所以
    // 首字必然 ≤ 总耗时。provider 自报的耗时只算模型侧、不含 CLI 启动那两秒，
    // 拿它当总耗时会比首字还小，所以降到副行单独标「模型」。
    const lat = document.createElement("td");
    lat.className = "num u-lat";
    const ttft = document.createElement("div");
    ttft.textContent = "首字 " + fmtDur(r.ttft_ms);
    const dur = document.createElement("div");
    dur.className = "u-sub";
    dur.textContent = r.wall_ms
      ? "总耗时 " + fmtDur(r.wall_ms) + "（模型 " + fmtDur(r.duration_ms) + "）"
      : "模型 " + fmtDur(r.duration_ms); // 老数据没量过墙钟
    lat.append(ttft, dur);
    lat.title = "首字与总耗时都从容器就绪开始算，含 CLI 启动；括号里是 provider 自报的模型侧耗时，不含启动。"
      + "三个都是回合级指标：同一回合拆成多行时每行都是这个值，不要跨行求和";
    tr.appendChild(lat);

    tr.appendChild(cell(fmtTime(r.ts), "num u-time"));
    body.appendChild(tr);
  }

  $("usage-empty").classList.toggle("hidden", data.rows.length > 0);
}

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
    if (tip) card.title = tip;
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
  const from = data.total.rows === 0 ? 0 : offset + 1;
  const to = offset + data.rows.length;
  $("usage-page").textContent = `${from}–${to} / ${num(data.total.rows)}`;
  $<HTMLButtonElement>("usage-prev").disabled = offset <= 0;
  $<HTMLButtonElement>("usage-next").disabled = to >= data.total.rows;
}

/* ---------------- 加载 ---------------- */

async function load() {
  if (loading) return;
  loading = true;
  $("usage-loading").classList.remove("hidden");
  try {
    const f = readFilters();
    const data = await api<UsageEvents>(
      "/usage/events?" + query(f, { limit: String(PAGE), offset: String(offset) }));
    last = data;
    fillSelect("uf-user", data.facets.users);
    fillSelect("uf-agent", data.facets.agents, agentName);
    fillSelect("uf-model", data.facets.models);
    renderSummary(data);
    renderRows(data);
    renderPager(data);
  } catch (e) {
    toast("读取使用记录失败：" + (e as Error).message, true);
  } finally {
    loading = false;
    $("usage-loading").classList.add("hidden");
  }
}

/* 改筛选条件要回到第一页：留在第 5 页上换条件，多半直接落进空页。 */
function reload() {
  offset = 0;
  void load();
}

export async function openUsageView() {
  showView("usage");
  $("usage-sub").textContent = S.role === "admin"
    ? "所有用户的消耗流水。每一行是一个回合在一个模型上的消耗，费用与 token 由 agent 在回合收尾时上报。"
    : "你自己的消耗流水。每一行是一个回合在一个模型上的消耗，费用与 token 由 agent 在回合收尾时上报。";
  await load();
}

/* ---------------- 导出 ---------------- */

/* CSV 导出的是**当前筛选的全部行**，不是当前这一页——导出用来做离线核对，
 * 给一页 50 行没有意义。上限就是服务端的单页上限，超了会提示缩小范围。 */
async function exportCSV() {
  const btn = $<HTMLButtonElement>("uf-export");
  btn.disabled = true;
  try {
    const f = readFilters();
    const data = await api<UsageEvents>(
      "/usage/events?" + query(f, { limit: "500", offset: "0" }));
    if (data.total.rows > data.rows.length) {
      toast(`只导出了最近 ${data.rows.length} 行（共 ${data.total.rows} 行），请缩小时间范围后分批导出`, true);
    }
    const head = ["时间", "用户", "会话", "会话ID", "账号", "Agent", "模型", "类型", "计费",
      "输入", "输出", "缓存读取", "缓存写入", "合计Token", "费用USD",
      "首字ms", "总耗时ms", "模型耗时ms", "回合ID"];
    const lines = [head.join(",")];
    for (const r of data.rows) {
      lines.push([
        new Date(r.ts).toLocaleString("zh-CN"),
        r.user, r.session_name || "", r.session_id, r.account_label || "",
        r.agent, r.model || "", KIND_LABEL[r.kind] || r.kind, (BILLING[r.billing] || { text: r.billing }).text,
        r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_write_tokens, r.total_tokens,
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

for (const id of ["uf-user", "uf-agent", "uf-model", "uf-kind", "uf-since", "uf-until"]) {
  $(id).addEventListener("change", reload);
}
$("uf-refresh").addEventListener("click", reload);
$("uf-reset").addEventListener("click", () => {
  for (const id of ["uf-user", "uf-agent", "uf-model", "uf-kind"]) $<HTMLSelectElement>(id).value = "";
  for (const id of ["uf-since", "uf-until"]) $<HTMLInputElement>(id).value = "";
  reload();
});
$("uf-export").addEventListener("click", () => void exportCSV());
$("usage-prev").addEventListener("click", () => {
  offset = Math.max(0, offset - PAGE);
  void load();
});
$("usage-next").addEventListener("click", () => {
  if (last && offset + PAGE < last.total.rows) {
    offset += PAGE;
    void load();
  }
});

bus.addEventListener("open-usage", () => void openUsageView());
