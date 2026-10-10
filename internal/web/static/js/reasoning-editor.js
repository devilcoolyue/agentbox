import { t as i18nText, htmlText as trHTML, setText, setTextRender } from "./i18n.js";
import { enhanceSelects, setSelectValue } from "./select.js";
import { allowedLevels, EFFORT_LABELS, BUDGETS } from "./reasoning.js";
import { api } from "./api.js";
import { askPrompt, toast } from "./util.js";
import { emit } from "./state.js";
import { refreshAll } from "./data.js";
import { decorateIcons } from "./icons.js";
export function reasoningLabel(r) {
    if (!r)
        return i18nText("自动识别");
    if (r.support === "unsupported")
        return i18nText("不支持调整");
    if (r.support === "unknown")
        return i18nText("支持情况未知");
    return (r.control === "budget" ? i18nText("思考预算") : i18nText("推理强度")) + " · " + (r.levels || []).join(" / ");
}
// undefined = inherit/discover, null = cancel. Unknown is an explicit override.
export function editReasoning(agent, title, current) {
    return new Promise(resolve => {
        const dlg = document.createElement("dialog");
        dlg.className = "reasoning-dialog";
        dlg.innerHTML = `<form><div class="dlg-head"><h2></h2></div><div class="dlg-body">
      <label><span data-i18n="支持范围">${trHTML("支持范围")}</span><select name="support"><option value="auto" data-i18n="自动识别 / 跟随上级配置">${trHTML("自动识别 / 跟随上级配置")}</option><option value="unknown" data-i18n="支持情况未知">${trHTML("支持情况未知")}</option><option value="supported" data-i18n="支持调整">${trHTML("支持调整")}</option><option value="unsupported" data-i18n="不支持调整">${trHTML("不支持调整")}</option></select></label>
      <label data-control><span data-i18n="调整方式">${trHTML("调整方式")}</span><select name="control"><option value="effort" data-i18n="推理强度（原生档位）">${trHTML("推理强度（原生档位）")}</option><option value="budget" data-i18n="思考预算（旧模式）">${trHTML("思考预算（旧模式）")}</option></select></label>
      <fieldset data-levels><legend><span data-i18n="允许的档位">${trHTML("允许的档位")}</span></legend><div></div></fieldset>
      <p class="field-hint"><span data-i18n="按当前接入服务的实际支持范围配置。跟随默认并不等于关闭推理；未知模型允许用户手动尝试。旧预算模式要求模型支持固定 thinking 预算。">${trHTML("按当前接入服务的实际支持范围配置。跟随默认并不等于关闭推理；未知模型允许用户手动尝试。旧预算模式要求模型支持固定 thinking 预算。")}</span></p>
      <p role="alert"></p></div><div class="dlg-actions"><button type="button" class="btn" data-cancel data-icon="close"><span class="action-label" data-i18n="取消">${trHTML("取消")}</span></button><button type="submit" class="btn btn-primary" data-icon="save"><span class="action-label" data-i18n="保存">${trHTML("保存")}</span></button></div></form>`;
        dlg.querySelector("h2").textContent = title;
        const form = dlg.querySelector("form");
        const support = form.elements.namedItem("support");
        const control = form.elements.namedItem("control");
        if (agent !== "claude")
            control.querySelector('[value="budget"]').remove();
        setSelectValue(support, current?.support || "auto");
        setSelectValue(control, current?.control || (agent === "claude" ? "budget" : "effort"));
        const render = () => {
            dlg.querySelector("[data-control]").classList.toggle("hidden", !["supported", "unknown"].includes(support.value));
            const field = dlg.querySelector("[data-levels]");
            field.hidden = support.value !== "supported";
            const box = field.querySelector("div");
            box.replaceChildren(...allowedLevels(agent, control.value).map(level => {
                const label = document.createElement("label");
                label.className = "check";
                const check = document.createElement("input");
                check.type = "checkbox";
                check.name = "level";
                check.value = level;
                check.checked = current?.control === control.value && !!current.levels?.includes(level);
                const caption = document.createTextNode("");
                const budget = control.value === "budget";
                setTextRender(caption, () => " " + EFFORT_LABELS[level] + (budget ? ` (${BUDGETS[level].toLocaleString("en-US")} tokens)` : EFFORT_LABELS[level].toLowerCase() === level ? "" : ` (${level})`));
                label.append(check, caption);
                return label;
            }));
        };
        support.addEventListener("change", render);
        control.addEventListener("change", render);
        render();
        let result = null;
        dlg.addEventListener("close", () => { dlg.remove(); resolve(result); }, { once: true });
        dlg.querySelector("[data-cancel]").addEventListener("click", () => dlg.close());
        form.addEventListener("submit", event => {
            event.preventDefault();
            const levels = [...form.querySelectorAll('input[name="level"]:checked')].map(x => x.value);
            if (support.value === "supported" && !levels.length) {
                setText(dlg.querySelector('[role="alert"]'), "至少选择一个档位");
                return;
            }
            result = support.value === "auto" ? undefined : {
                support: support.value,
                ...(support.value !== "unsupported" ? { control: control.value } : {}),
                ...(support.value === "supported" ? { levels } : {}),
            };
            dlg.close();
        });
        document.body.append(dlg);
        decorateIcons(dlg);
        enhanceSelects(dlg);
        dlg.showModal();
    });
}
export async function editAccountReasoning(account) {
    try {
        const accounts = await api("/accounts");
        const current = accounts.find(a => a.id === account.id);
        if (!current)
            throw new Error(i18nText("账号已不存在"));
        const model = await askPrompt({ get title() { return current.label + i18nText(" · 模型能力"); }, get label() { return i18nText("模型 ID"); }, get hint() { return i18nText("为这个账号覆盖模型能力。已配置：") + (Object.keys(current.model_reasoning || {}).join("、") || i18nText("无")); }, validate: value => /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}(\[1m\])?$/.test(value.trim()) ? "" : i18nText("请输入有效的模型 ID") });
        if (model === null)
            return;
        const id = model.trim();
        const policy = await editReasoning(current.type, current.label + " · " + id, current.model_reasoning?.[id]);
        if (policy === null)
            return;
        // Read again so editing one model does not overwrite concurrent changes
        // to other entries that happened while the dialog was open.
        const fresh = (await api("/accounts")).find(a => a.id === account.id);
        if (!fresh)
            throw new Error(i18nText("账号已不存在"));
        const next = { ...fresh.model_reasoning };
        if (policy)
            next[id] = policy;
        else
            delete next[id];
        await api(`/accounts/${encodeURIComponent(account.id)}`, { method: "PATCH", body: JSON.stringify({ model_reasoning: next }) });
        await refreshAll();
        emit("models-updated");
        toast(i18nText("已保存账号模型能力"));
    }
    catch (error) {
        toast(error.message, true);
    }
}
