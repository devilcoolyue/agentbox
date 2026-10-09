import { t as i18nText, setText, setTextRender } from "./i18n.js";
/* 模型管理「账号模型」里点价格打开的弹窗：只改一个模型的四档单价，或让它跟随价格目录。
 * 写的是同一张全局价目表（按模型计价，与账号无关），长上下文档位仍在「价格」标签页编辑。 */
import type { TokenRates } from "./types.js";
import { $, btnBusy, btnDone, toast } from "./util.js";
import { catalogName, catalogPrice, followCatalogPrice, followsCatalog, priceLookup, saveModelPrice } from "./pricing.js";

const FIELDS = ["input", "output", "cache_read", "cache_write"] as const;
const dialog = () => $<HTMLDialogElement>("dlg-model-price");
let key = "";
let busy = false;
let wired = false;

export const usd = (n: number) => "$" + (Number.isInteger(n) ? String(n) : String(+n.toPrecision(4)));

/** 四档单价的一行说明：输入 $5 · 输出 $25 · 缓存读 $0.5 · 缓存写 $10 */
export function ratesText(p: TokenRates): string {
  return [i18nText("输入"), i18nText("输出"), i18nText("缓存读"), i18nText("缓存写")]
    .map((label, i) => `${label} ${usd(p[FIELDS[i]])}`).join(" · ");
}

function setError(text: string) {
  $("mp-error").textContent = text;
  $("mp-error").classList.toggle("hidden", !text);
}

function setBusy(next: boolean) {
  busy = next;
  for (const id of ["mp-ok", "mp-follow", "mp-cancel"]) $<HTMLButtonElement>(id).disabled = next;
  for (const f of FIELDS) $<HTMLInputElement>("mp-" + f).disabled = next;
}

async function run(button: HTMLButtonElement, action: () => Promise<void>, done: string) {
  if (busy) return;
  setError("");
  btnBusy(button, () => i18nText("保存中…"));
  setBusy(true);
  try {
    await action();
    dialog().close();
    toast(done);
  } catch (error) {
    setError((error as Error).message);
  } finally {
    btnDone(button);
    setBusy(false);
  }
}

function submit(event: SubmitEvent) {
  event.preventDefault();
  const rates = {} as TokenRates;
  for (const f of FIELDS) {
    const text = $<HTMLInputElement>("mp-" + f).value.trim();
    const value = text ? Number(text) : 0;
    if (!Number.isFinite(value) || value < 0) { setError(i18nText("单价必须是不小于 0 的数字")); return; }
    rates[f] = value;
  }
  const target = key;
  void run($<HTMLButtonElement>("mp-ok"), () => saveModelPrice(target, rates), i18nText("已保存 {key} 的价格", { key: target }));
}

function wire() {
  if (wired) return;
  wired = true;
  $("mp-form").addEventListener("submit", e => submit(e as SubmitEvent));
  $("mp-follow").addEventListener("click", () => {
    const target = key;
    void run($<HTMLButtonElement>("mp-follow"), () => followCatalogPrice(target), i18nText("{key} 已跟随价格目录", { key: target }));
  });
  for (const id of ["mp-close", "mp-cancel"]) $(id).addEventListener("click", () => { if (!busy) dialog().close(); });
  dialog().addEventListener("cancel", e => { if (busy) e.preventDefault(); });
}

/** agent / model：账号模型；label：显示名称。 */
export function openModelPrice(agent: string, model: string, label: string) {
  const hit = priceLookup(agent, model);
  if (!hit) return;
  wire();
  // The row actually used; a model without one gets a row named without its
  // snapshot date, which covers its other snapshots too (same lookup order).
  key = hit.kind === "exact" || hit.kind === "dated" ? hit.key : model.replace(/-\d{8}$/, "");
  const k = key;
  setTextRender($("mp-title"), () => i18nText("{model} · 价格", { model: label }));
  setTextRender($("mp-desc"), () => hit.kind === "exact" ? i18nText("修改价目表里的 {key}。", { key: k })
    : hit.kind === "dated" ? i18nText("{model} 按 {key} 这一行计价，修改会影响这个模型的所有日期快照。", { model, key: k })
    : hit.kind === "fallback" ? i18nText("这个模型现在按 {agent} 兜底价计费。保存后在价目表里新增 {key}。", { agent: hit.key, key: k })
    : i18nText("这个模型现在没有价格，只记用量、不扣额度。保存后在价目表里新增 {key}。", { key: k }));
  const own = hit.kind === "exact" || hit.kind === "dated" ? hit.price : undefined;
  for (const f of FIELDS) $<HTMLInputElement>("mp-" + f).value = own ? String(own[f]) : "";
  const entry = catalogPrice(k), follows = followsCatalog(k);
  $("mp-catalog").classList.toggle("hidden", !entry);
  $("mp-follow").classList.toggle("hidden", !entry || follows);
  if (entry) {
    const source = catalogName();
    setTextRender($("mp-catalog-text"), () => (follows
      ? i18nText("当前跟随{source}：", { source }) : i18nText("{source} 的价格：", { source }))
      + ratesText(entry.price) + (follows ? i18nText("。手动保存后改为自定义价格。") : ""));
  } else {
    setText($("mp-catalog-text"), "");
  }
  setError("");
  setBusy(false);
  btnDone($<HTMLButtonElement>("mp-ok"));
  btnDone($<HTMLButtonElement>("mp-follow"));
  if (!dialog().open) dialog().showModal();
  $<HTMLInputElement>("mp-input").focus();
}
