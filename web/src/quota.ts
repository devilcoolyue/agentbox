/* quota：额度的展示与管理。
 *
 * 金额一律以「微美元」（USD × 1e6 的整数）在前后端之间传递，前端只在显示的
 * 最后一步除 1e6。别把它当美元浮点数往回传：服务端按整数记账，浮点绕一圈
 * 回来会把分账算歪。
 *
 * 管理员在「系统设置 → 用户管理」每行进这个弹窗；普通用户只在侧栏看到自己的
 * 剩余额度（服务端 /me 下发，没开额度就不显示这一条）。 */
"use strict";

import { S, bus } from "./state.js";
import type { CreditResult, LedgerEntry, Quota, QuotaDetail } from "./types.js";
import { $, toast, btnBusy, btnDone, askConfirm } from "./util.js";
import { api } from "./api.js";
import { setTip } from "./tip.js";

/* 微美元 → 给人看的金额。默认四位小数：一个便宜回合只有几百微美元，
 * 两位小数会全部显示成 $0.00，看不出扣没扣。负号放 $ 前面。 */
export function fmtUSD(micro: number | undefined, digits = 4) {
  const n = (micro || 0) / 1e6;
  const s = "$" + Math.abs(n).toFixed(digits);
  return n < 0 ? "-" + s : s;
}

/** 额度的三种模式：不限额 / 只计不拦 / 超支拦截 */
type QuotaMode = "off" | "track" | "block";

/* 三种模式对应服务端的两个开关：没有额度行 = 不限额；有行 + enforced 决定拦不拦。 */
function modeOf(q: Quota | null): QuotaMode {
  if (!q || !q.metered) return "off";
  return q.enforced ? "block" : "track";
}

/* ---------------- 用户列表里的额度标签 ---------------- */

/* 给用户管理的每一行做一个额度小标签。没开额度的显示「不限额」，
 * 被拦住的标红——管理员扫一眼就知道谁停在门外。 */
export function quotaChip(q: Quota | null | undefined) {
  const el = document.createElement("span");
  el.className = "u-quota";
  if (!q || !q.metered) {
    el.classList.add("off");
    el.textContent = "不限额";
    return el;
  }
  el.textContent = fmtUSD(q.balance_micro_usd, 2);
  if (q.blocked) {
    el.classList.add("blocked");
    setTip(el, "余额已用完，该用户无法发起新对话");
  } else if (!q.enforced) {
    el.classList.add("track");
    setTip(el, "只计不拦：照常扣减，见底也不阻止");
  }
  return el;
}

/* ---------------- 侧栏：自己的剩余额度 ---------------- */

export function renderMyQuota() {
  const box = $("my-quota");
  const usage = $("btn-usagelog");
  const q = S.quota;
  usage.classList.toggle("quota-blocked", !!q?.metered && !!q.blocked);
  if (!q || !q.metered) {
    box.classList.add("hidden");
    setTip(usage, "使用记录");
    usage.setAttribute("aria-label", "使用记录");
    return;
  }
  box.classList.remove("hidden");
  const num = $("my-quota-num");
  num.textContent = fmtUSD(q.balance_micro_usd, 2);
  num.classList.toggle("empty", !!q.blocked);
  setTip(box, q.blocked
    ? "额度已用完，无法发起新对话，请联系管理员充值"
    : "剩余额度 " + fmtUSD(q.balance_micro_usd));
  box.setAttribute("aria-label", box.dataset.tip!);
  setTip(usage, "使用记录 · " + box.dataset.tip!);
  usage.setAttribute("aria-label", "使用记录，" + box.dataset.tip!);
}

// refreshAll 每轮都会带回自己的额度（回合收尾也会调它），这里跟着重画。
bus.addEventListener("data-updated", renderMyQuota);

/* ---------------- 管理弹窗 ---------------- */

const dlg = () => $<HTMLDialogElement>("dlg-quota");
let curUser = "";     // 当前弹窗操作的用户
let curQuota: Quota | null = null;  // 最近一次拉到的额度状态
let doneCb: (() => void) | null = null; // 关掉弹窗时刷新用户列表用
let dirty = false;    // 这次打开期间有没有真的改动过

/* 打开某个用户的额度弹窗。onDone 只在确实改动过之后、关窗时调用一次——
 * 只是进来看一眼不该触发整张列表重绘。 */
export async function openQuota(user: string, onDone?: () => void) {
  curUser = user;
  curQuota = null;
  doneCb = onDone || null;
  dirty = false;
  $("q-user").textContent = user;
  $<HTMLInputElement>("q-amount").value = "";
  $<HTMLInputElement>("q-note").value = "";
  $("q-balance").textContent = "…";
  $("q-totals").textContent = "";
  $("q-ledger").replaceChildren();
  dlg().showModal();
  await load();
}

async function load() {
  let data: QuotaDetail;
  try {
    data = await api<QuotaDetail>("/users/" + encodeURIComponent(curUser) + "/quota");
  } catch (e) {
    toast("读取额度失败：" + (e as Error).message, true);
    return;
  }
  curQuota = data.quota;
  render(data.quota, data.ledger || []);
}

function render(q: Quota, ledger: LedgerEntry[]) {
  const bal = $("q-balance");
  bal.textContent = q.metered ? fmtUSD(q.balance_micro_usd) : "不限额";
  bal.classList.toggle("neg", q.metered && q.balance_micro_usd <= 0);

  $("q-totals").textContent = q.metered
    ? "已充 " + fmtUSD(q.granted_micro_usd, 2) + " · 已花 " + fmtUSD(q.spent_micro_usd, 2)
    : "该用户不受额度限制，用量仍在记账";

  const mode = modeOf(q);
  for (const r of dlg().querySelectorAll<HTMLInputElement>('input[name="q-mode"]')) r.checked = r.value === mode;

  const box = $("q-ledger");
  box.replaceChildren();
  if (!ledger.length) {
    const empty = document.createElement("div");
    empty.className = "q-empty";
    empty.textContent = "还没有流水";
    box.appendChild(empty);
    return;
  }
  for (const e of ledger) box.appendChild(ledgerRow(e));
}

const REASON: Record<string, string> = { grant: "充值", spend: "消耗", adjust: "冲正" };

function ledgerRow(e: LedgerEntry) {
  const row = document.createElement("div");
  row.className = "q-led-row";

  const ts = document.createElement("span");
  ts.className = "q-led-ts mono";
  ts.textContent = new Date(e.ts).toLocaleString("zh-CN", {
    month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit",
  });

  const reason = document.createElement("span");
  reason.className = "q-led-reason " + e.reason;
  reason.textContent = REASON[e.reason] || e.reason;

  const delta = document.createElement("span");
  delta.className = "q-led-delta mono " + (e.delta_micro_usd < 0 ? "out" : "in");
  delta.textContent = (e.delta_micro_usd > 0 ? "+" : "") + fmtUSD(e.delta_micro_usd);

  const after = document.createElement("span");
  after.className = "q-led-after mono";
  after.textContent = "余 " + fmtUSD(e.balance_after);

  const note = document.createElement("span");
  note.className = "q-led-note";
  // 消耗流水的 note 存的是模型名；充值存管理员填的备注，后面带上操作人。
  note.textContent = e.note || "";
  if (e.actor) note.textContent += (e.note ? " · " : "") + e.actor;
  setTip(note, note.textContent);

  row.append(ts, reason, delta, after, note);
  return row;
}

/* ---------------- 交互 ---------------- */

$("q-close").addEventListener("click", () => dlg().close());

dlg().addEventListener("close", () => {
  if (dirty && doneCb) doneCb();
  doneCb = null;
  dirty = false;
});

for (const radio of document.querySelectorAll<HTMLInputElement>('#dlg-quota input[name="q-mode"]')) {
  radio.addEventListener("change", async () => {
    const mode = radio.value as QuotaMode;
    if (mode === modeOf(curQuota)) return;
    // 解除限额会丢掉余额（账本保留），这一步不可逆，先问一声。
    if (mode === "off" && curQuota && curQuota.metered) {
      const ok = await askConfirm("解除「" + curUser + "」的额度限制？", {
        title: "解除限额",
        hint: "当前余额 " + fmtUSD(curQuota.balance_micro_usd) +
          " 将被清空（历史流水保留）。以后重新开启额度会从 0 开始。",
        okLabel: "解除", danger: true,
      });
      if (!ok) {
        for (const r of dlg().querySelectorAll<HTMLInputElement>('input[name="q-mode"]')) {
          r.checked = r.value === modeOf(curQuota);
        }
        return;
      }
    }
    try {
      await api("/users/" + encodeURIComponent(curUser) + "/quota", {
        method: "PUT",
        body: JSON.stringify({ metered: mode !== "off", enforced: mode === "block" }),
      });
      toast("已切换为「" + { off: "不限额", track: "只计不拦", block: "超支拦截" }[mode] + "」");
      dirty = true;
      await load();
    } catch (e) {
      toast("切换失败：" + (e as Error).message, true);
      await load();
    }
  });
}

$("q-grant-btn").addEventListener("click", async () => {
  const raw = $<HTMLInputElement>("q-amount").value.trim();
  const usd = Number(raw);
  if (!raw || !Number.isFinite(usd) || usd === 0) {
    toast("请填写充值金额（美元），负数表示冲正", true);
    return;
  }
  const btn = $<HTMLButtonElement>("q-grant-btn");
  btnBusy(btn, "提交中…");
  try {
    // ref 是幂等键：同一次点击生成一次，网络重试或连点都不会重复入账。
    const ref = "ui-" + Date.now().toString(36) + "-" + Math.random().toString(36).slice(2, 8);
    const res = await api<CreditResult>("/users/" + encodeURIComponent(curUser) + "/credits", {
      method: "POST",
      body: JSON.stringify({ usd, note: $<HTMLInputElement>("q-note").value.trim(), ref }),
    });
    toast(res.applied
      ? (usd > 0 ? "已充值 " : "已冲正 ") + fmtUSD(Math.round(usd * 1e6), 2)
      : "该笔已入过账，未重复扣充");
    $<HTMLInputElement>("q-amount").value = "";
    $<HTMLInputElement>("q-note").value = "";
    dirty = true;
    await load();
  } catch (e) {
    toast("充值失败：" + (e as Error).message, true);
  } finally {
    btnDone(btn);
  }
});
