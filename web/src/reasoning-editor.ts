import { t as i18nText, htmlText as trHTML, setAttrRender, setText, setTextRender } from "./i18n.js";
import { enhanceSelects, setSelectValue } from "./select.js";
import { allowedLevels, EFFORT_LABELS, BUDGETS } from "./reasoning.js";
import type { Account, ModelOption, ReasoningCapability } from "./types.js";
import { api } from "./api.js";
import { toast } from "./util.js";
import { emit } from "./state.js";
import { refreshAll } from "./data.js";
import { decorateIcons } from "./icons.js";

export function reasoningLabel(r?: ReasoningCapability): string {
  if (!r) return i18nText("自动识别");
  if (r.support === "unsupported") return i18nText("不支持调整");
  if (r.support === "unknown") return i18nText("支持情况未知");
  return (r.control === "budget" ? i18nText("思考预算") : i18nText("推理强度")) + " · " + (r.levels || []).join(" / ");
}

// undefined = inherit/discover, null = cancel. Unknown is an explicit override.
export function editReasoning(agent: string, title: string, current?: ReasoningCapability): Promise<ReasoningCapability | undefined | null> {
  return new Promise(resolve => {
    const dlg = document.createElement("dialog");
    dlg.className = "reasoning-dialog";
    dlg.innerHTML = `<form><div class="dlg-head"><h2></h2></div><div class="dlg-body">
      <label><span data-i18n="支持范围">${trHTML("支持范围")}</span><select name="support"><option value="auto" data-i18n="自动识别 / 跟随上级配置">${trHTML("自动识别 / 跟随上级配置")}</option><option value="unknown" data-i18n="支持情况未知">${trHTML("支持情况未知")}</option><option value="supported" data-i18n="支持调整">${trHTML("支持调整")}</option><option value="unsupported" data-i18n="不支持调整">${trHTML("不支持调整")}</option></select></label>
      <label data-control><span data-i18n="调整方式">${trHTML("调整方式")}</span><select name="control"><option value="effort" data-i18n="推理强度（原生档位）">${trHTML("推理强度（原生档位）")}</option><option value="budget" data-i18n="思考预算（旧模式）">${trHTML("思考预算（旧模式）")}</option></select></label>
      <fieldset data-levels><legend><span data-i18n="允许的档位">${trHTML("允许的档位")}</span></legend><div></div></fieldset>
      <p class="field-hint"><span data-i18n="按当前接入服务的实际支持范围配置。跟随默认并不等于关闭推理；未知模型允许用户手动尝试。旧预算模式要求模型支持固定 thinking 预算。">${trHTML("按当前接入服务的实际支持范围配置。跟随默认并不等于关闭推理；未知模型允许用户手动尝试。旧预算模式要求模型支持固定 thinking 预算。")}</span></p>
      <p role="alert"></p></div><div class="dlg-actions"><button type="button" class="btn" data-cancel data-icon="close"><span class="action-label" data-i18n="取消">${trHTML("取消")}</span></button><button type="submit" class="btn btn-primary" data-icon="save"><span class="action-label" data-i18n="保存">${trHTML("保存")}</span></button></div></form>`;
    dlg.querySelector("h2")!.textContent = title;
    const form = dlg.querySelector("form")!;
    const support = form.elements.namedItem("support") as HTMLSelectElement;
    const control = form.elements.namedItem("control") as HTMLSelectElement;
    if (agent !== "claude") control.querySelector('[value="budget"]')!.remove();
    setSelectValue(support, current?.support || "auto");
    setSelectValue(control, current?.control || (agent === "claude" ? "budget" : "effort"));
    const render = () => {
      dlg.querySelector<HTMLElement>("[data-control]")!.classList.toggle("hidden", !["supported", "unknown"].includes(support.value));
      const field = dlg.querySelector<HTMLFieldSetElement>("[data-levels]")!;
      field.hidden = support.value !== "supported";
      const box = field.querySelector("div")!;
      box.replaceChildren(...allowedLevels(agent, control.value).map(level => {
        const label = document.createElement("label");
        label.className = "check";
        const check = document.createElement("input");
        check.type = "checkbox"; check.name = "level"; check.value = level;
        check.checked = current?.control === control.value && !!current.levels?.includes(level);
        const caption = document.createTextNode("");
        const budget = control.value === "budget";
        setTextRender(caption, () => " " + EFFORT_LABELS[level] + (budget ? ` (${BUDGETS[level].toLocaleString("en-US")} tokens)` : EFFORT_LABELS[level].toLowerCase() === level ? "" : ` (${level})`));
        label.append(check, caption);
        return label;
      }));
    };
    support.addEventListener("change", render); control.addEventListener("change", render); render();
    let result: ReasoningCapability | undefined | null = null;
    dlg.addEventListener("close", () => { dlg.remove(); resolve(result); }, { once: true });
    dlg.querySelector("[data-cancel]")!.addEventListener("click", () => dlg.close());
    form.addEventListener("submit", event => {
      event.preventDefault();
      const levels = [...form.querySelectorAll<HTMLInputElement>('input[name="level"]:checked')].map(x => x.value);
      if (support.value === "supported" && !levels.length) { setText(dlg.querySelector('[role="alert"]')!, "至少选择一个档位"); return; }
      result = support.value === "auto" ? undefined : {
        support: support.value as ReasoningCapability["support"],
        ...(support.value !== "unsupported" ? { control: control.value as "effort" | "budget" } : {}),
        ...(support.value === "supported" ? { levels } : {}),
      };
      dlg.close();
    });
    document.body.append(dlg); decorateIcons(dlg); enhanceSelects(dlg); dlg.showModal();
  });
}

const MODEL_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}(\[1m\])?$/;
// 下拉里「手动填写」那一项的取值；含空格，不可能与合法模型 ID 撞上
const CUSTOM_MODEL = "custom model";

/* 选要覆盖能力的模型：已覆盖的、系统模型列表里的直接选，列表外的才手填。
 * 只有没配自己模型列表的账号走这里（有列表的在「可用模型」里按模型设置），
 * 所以候选取系统模型列表。null = 取消。 */
function pickReasoningModel(account: Account, system: ModelOption[]): Promise<string | null> {
  return new Promise(resolve => {
    const dlg = document.createElement("dialog");
    dlg.className = "reasoning-dialog";
    dlg.innerHTML = `<form><div class="dlg-head"><h2></h2></div><div class="dlg-body">
      <label><span data-i18n="模型">${trHTML("模型")}</span><select name="model"></select></label>
      <label data-custom><span data-i18n="模型 ID">${trHTML("模型 ID")}</span><input type="text" name="custom" class="mono" maxlength="70" autocomplete="off" spellcheck="false" placeholder="${trHTML("例如 claude-opus-5")}" data-i18n-attrs="{&quot;placeholder&quot;:&quot;例如 claude-opus-5&quot;}"></label>
      <p class="field-hint"><span data-i18n="为这个账号覆盖模型的推理强度能力。不在系统模型列表里的模型，选「其他模型 ID…」手动填写。">${trHTML("为这个账号覆盖模型的推理强度能力。不在系统模型列表里的模型，选「其他模型 ID…」手动填写。")}</span></p>
      <p role="alert" class="login-error"></p></div><div class="dlg-actions"><button type="button" class="btn" data-cancel data-icon="close"><span class="action-label" data-i18n="取消">${trHTML("取消")}</span></button><button type="submit" class="btn btn-primary" data-icon="arrow-right"><span class="action-label" data-i18n="下一步">${trHTML("下一步")}</span></button></div></form>`;
    setTextRender(dlg.querySelector("h2")!, () => (account.label || account.id) + i18nText(" · 模型能力"));
    const form = dlg.querySelector("form")!;
    const model = form.elements.namedItem("model") as HTMLSelectElement;
    const custom = form.elements.namedItem("custom") as HTMLInputElement;
    const customRow = dlg.querySelector<HTMLElement>("[data-custom]")!;
    const error = dlg.querySelector<HTMLElement>('[role="alert"]')!;
    const labels = new Map(system.map(m => [m.id, m.label]));
    const caption = (id: string) => { const label = labels.get(id); return label && label !== id ? `${label} (${id})` : id; };
    const group = (label: string, options: HTMLOptionElement[]) => {
      if (!options.length) return;
      const el = document.createElement("optgroup");
      setAttrRender(el, "label", () => i18nText(label));
      el.append(...options);
      model.append(el);
    };
    const option = (value: string, text: () => string) => {
      const el = document.createElement("option");
      el.value = value;
      setTextRender(el, text);
      return el;
    };
    const overrides = account.model_reasoning || {};
    const configured = Object.keys(overrides).sort();
    group("已覆盖", configured.map(id => option(id, () => caption(id) + " · " + reasoningLabel(overrides[id]))));
    group("系统模型列表", system.filter(m => !(m.id in overrides) && MODEL_ID.test(m.id)).map(m => option(m.id, () => caption(m.id))));
    group("其他", [option(CUSTOM_MODEL, () => i18nText("其他模型 ID…"))]);
    const sync = () => {
      customRow.classList.toggle("hidden", model.value !== CUSTOM_MODEL);
      error.textContent = "";
    };
    model.addEventListener("change", () => { sync(); if (model.value === CUSTOM_MODEL) custom.focus(); });
    custom.addEventListener("input", () => { error.textContent = ""; });
    setSelectValue(model, model.options[0].value);
    sync();
    let result: string | null = null;
    dlg.addEventListener("close", () => { dlg.remove(); resolve(result); }, { once: true });
    dlg.querySelector("[data-cancel]")!.addEventListener("click", () => dlg.close());
    form.addEventListener("submit", event => {
      event.preventDefault();
      const id = model.value === CUSTOM_MODEL ? custom.value.trim() : model.value;
      if (!MODEL_ID.test(id)) { setText(error, "请输入有效的模型 ID"); custom.focus(); return; }
      result = id;
      dlg.close();
    });
    document.body.append(dlg); decorateIcons(dlg); enhanceSelects(dlg); dlg.showModal();
    if (model.value === CUSTOM_MODEL) custom.focus();
  });
}

/** system：账号没有自己的模型列表时沿用的系统模型列表，作为下拉候选 */
export async function editAccountReasoning(account: Account, system: ModelOption[] = []) {
    try {
      const accounts = await api<Account[]>("/accounts");
      const current = accounts.find(a => a.id === account.id);
      if (!current) throw new Error(i18nText("账号已不存在"));
      const id = await pickReasoningModel(current, system);
      if (id === null) return;
      const policy = await editReasoning(current.type, (current.label || current.id) + " · " + id, current.model_reasoning?.[id]);
      if (policy === null) return;
      // Read again so editing one model does not overwrite concurrent changes
      // to other entries that happened while the dialog was open.
      const fresh = (await api<Account[]>("/accounts")).find(a => a.id === account.id);
      if (!fresh) throw new Error(i18nText("账号已不存在"));
      const next = { ...fresh.model_reasoning };
      if (policy) next[id] = policy; else delete next[id];
      await api(`/accounts/${encodeURIComponent(account.id)}`, { method: "PATCH", body: JSON.stringify({ model_reasoning: next }) });
      await refreshAll(); emit("models-updated"); toast(i18nText("已保存账号模型能力"));
    } catch (error) { toast((error as Error).message, true); }
}
