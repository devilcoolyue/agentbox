/* 输入框工具栏里的「委派」按钮：和附件、语音一样是个图标按钮，开启时变色并角标显示跑腿个数。
 * 点开弹层里有总开关和跑腿名单；打开后，本条消息的主 Agent 可以把明确的子任务交给勾选的跑腿模型。
 * 跑腿名单与 Jev 开关按空间记在本浏览器（只是偏好，丢了就回到默认）；发送时冻结进聊天回执，
 * 服务端每次委派前仍会重新核对账号授权和跑腿标签。
 * 可选的跑腿随管理员的标签、账号授权和 Jev 设置变化，所以除了进入空间，保存模型设置、回到对话页、
 * 切回浏览器标签页和点开弹层时都静默重读一次：读取期间沿用旧选项，失败也不清空。 */
import { api } from "../../api.js";
import { agentName } from "../../brand.js";
import { setText, setTextRender, t } from "../../i18n.js";
import { S, bus } from "../../state.js";
import { setTip } from "../../tip.js";
import type { DelegationOption, DelegationOptions } from "../../types.js";
import type { DelegationOptions as WireOptions } from "../../contracts/v1.js";
import { $ } from "../../util.js";
import { riskLabel, taskTypeLabel, tierLabel } from "./labels.js";

interface Prefs { on: boolean; workers: string[]; jev: boolean }
const key = (o: DelegationOption) => o.account_id + "/" + o.model;
const prefsKey = (session: string) => "agentbox_delegation_" + session;

function readPrefs(session: string): Partial<Prefs> {
  try {
    const raw = JSON.parse(localStorage.getItem(prefsKey(session)) || "null");
    if (!raw || typeof raw !== "object") return {};
    return { on: raw.on === true, workers: Array.isArray(raw.workers) ? raw.workers.filter((w: unknown) => typeof w === "string") : undefined, jev: raw.jev === true };
  } catch { return {}; }
}
function writePrefs(session: string, prefs: Prefs) {
  try { localStorage.setItem(prefsKey(session), JSON.stringify(prefs)); } catch { /* 偏好写不进去只影响下次默认值 */ }
}

export function initDelegationComposer(hooks: { changed: () => void; supported: () => boolean }) {
  const lifetime = new AbortController();
  let session = "", options: DelegationOptions | null = null, abort: AbortController | null = null;
  let prefs: Prefs = { on: false, workers: [], jev: false };
  /** 本空间偏好的来源：首次读本机存储，之后随用户改动更新；存储写不进去时重读也不丢改动。
   * workers 缺省表示从没选过，名单里的跑腿全部勾选。 */
  let base: Partial<Prefs> = {};
  /** 对话页是否正显示在屏幕上；从别处回来时重读选项。 */
  let shown = false;
  const picker = $("chat-delegate-picker"), button = $<HTMLButtonElement>("chat-delegate"), count = $("chat-delegate-count"), pop = $("chat-delegate-pop");

  const available = () => !!options?.available && !!options.workers.length && hooks.supported() && S.delegationProtocol === 1;
  const chosen = () => (options?.workers || []).filter(o => prefs.workers.includes(key(o)));
  const active = () => prefs.on && chosen().length > 0;
  const isOpen = () => !pop.classList.contains("hidden");

  function open() { buildPop(); pop.classList.remove("hidden"); button.setAttribute("aria-expanded", "true"); void refresh(true); }
  function close() { pop.classList.add("hidden"); button.setAttribute("aria-expanded", "false"); }
  function save() { base = { ...prefs }; if (session) writePrefs(session, prefs); render(); hooks.changed(); }

  function stateText() {
    return active()
      ? t("委派已开启：{names}", { names: chosen().map(o => o.model_label || o.model).join("、") })
      : t("委派：未开启，点击选择跑腿模型");
  }

  function render() {
    const show = available();
    picker.hidden = button.hidden = !show;
    if (!show) { close(); return; }
    const on = active();
    button.classList.toggle("on", on);
    count.hidden = !on;
    count.textContent = on ? String(chosen().length) : "";
    setTip(button, stateText);
    button.setAttribute("aria-label", stateText());
    if (isOpen()) syncPop();
  }

  /** 开关与说明随勾选变化；名单本身不重建，免得勾选时焦点跳走。 */
  function syncPop() {
    const box = pop.querySelector<HTMLInputElement>(".dg-switch");
    const state = pop.querySelector<HTMLElement>(".dg-pop-state");
    if (!box || !state) return;
    box.checked = active();
    box.disabled = !chosen().length;
    state.textContent = !chosen().length ? t("先勾选至少一个跑腿模型")
      : active() ? t("本条消息起生效，{n} 个跑腿模型可用", { n: String(chosen().length) })
      : t("未开启：主 Agent 自己完成全部工作");
    pop.classList.toggle("off", !active());
  }

  function workerRow(o: DelegationOption) {
    const row = document.createElement("label");
    row.className = "dg-pop-worker";
    const box = Object.assign(document.createElement("input"), { type: "checkbox", checked: prefs.workers.includes(key(o)) });
    box.addEventListener("change", () => {
      prefs.workers = box.checked ? [...new Set([...prefs.workers, key(o)])] : prefs.workers.filter(k => k !== key(o));
      if (box.checked) prefs.on = true;
      save();
    });
    const text = document.createElement("span");
    text.className = "dg-pop-text";
    const name = Object.assign(document.createElement("span"), { className: "dg-pop-name", textContent: o.model_label || o.model });
    const meta = document.createElement("span");
    meta.className = "dg-pop-sub";
    setTextRender(meta, () => [agentName(o.agent), o.account_label, tierLabel(o.tags.tier), o.priced ? "" : t("未定价")].filter(Boolean).join(" · "));
    const can = document.createElement("span");
    can.className = "dg-pop-sub";
    setTextRender(can, () => t("可接：{list}", { list: (o.tags.can || []).map(taskTypeLabel).join("、") })
      + ((o.tags.avoid || []).length ? "；" + t("不接：{list}", { list: (o.tags.avoid || []).map(riskLabel).join("、") }) : ""));
    text.append(name, meta, can);
    if (o.tags.notes) text.append(Object.assign(document.createElement("span"), { className: "dg-pop-sub", textContent: o.tags.notes }));
    row.append(box, text);
    return row;
  }

  function buildPop() {
    if (!options) return;
    // 头部整行是开关的标签：点标题也能开关。复用模型管理里的滑块开关样式。
    const head = document.createElement("label");
    head.className = "dg-pop-top";
    const title = document.createElement("span");
    title.className = "dg-pop-title";
    const name = document.createElement("span");
    name.className = "dg-pop-head";
    setText(name, "委派给跑腿模型");
    const state = document.createElement("span");
    state.className = "dg-pop-state";
    title.append(name, state);
    const toggle = Object.assign(document.createElement("input"), { type: "checkbox", className: "mv-switch dg-switch" });
    toggle.setAttribute("role", "switch");
    toggle.addEventListener("change", () => { prefs.on = toggle.checked; save(); });
    head.append(title, toggle);
    const intro = document.createElement("p");
    intro.className = "dg-pop-intro";
    setText(intro, "开启后，主 Agent 负责规划、判断和验收，把读代码、搜索、批量修改、补测试这类活交给下面勾选的模型。跑腿直接修改当前空间的文件，主 Agent 核对后再汇报。");
    const list = document.createElement("div");
    list.className = "dg-pop-list";
    list.append(...options.workers.map(workerRow));
    pop.replaceChildren(head, intro, list);
    if (options.jev) {
      const jev = document.createElement("label");
      jev.className = "dg-pop-worker dg-pop-jev";
      const box = Object.assign(document.createElement("input"), { type: "checkbox", checked: prefs.jev });
      box.addEventListener("change", () => { prefs.jev = box.checked; save(); });
      const text = document.createElement("span");
      text.className = "dg-pop-text";
      const jevName = document.createElement("span");
      jevName.className = "dg-pop-name";
      setText(jevName, "用 Jev 辅助判断");
      const sub = document.createElement("span");
      sub.className = "dg-pop-sub";
      setText(sub, "标签筛选后再问 Jev：交给哪个跑腿，还是主 Agent 自己做更合适。");
      text.append(jevName, sub);
      jev.append(box, text);
      pop.append(jev);
    }
    const foot = document.createElement("p");
    foot.className = "dg-pop-foot";
    setTextRender(foot, () => t("每条消息最多委派 {n} 次；跑腿的花费单独记账，显示在对话里的委派卡片上。", { n: String(options?.max_calls || 8) }));
    pop.append(foot);
    syncPop();
  }

  /** silent：保留当前选项直到新结果到达（重读）；否则先清空（换空间）。 */
  async function refresh(silent = false) {
    const id = session;
    abort?.abort();
    if (!silent) { options = null; base = id ? readPrefs(id) : {}; render(); }
    if (!id || S.delegationProtocol !== 1) return;
    const controller = abort = new AbortController();
    try {
      const value = await api<DelegationOptions>(`/sessions/${encodeURIComponent(id)}/delegation/options`, { signal: AbortSignal.any([controller.signal, lifetime.signal]) });
      if (controller.signal.aborted || id !== session || !Array.isArray(value?.workers)) return;
      const changed = JSON.stringify(value) !== JSON.stringify(options);
      options = value;
      const keys = value.workers.map(key);
      prefs = {
        on: !!base.on,
        workers: base.workers ? base.workers.filter(k => keys.includes(k)) : keys,
        jev: base.workers ? !!base.jev && value.jev : !!value.jev_default,
      };
      render();
      // 名单变了才重建弹层，免得每次重读都打断正在勾选的人。
      if (changed && isOpen()) buildPop();
      hooks.changed();
    } catch { /* 读不到选项就不提供委派（重读失败沿用旧选项），普通对话不受影响 */ }
  }

  function onNavigation() {
    const now = S.view === "work" && S.tab === "chat" && !document.hidden;
    if (now && !shown && session) void refresh(!!options);
    shown = now;
  }

  button.addEventListener("click", () => {
    if (!options) return;
    if (isOpen()) close(); else open();
  }, { signal: lifetime.signal });
  document.addEventListener("click", event => {
    const target = event.target as Element;
    if (isOpen() && !target.closest("#chat-delegate-picker")) close();
  }, { signal: lifetime.signal });
  document.addEventListener("keydown", event => { if (event.key === "Escape" && isOpen()) { close(); button.focus(); } }, { signal: lifetime.signal });
  window.addEventListener("agentbox-language-change", render, { signal: lifetime.signal });
  // 在「模型管理」里改了跑腿标签或可用模型：本页保存后立刻重读。
  bus.addEventListener("models-updated", () => { if (session) void refresh(!!options); }, { signal: lifetime.signal });
  for (const type of ["navigation-changed", "view-changed"]) bus.addEventListener(type, onNavigation, { signal: lifetime.signal });
  document.addEventListener("visibilitychange", onNavigation, { signal: lifetime.signal });

  return {
    /** 每次加载这个空间的历史时调用；同一空间重读选项，保留弹层与勾选。 */
    enter(id: string) {
      if (id === session) { void refresh(!!options); return; }
      session = id; close(); void refresh();
    },
    leave() { abort?.abort(); session = ""; options = null; render(); },
    /** 本条消息冻结的委派选项；未开启或不可用时为 undefined（普通对话）。 */
    value(): WireOptions | undefined {
      if (!available() || !prefs.on) return;
      const workers = chosen().map(o => ({ account_id: o.account_id, model: o.model })).slice(0, options?.max_workers || 8);
      return workers.length ? { version: 1, workers, jev: prefs.jev && !!options?.jev } : undefined;
    },
    dispose() { lifetime.abort(); abort?.abort(); close(); },
  };
}
