/* 「系统设置 → 登录与活跃」：管理员查看每个用户的最后活跃时间与登录记录。
 * 接口只对管理员开放（server/login_activity.go），入口也只有管理员看得到。 */
import { api } from "../../api.js";
import { setText, setTextRender, t } from "../../i18n.js";
import { setSelectValue } from "../../select.js";
import { toast } from "../../toast.js";
import type { LoginEvent, LoginEvents, UserActivity } from "../../types.js";
import { $, fmtAgo, fmtDateTime, keepScroll } from "../../util.js";

const PAGE_SIZE = 50;

let page: AbortController | null = null;
let wired = false;
let events: LoginEvent[] = [];
let hasMore = false;
let loadingMore = false;

const ROLE_LABELS: Record<string, () => string> = {
  admin: () => t("管理员"),
  user: () => t("普通用户"),
};
const METHOD_LABELS: Record<string, () => string> = {
  password: () => t("密码"),
  desktop: () => t("桌面端配对"),
  "abox-link": () => t("abox-link 配对"),
};

// 顺序有讲究：Edge、Opera 的 UA 里也有 Chrome，Chrome 的 UA 里也有 Safari；
// iOS 的 UA 里也有 Mac OS X，Android 的 UA 里也有 Linux。
const BROWSERS: [RegExp, string][] = [
  [/Edg(?:e|A|iOS)?\//, "Edge"], [/OPR\//, "Opera"], [/Firefox\/|FxiOS\//, "Firefox"],
  [/Chrome\/|CriOS\//, "Chrome"], [/Safari\//, "Safari"],
];
const SYSTEMS: [RegExp, string][] = [
  [/Windows/, "Windows"], [/iPhone|iPad|iPod/, "iOS"], [/Android/, "Android"],
  [/Mac OS X|Macintosh/, "macOS"], [/CrOS/, "ChromeOS"], [/Linux/, "Linux"],
];
const firstMatch = (ua: string, table: [RegExp, string][]) => table.find(([re]) => re.test(ua))?.[1];

/** 从 User-Agent 里挑出读得懂的「浏览器 · 系统」，认不出就显示原文。 */
function describeClient(ua: string) {
  const browser = firstMatch(ua, BROWSERS), system = firstMatch(ua, SYSTEMS);
  if (!browser) return ua;
  return system ? `${browser} · ${system}` : browser;
}

/** 记录用绝对时间，都在几小时内的再带上相对时间，免得心算。 */
function timeCell(cell: HTMLTableCellElement, ms: number) {
  if (!ms) {
    cell.className = "note";
    setText(cell, "暂无记录");
    return;
  }
  setTextRender(cell, () => {
    const ago = fmtAgo(ms), full = fmtDateTime(ms);
    return Date.now() - ms < 86_400_000 ? `${full} · ${ago}` : full;
  });
}

function emptyRow(body: HTMLTableSectionElement, cols: number, text: string) {
  const cell = body.insertRow().insertCell();
  cell.colSpan = cols;
  cell.className = "note";
  setText(cell, text);
}

function renderUsers(users: UserActivity[]) {
  const body = $<HTMLTableSectionElement>("ua-users-tbody");
  body.replaceChildren();
  if (!users.length) return emptyRow(body, 5, "暂无记录");
  for (const u of users) {
    const row = body.insertRow();
    row.insertCell().textContent = u.name;
    const role = row.insertCell();
    setTextRender(role, () => ROLE_LABELS[u.role]?.() ?? u.role);
    timeCell(row.insertCell(), u.last_active_at);
    timeCell(row.insertCell(), u.last_login_at);
    row.insertCell().textContent = u.last_login_ip;
  }
}

function fillUserFilter(users: UserActivity[]) {
  const select = $<HTMLSelectElement>("ua-user");
  const current = select.value;
  const names = users.map(u => u.name).sort((a, b) => a.localeCompare(b));
  select.replaceChildren();
  const all = document.createElement("option");
  all.value = "";
  setText(all, "全部用户");
  select.append(all);
  for (const name of names) {
    const option = document.createElement("option");
    option.value = option.textContent = name;
    select.append(option);
  }
  setSelectValue(select, names.includes(current) ? current : "");
}

function renderEvents() {
  const body = $<HTMLTableSectionElement>("ua-events-tbody");
  body.replaceChildren();
  $("ua-more").classList.toggle("hidden", !hasMore);
  if (!events.length) return emptyRow(body, 5, "暂无记录");
  for (const e of events) {
    const row = body.insertRow();
    timeCell(row.insertCell(), e.at);
    row.insertCell().textContent = e.user;
    const method = row.insertCell();
    setTextRender(method, () => METHOD_LABELS[e.method]?.() ?? e.method);
    row.insertCell().textContent = e.ip;
    const client = row.insertCell();
    client.className = "ua-client"; // 认不出的 UA 显示原文，长度由客户端决定，不能把表格撑宽
    client.textContent = describeClient(e.user_agent);
    client.title = e.user_agent;
  }
}

async function loadUsers(signal: AbortSignal) {
  try {
    const { users } = await api<{ users: UserActivity[] }>("/user-activity", { signal });
    if (signal.aborted) return;
    renderUsers(users);
    fillUserFilter(users);
  } catch (e) {
    if (!signal.aborted) toast(t("读取用户活跃记录失败：") + (e as Error).message, true);
  }
}

/** 不带 before 是从头读；筛选用户变了就重置。列表按 id 翻页，新登录不会打乱后面几页。 */
async function loadEvents(signal: AbortSignal, more = false) {
  if (more && loadingMore) return; // 连点不能拿同一个游标读两遍，否则出现重复行
  loadingMore = more;
  const query = new URLSearchParams({ limit: String(PAGE_SIZE) });
  const user = $<HTMLSelectElement>("ua-user").value;
  if (user) query.set("user", user);
  if (more && events.length) query.set("before", String(events[events.length - 1].id));
  try {
    const v = await api<LoginEvents>("/login-events?" + query, { signal });
    if (signal.aborted) return;
    events = more ? [...events, ...v.events] : v.events;
    hasMore = v.has_more;
    keepScroll($("ua-events-tbody"), renderEvents);
  } catch (e) {
    if (!signal.aborted) toast(t("读取登录记录失败：") + (e as Error).message, true);
  } finally {
    if (more) loadingMore = false;
  }
}

/** 重新读取时中止上一轮；离开分区或换登录后迟到的响应不再写页面。 */
function restartPage(parent: AbortSignal) {
  page?.abort();
  page = new AbortController();
  return AbortSignal.any([parent, page.signal]);
}

export function openUserActivity(parent: AbortSignal) {
  const signal = restartPage(parent);
  void loadUsers(signal);
  void loadEvents(signal);
  if (wired) return;
  wired = true;
  $("ua-refresh").addEventListener("click", () => {
    const fresh = restartPage(parent);
    void loadUsers(fresh);
    void loadEvents(fresh);
  }, { signal: parent });
  $("ua-user").addEventListener("change", () => void loadEvents(restartPage(parent)), { signal: parent });
  $("ua-more").addEventListener("click", () => {
    if (page) void loadEvents(AbortSignal.any([parent, page.signal]), true);
  }, { signal: parent });
  parent.addEventListener("abort", () => { wired = false; page = null; });
}

export function leaveUserActivity() {
  page?.abort();
  page = null;
}
