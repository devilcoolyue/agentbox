import { setAttrRender, setTextRender, t as i18nText } from "./i18n.js";
import type { StorageQuotaSummary, User, UserStorageLimits } from "./types.js";
import { $, fmtBytes, toast, btnBusy, btnDone, isImeEnter } from "./util.js";
import { api } from "./api.js";
import { setTip } from "./tip.js";

/* 用户存储上限（管理员）：服务端启用了存储配额（storage_policy）时，建用户可指定
 * 数据/缓存上限，之后在用户行的「存储」里调。输入按 GB（1024 进制，同 fmtBytes），
 * 留空用默认值。默认值与总预算来自 /storage/quotas。 */

const GB = 1 << 30;
const MB = 1 << 20;

/** GB 数换成字节（按 MB 取整）：空串 = 用默认值（0），不合法返回 null */
export function parseGB(value: string): number | null {
  const v = value.trim();
  if (!v) return 0;
  if (!/^\d+(\.\d+)?$/.test(v)) return null;
  const bytes = Math.round(Number(v) * 1024) * MB;
  return bytes >= MB ? bytes : null;
}

const gbText = (bytes: number) => String(Math.round(bytes / GB * 100) / 100);

function budgetText(s: StorageQuotaSummary) {
  if (!s.budget_bytes) return i18nText("存储已分配 {p0}，未设总预算", { p0: fmtBytes(s.allocated_bytes) });
  return i18nText("存储预算：已分配 {p0} / {p1}，剩余 {p2}", {
    p0: fmtBytes(s.allocated_bytes), p1: fmtBytes(s.budget_bytes), p2: fmtBytes(Math.max(0, s.budget_bytes - s.allocated_bytes)),
  });
}

/* ---------------- 添加用户：两个可选上限 + 预算提示 ---------------- */

let summary: StorageQuotaSummary | null = null;

export function renderStorageAdd(s: StorageQuotaSummary | null) {
  summary = s?.enabled ? s : null;
  const data = $<HTMLInputElement>("user-new-data");
  const cache = $<HTMLInputElement>("user-new-cache");
  const note = $("user-storage-note");
  $("user-add-storage").classList.toggle("hidden", !summary);
  cache.classList.toggle("hidden", !summary?.cache_enabled);
  if (!summary) return;
  const cur = summary;
  setAttrRender(data, "placeholder", () => i18nText("数据上限 GB（默认 {p0}）", { p0: gbText(cur.default_data_bytes) }));
  setAttrRender(cache, "placeholder", () => i18nText("缓存上限 GB（默认 {p0}）", { p0: gbText(cur.default_cache_bytes) }));
  setTextRender(note, () => budgetText(cur) + (cur.error ? i18nText("（读取用量失败）") : ""));
  setTip(note, cur.error || null);
}

/** 添加用户请求里的上限字段；没启用配额时为空对象，输入不合法时提示并返回 null */
export function storageAddLimits(): { data_bytes?: number; cache_bytes?: number } | null {
  if (!summary) return {};
  const data = parseGB($<HTMLInputElement>("user-new-data").value);
  const cache = summary.cache_enabled ? parseGB($<HTMLInputElement>("user-new-cache").value) : 0;
  if (data === null || cache === null) {
    toast(i18nText("存储上限请填 GB 数（至少 1 MB），留空用默认值"), true);
    return null;
  }
  return { data_bytes: data, cache_bytes: cache };
}

export function clearStorageAdd() {
  $<HTMLInputElement>("user-new-data").value = "";
  $<HTMLInputElement>("user-new-cache").value = "";
}

/* ---------------- 用户行：存储标签 ---------------- */

function usedOf(used: number | undefined, limit: number) {
  return (used === undefined ? "—" : fmtBytes(used)) + " / " + fmtBytes(limit);
}

export function storageChip(st: UserStorageLimits) {
  const el = document.createElement("span");
  el.className = "u-storage mono";
  el.textContent = usedOf(st.data_used, st.data_bytes);
  const full = (used: number | undefined, limit: number) => used !== undefined && limit > 0 && used / limit >= 0.9;
  el.classList.toggle("hot", full(st.data_used, st.data_bytes) || full(st.cache_used, st.cache_bytes));
  setTip(el, () => {
    const parts = [i18nText("数据 {p0}", { p0: usedOf(st.data_used, st.data_bytes) }) + (st.data_custom ? "" : i18nText("（默认）"))];
    if (st.cache_bytes) parts.push(i18nText("缓存 {p0}", { p0: usedOf(st.cache_used, st.cache_bytes) }) + (st.cache_custom ? "" : i18nText("（默认）")));
    return parts.join(" · ");
  });
  return el;
}

/* ---------------- 调整上限弹窗 ---------------- */

const dlg = () => $<HTMLDialogElement>("dlg-user-storage");
let current: { user: User; done?: () => void } | null = null;

function setError(msg: string) {
  const el = $("us-error");
  el.textContent = msg;
  el.classList.toggle("hidden", !msg);
}

export function openUserStorage(user: User, s: StorageQuotaSummary | null, onDone?: () => void) {
  const st = user.storage;
  if (!st) return;
  current = { user, done: onDone };
  $("us-user").textContent = user.name;
  const data = $<HTMLInputElement>("us-data");
  const cache = $<HTMLInputElement>("us-cache");
  data.value = st.data_custom ? gbText(st.data_bytes) : "";
  cache.value = st.cache_custom ? gbText(st.cache_bytes) : "";
  const defData = s?.default_data_bytes ?? st.data_bytes;
  const defCache = s?.default_cache_bytes ?? st.cache_bytes;
  setAttrRender(data, "placeholder", () => i18nText("默认 {p0}", { p0: gbText(defData) }));
  setAttrRender(cache, "placeholder", () => i18nText("默认 {p0}", { p0: gbText(defCache) }));
  setTextRender($("us-data-used"), () => st.data_used === undefined ? "" : i18nText("已用 {p0}", { p0: fmtBytes(st.data_used) }));
  setTextRender($("us-cache-used"), () => st.cache_used === undefined ? "" : i18nText("已用 {p0}", { p0: fmtBytes(st.cache_used) }));
  $("us-cache-field").classList.toggle("hidden", !st.cache_bytes);
  const budget = $("us-budget");
  budget.classList.toggle("hidden", !s);
  if (s) {
    // 这个用户现在的上限也算在已分配里，改的时候能用回来
    const room = s.budget_bytes ? Math.max(0, s.budget_bytes - s.allocated_bytes + st.data_bytes + st.cache_bytes) : 0;
    setTextRender(budget, () => budgetText(s) + (s.budget_bytes ? i18nText("；该用户最多可设 {p0}（数据+缓存）", { p0: fmtBytes(room) }) : ""));
  }
  setError("");
  dlg().showModal();
  data.focus();
}

async function save() {
  if (!current) return;
  const { user, done } = current;
  const data = parseGB($<HTMLInputElement>("us-data").value);
  const cache = user.storage?.cache_bytes ? parseGB($<HTMLInputElement>("us-cache").value) : 0;
  if (data === null || cache === null) {
    setError(i18nText("存储上限请填 GB 数（至少 1 MB），留空用默认值"));
    return;
  }
  const btn = $<HTMLButtonElement>("us-save");
  btnBusy(btn, () => i18nText("保存中…"));
  try {
    await api("/users/" + encodeURIComponent(user.name) + "/storage", {
      method: "PUT", body: JSON.stringify({ data_bytes: data, cache_bytes: cache }),
    });
    current = null;
    dlg().close();
    toast(i18nText("已更新「{p0}」的存储上限", { p0: user.name }));
    done?.();
  } catch (e) {
    setError((e as Error).message);
  } finally {
    btnDone(btn);
  }
}

$("us-save").addEventListener("click", () => void save());
$("us-cancel").addEventListener("click", () => dlg().close());
$("us-close").addEventListener("click", () => dlg().close());
for (const id of ["us-data", "us-cache"]) {
  $(id).addEventListener("keydown", (e) => {
    if ((e as KeyboardEvent).key === "Enter" && !isImeEnter(e as KeyboardEvent)) { e.preventDefault(); void save(); }
  });
}
