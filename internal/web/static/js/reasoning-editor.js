import { enhanceSelects, setSelectValue } from "./select.js";
import { allowedLevels, EFFORT_LABELS, BUDGETS } from "./reasoning.js";
import { api } from "./api.js";
import { askPrompt, toast } from "./util.js";
import { emit } from "./state.js";
import { refreshAll } from "./data.js";
import { decorateIcons } from "./icons.js";
export function reasoningLabel(r) {
    if (!r)
        return "自动识别";
    if (r.support === "unsupported")
        return "不支持调整";
    if (r.support === "unknown")
        return "支持情况未知";
    return (r.control === "budget" ? "思考预算" : "推理强度") + " · " + (r.levels || []).join(" / ");
}
// undefined = inherit/discover, null = cancel. Unknown is an explicit override.
export function editReasoning(agent, title, current) {
    return new Promise(resolve => {
        const dlg = document.createElement("dialog");
        dlg.className = "reasoning-dialog";
        dlg.innerHTML = `<form><h2></h2>
      <label>支持范围<select name="support"><option value="auto">自动识别 / 跟随上级配置</option><option value="unknown">支持情况未知</option><option value="supported">支持调整</option><option value="unsupported">不支持调整</option></select></label>
      <label data-control>调整方式<select name="control"><option value="effort">推理强度（原生档位）</option><option value="budget">思考预算（旧模式）</option></select></label>
      <fieldset data-levels><legend>允许的档位</legend><div></div></fieldset>
      <p class="field-hint">按当前接入服务的实际支持范围配置。跟随默认并不等于关闭推理；未知模型允许用户手动尝试。旧预算模式要求模型支持固定 thinking 预算。</p>
      <p role="alert"></p><div class="dlg-actions"><button type="button" class="btn" data-cancel data-icon="close">取消</button><button type="submit" class="btn btn-primary" data-icon="save">保存</button></div></form>`;
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
                label.append(check, document.createTextNode(" " + EFFORT_LABELS[level] + (control.value === "budget" ? `（${BUDGETS[level].toLocaleString("en-US")} tokens）` : `（${level}）`)));
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
                dlg.querySelector('[role="alert"]').textContent = "至少选择一个档位";
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
            throw new Error("账号已不存在");
        const model = await askPrompt({ title: current.label + " · 模型能力", label: "模型 ID", hint: "为这个账号覆盖模型能力。已配置：" + (Object.keys(current.model_reasoning || {}).join("、") || "无"), validate: value => /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}(\[1m\])?$/.test(value.trim()) ? "" : "请输入有效的模型 ID" });
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
            throw new Error("账号已不存在");
        const next = { ...fresh.model_reasoning };
        if (policy)
            next[id] = policy;
        else
            delete next[id];
        await api(`/accounts/${encodeURIComponent(account.id)}`, { method: "PATCH", body: JSON.stringify({ model_reasoning: next }) });
        await refreshAll();
        emit("models-updated");
        toast("已保存账号模型能力");
    }
    catch (error) {
        toast(error.message, true);
    }
}
