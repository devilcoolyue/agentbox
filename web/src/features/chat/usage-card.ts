import type { AgentEvent, UsageLimit, UsageReport } from "../../types.js";
import { setAttrRender, setText, setTextRender, t as i18nText } from "../../i18n.js";
import { fmtTime } from "../../util.js";

/* /usage 的回答卡片：CLI 随这条本地回答附带结构化的 usage_report（套餐各额度窗口的
 * 已用百分比、重置时间、服务端给的严重程度，以及额外用量），这里画成终端 /usage 页
 * 那几根进度条。窗口有哪些、什么顺序、什么颜色都照服务端给的，按 kind/scope 起名，
 * 不按文案猜；重置时间按系统时区显示（容器里的 CLI 只会写 UTC）。
 * usage_report.session 是这一次 CLI 进程的累计，网页每轮另起进程，这里恒为 0，不显示；
 * 网页自己的花费在每轮底部和「使用记录」里。
 * 「用量构成」不在结构化字段里，只能从文字回答解析；认不出的行原样显示。
 * 字段是 CLI 标明的实验字段，没有限额行时退回文字回答。 */

/** 这条 assistant 事件带的 /usage 结构化结果；没有可画的限额行时返回 undefined */
export function usageReportOf(ev: AgentEvent): UsageReport | undefined {
  const report = ev.usage_report as UsageReport | undefined;
  if (!report || typeof report !== "object") return undefined;
  const limits = report.rate_limits?.limits;
  return Array.isArray(limits) && limits.some(validLimit) ? report : undefined;
}

const validLimit = (l: UsageLimit | null | undefined): l is UsageLimit =>
  !!l && typeof l === "object" && typeof l.percent === "number" && Number.isFinite(l.percent);

const el = <K extends keyof HTMLElementTagNameMap>(tag: K, className: string, text?: string) => {
  const node = document.createElement(tag);
  node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
};

function limitLabel(l: UsageLimit): () => string {
  const scope = l.scope?.model?.display_name || l.scope?.surface?.display_name || "";
  if (l.kind === "session") return () => i18nText("当前会话");
  if (l.kind === "weekly_all") return () => i18nText("本周（全部模型）");
  if (l.kind === "weekly_scoped" && scope) return () => i18nText("本周（{p0}）", { p0: scope });
  const group = l.group === "session" ? () => i18nText("当前会话")
    : l.group === "weekly" ? () => i18nText("本周")
    : l.group === "monthly" ? () => i18nText("本月")
    : () => l.group || l.kind || "";
  return scope ? () => i18nText("{p0}（{p1}）", { p0: group(), p1: scope }) : group;
}

/** 剩余时间：3 小时 12 分钟后 / 2 天 4 小时后 */
function untilText(ms: number) {
  const minutes = Math.max(1, Math.round(ms / 60000));
  if (minutes < 60) return i18nText("{p0} 分钟后", { p0: String(minutes) });
  const hours = Math.floor(minutes / 60), rest = minutes % 60;
  if (hours < 24) return rest ? i18nText("{p0} 小时 {p1} 分钟后", { p0: String(hours), p1: String(rest) }) : i18nText("{p0} 小时后", { p0: String(hours) });
  const days = Math.floor(hours / 24), h = hours % 24;
  return h ? i18nText("{p0} 天 {p1} 小时后", { p0: String(days), p1: String(h) }) : i18nText("{p0} 天后", { p0: String(days) });
}

function resetText(at: string | null | undefined) {
  const t = at ? Date.parse(at) : NaN;
  if (!Number.isFinite(t)) return "";
  const left = t - Date.now();
  return left > 0
    ? i18nText("{p0} 重置 · {p1}", { p0: fmtTime(t), p1: untilText(left) })
    : i18nText("已于 {p0} 重置", { p0: fmtTime(t) });
}

const level = (severity: unknown) => severity === "critical" ? "critical" : severity === "warning" ? "warning" : "normal";

function meter(label: () => string, percent: number, value: () => string, note: () => string, severity: unknown) {
  const p = Math.min(100, Math.max(0, percent));
  const row = el("div", "usage-limit");
  row.dataset.level = level(severity);
  const head = el("div", "usage-limit-head");
  const name = el("span", "usage-limit-label");
  setTextRender(name, label);
  const amount = el("span", "usage-limit-value mono");
  setTextRender(amount, value);
  head.append(name, amount);
  const bar = el("div", "usage-bar");
  bar.setAttribute("role", "progressbar");
  bar.setAttribute("aria-valuemin", "0");
  bar.setAttribute("aria-valuemax", "100");
  bar.setAttribute("aria-valuenow", String(Math.round(p)));
  setAttrRender(bar, "aria-label", label);
  const fill = el("span", "usage-bar-fill");
  fill.style.setProperty("--p", `${p}%`);
  bar.append(fill);
  row.append(head, bar);
  const reset = el("div", "usage-limit-reset");
  setTextRender(reset, note);
  if (note()) row.append(reset);
  return row;
}

/** 额外用量的金额是 currency 的最小单位（美元即美分） */
function money(minor: number, currency: string | null | undefined) {
  const code = (currency || "USD").toUpperCase();
  try {
    const fmt = new Intl.NumberFormat("en-US", { style: "currency", currency: code });
    return fmt.format(minor / 10 ** (fmt.resolvedOptions().maximumFractionDigits ?? 2));
  } catch {
    return `${(minor / 100).toFixed(2)} ${code}`;
  }
}

function extraRow(extra: NonNullable<NonNullable<UsageReport["rate_limits"]>["extra_usage"]>) {
  const limit = typeof extra.monthly_limit === "number" ? extra.monthly_limit : null;
  const used = typeof extra.used_credits === "number" ? extra.used_credits : null;
  if (!extra.is_enabled && limit === null) return null;
  const percent = typeof extra.utilization === "number" ? extra.utilization : limit && used !== null ? used / limit * 100 : 0;
  const amount = () => limit !== null
    ? `${money(used ?? 0, extra.currency)} / ${money(limit, extra.currency)}`
    : i18nText("已用 {p0}，未设上限", { p0: money(used ?? 0, extra.currency) });
  const note = () => extra.is_enabled ? i18nText("套餐额度用完后按量计费") : i18nText("当前不可用于继续发送");
  return meter(() => i18nText("本月额外用量"), percent, amount, note, percent >= 100 ? "critical" : "normal");
}

/* ---- 文字回答里的「用量构成」：CLI 只给了文字，按它的固定句式解析 ---- */

type Trait = { pct?: string; text: () => string; list?: string[] };
type Period = { head: () => string; traits: Trait[] };

const TRAITS: [RegExp, () => string][] = [
  [/^(\d+)% of your usage hit a >100k-token cache miss$/, () => i18nText("用量发生在超过 10 万 token 的缓存未命中时")],
  [/^(\d+)% of your usage was at >150k context$/, () => i18nText("用量发生在上下文超过 15 万 token 时")],
  [/^(\d+)% of your usage came from subagent-heavy sessions$/, () => i18nText("用量来自大量使用子 Agent 的会话")],
  [/^(\d+)% of your usage was while 4\+ sessions ran in parallel$/, () => i18nText("用量发生在 4 个以上会话并行时")],
  [/^(\d+)% of your usage came from sessions active for 8\+ hours$/, () => i18nText("用量来自持续活跃 8 小时以上的会话")],
];
const TOPS: Record<string, () => string> = {
  "skills": () => i18nText("常用技能"),
  "subagents": () => i18nText("常用子 Agent"),
  "plugins": () => i18nText("常用插件"),
  "MCP servers": () => i18nText("常用 MCP 服务"),
};

function trait(line: string): Trait {
  for (const [re, text] of TRAITS) {
    const m = re.exec(line);
    if (m) return { pct: `${m[1]}%`, text };
  }
  const top = /^Top (skills|subagents|plugins|MCP servers): (.+)$/.exec(line);
  if (top) {
    const list = top[2].split(", ").map(item => {
      const more = /^\+(\d+) more$/.exec(item);
      return more ? i18nText("另有 {p0} 个", { p0: more[1] }) : item;
    });
    return { text: TOPS[top[1]], list };
  }
  const other = /^(\d+)% of your usage (.+)$/.exec(line);
  if (other) return { pct: `${other[1]}%`, text: () => other[2] };
  return { text: () => line };
}

function periodHead(line: string): (() => string) | null {
  const m = /^Last (24h|7d) · (\d+) requests? · (\d+) sessions?$/.exec(line);
  if (m) {
    const span = m[1];
    return () => i18nText(span === "24h" ? "近 24 小时 · {p0} 次请求 · {p1} 个会话" : "近 7 天 · {p0} 次请求 · {p1} 个会话", { p0: m[2], p1: m[3] });
  }
  return /^Last \S+ · /.test(line) ? () => line : null;
}

/** 拆开 CLI 的文字回答：用的是订阅还是超额、用量构成，以及认不出的其他行 */
export function parseUsageText(text: string) {
  const lines = text.replace(/\x1b\[[0-9;]*m/g, "").split("\n");
  let plan = "";
  const periods: Period[] = [];
  const notes: string[] = [];
  let behaviors = false;
  for (const raw of lines) {
    const line = raw.trim();
    if (!line) continue;
    if (/^You are currently using your overages\b/.test(line)) { plan = "overage"; continue; }
    if (/^You are currently using your subscription\b/.test(line)) { plan = "subscription"; continue; }
    if (/^Current (session|week\b[^:]*):\s*\d+% used\b/.test(line)) continue; // 进度条已经画了
    if (line === "What's contributing to your limits usage?") { behaviors = true; continue; }
    if (behaviors && /^(Approximate, based on local sessions|Behaviors are independent characteristics)\b/.test(line)) continue;
    const head = behaviors ? periodHead(line) : null;
    if (head) { periods.push({ head, traits: [] }); continue; }
    if (behaviors && periods.length && /^\s{2}/.test(raw)) { periods[periods.length - 1].traits.push(trait(line)); continue; }
    notes.push(line);
  }
  return { plan, periods, notes };
}

function behaviorsBlock(periods: Period[]) {
  const box = el("div", "usage-traits");
  const title = el("div", "usage-traits-title");
  setText(title, "用量构成");
  const note = el("div", "usage-traits-note");
  setText(note, "按这个工作空间里的本地会话估算，不含其他设备与 claude.ai；各项是独立特征，加起来不等于 100%。");
  box.append(title, note);
  for (const period of periods) {
    const block = el("div", "usage-period");
    const head = el("div", "usage-period-head");
    setTextRender(head, period.head);
    block.append(head);
    for (const item of period.traits) {
      const row = el("div", "usage-trait");
      const pct = el("span", "usage-trait-pct mono", item.pct || "");
      const text = el("span", item.list ? "usage-trait-label" : "usage-trait-text");
      setTextRender(text, item.text);
      row.append(pct, text);
      if (item.list) {
        const tags = el("span", "usage-trait-tags");
        for (const name of item.list) tags.append(el("span", "usage-tag mono", name));
        row.append(tags);
      }
      block.append(row);
    }
    box.append(block);
  }
  return box;
}

export function usageCard(report: UsageReport, text: string) {
  const card = el("div", "msg agent usage-card");
  const parsed = parseUsageText(text);
  const head = el("div", "usage-card-head");
  const title = el("div", "usage-card-title");
  setText(title, "套餐用量");
  head.append(title);
  if (parsed.plan) {
    const sub = el("div", "usage-card-sub");
    setText(sub, parsed.plan === "overage" ? "正在使用超额额度，套餐额度重置后自动切回" : "正在使用订阅套餐额度");
    head.append(sub);
  }
  card.append(head);

  const limits = el("div", "usage-limits");
  for (const l of (report.rate_limits?.limits || []).filter(validLimit)) {
    const pct = Math.floor(l.percent!); // 与 CLI 一致向下取整，没满就不显示 100%
    limits.append(meter(limitLabel(l), l.percent!, () => i18nText("已用 {p0}%", { p0: String(pct) }), () => resetText(l.resets_at), l.severity));
  }
  const extra = report.rate_limits?.extra_usage;
  const extraMeter = extra && typeof extra === "object" ? extraRow(extra) : null;
  if (extraMeter) limits.append(extraMeter);
  card.append(limits);

  if (parsed.periods.length) card.append(behaviorsBlock(parsed.periods));
  if (parsed.notes.length) card.append(el("div", "usage-note", parsed.notes.join("\n")));
  return card;
}
