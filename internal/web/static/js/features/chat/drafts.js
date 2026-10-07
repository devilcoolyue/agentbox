import { S } from "../../state.js";
import { $ } from "../../util.js";
import { api } from "../../api.js";
import { setTextRender, t } from "../../i18n.js";
import { DraftStore } from "./draft-store.js";
export function initComposerDrafts(hooks) {
    const owner = S.token, lifetime = new AbortController();
    const storage = new DraftStore(localStorage, sessionStorage, S.draftScope, crypto.randomUUID());
    const enabled = $("chat-draft-enabled");
    let context, active = true, applying = false, checking = false, notice = "", epoch = 0, validation;
    const owns = () => { try {
        return active && S.token === owner && localStorage.getItem("agentbox_token") === owner;
    }
    catch {
        return active && S.token === owner;
    } };
    const invalid = () => hooks.read().attachments.some(a => !a.path || a.valid === false);
    const render = () => {
        enabled.checked = storage.supported && storage.enabled;
        enabled.disabled = !storage.supported;
        $("chat-draft-check").disabled = checking || !context || !hooks.read().attachments.length;
        setTextRender($("chat-draft-status"), () => storage.error ? t("草稿暂未保存到本机，请复制文本或检查浏览器存储空间。") : !storage.supported ? t("此服务端暂不支持持久草稿隔离，内容仅保留在当前页面。") : !storage.enabled ? t("本机保存已关闭，内容仅保留在当前页面。") : checking ? t("正在检查草稿附件…") : notice ? t(notice) : t("草稿保存在此浏览器 7 天；退出登录会清除。"));
        $("chat-draft-status").classList.toggle("hidden", !(storage.error || !storage.supported || !storage.enabled || checking || notice));
        hooks.changed();
    };
    const save = () => {
        if (!owns() || !context || applying)
            return;
        if (!invalid())
            notice = "";
        storage.save(context.session, context.thread, hooks.read());
        render();
    };
    const validate = async () => {
        if (!owns() || !context)
            return false;
        const attachments = hooks.read().attachments, paths = attachments.filter(a => a.path).map(a => a.path);
        if (!attachments.length)
            return true;
        if (S.draftProtocol !== 1) {
            if (attachments.every(a => a.path && a.valid === true))
                return true;
            notice = "服务端不支持附件检查，请移除恢复的附件并重新上传。";
            render();
            return false;
        }
        validation?.abort();
        const controller = validation = new AbortController(), gen = epoch, session = context.session;
        checking = true;
        render();
        try {
            const result = paths.length ? await api(`/sessions/${encodeURIComponent(session)}/attachments/validate`, { method: "POST", body: JSON.stringify({ paths }), signal: AbortSignal.any([lifetime.signal, controller.signal, AbortSignal.timeout(8000)]) }) : { valid: [] };
            if (!owns() || gen !== epoch || controller.signal.aborted)
                return false;
            if (!Array.isArray(result.valid) || result.valid.length !== paths.length || result.valid.some(v => typeof v !== "boolean"))
                throw new Error("invalid attachment result");
            hooks.validated(paths, result.valid);
            notice = invalid() ? "部分草稿附件已失效或尚未上传，请移除后重新选择。" : "";
            return !invalid();
        }
        catch {
            if (owns() && gen === epoch && !controller.signal.aborted) {
                hooks.validated(paths, paths.map(() => false));
                notice = "无法确认草稿附件，请重试检查或移除后重新选择。";
            }
            return false;
        }
        finally {
            if (validation === controller) {
                validation = undefined;
                checking = false;
                render();
            }
        }
    };
    const leave = () => {
        $("chat-draft-options").open = false;
        save();
        ++epoch;
        validation?.abort();
        validation = undefined;
        checking = false;
        context = undefined;
        notice = "";
        applying = true;
        hooks.apply({ text: "", attachments: [] });
        applying = false;
        render();
    };
    enabled.addEventListener("change", () => { if (!owns())
        return; storage.setEnabled(enabled.checked); if (enabled.checked)
        save(); render(); window.dispatchEvent(new CustomEvent("chat-local-saving-changed", { detail: { enabled: storage.enabled } })); }, { signal: lifetime.signal });
    $("chat-draft-clear").addEventListener("click", () => { if (!owns())
        return; storage.clear(); storage.setEnabled(false); notice = ""; render(); window.dispatchEvent(new CustomEvent("chat-local-saving-changed", { detail: { enabled: false } })); }, { signal: lifetime.signal });
    $("chat-draft-check").addEventListener("click", () => void validate(), { signal: lifetime.signal });
    window.addEventListener("storage", event => { if (event.key?.includes("chat-draft.v1.") && owns())
        render(); }, { signal: lifetime.signal });
    document.addEventListener("pointerdown", event => { const details = $("chat-draft-options"); if (!details.contains(event.target))
        details.open = false; }, { signal: lifetime.signal });
    $("chat-draft-options").addEventListener("keydown", event => { if (event.key === "Escape") {
        event.stopPropagation();
        $("chat-draft-options").open = false;
        $("chat-draft-options").querySelector("summary").focus();
    } }, { signal: lifetime.signal });
    document.addEventListener("visibilitychange", () => { if (document.hidden)
        save(); }, { signal: lifetime.signal });
    render();
    return {
        save, leave, validate,
        blocked: () => checking || invalid(),
        async enter(session, thread) {
            if (!owns() || context?.session === session && context.thread === thread)
                return;
            leave();
            context = { session, thread };
            const value = storage.load(session, thread);
            applying = true;
            hooks.apply({ ...value, attachments: value.attachments.map(a => ({ ...a, valid: false })) });
            applying = false;
            notice = value.text || value.attachments.length ? "已恢复此空间、此对话的草稿。" : "";
            render();
            if (value.attachments.length)
                await validate();
        },
        dispose() {
            if (!active)
                return;
            if (S.token !== owner) {
                if (S.preserveChatCopiesOnSignout)
                    storage.forget();
                else
                    storage.clear();
            }
            else
                save();
            active = false;
            lifetime.abort();
            validation?.abort();
            context = undefined;
            applying = true;
            hooks.apply({ text: "", attachments: [] });
            applying = false;
            $("chat-draft-status").textContent = "";
            enabled.checked = false;
        },
    };
}
