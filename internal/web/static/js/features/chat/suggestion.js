/* 下一步提示：Claude Code 在回合结束后预测的下一句输入。只在输入框为空时
 * 作为占位文字出现，Tab 或「填入」按钮把它放进输入框，从不替用户发送。
 * 开关是此浏览器的偏好；关掉后新消息不再请求，已有提示也不再显示。 */
const KEY = "agentbox_chat_suggestions";
const MAX = 500;
export function suggestionsEnabled() { try {
    return localStorage.getItem(KEY) !== "off";
}
catch {
    return true;
} }
export function setSuggestionsEnabled(on) { try {
    if (on)
        localStorage.removeItem(KEY);
    else
        localStorage.setItem(KEY, "off");
}
catch { /* 存储不可用时只影响本页 */ } }
export function suggestionOf(ev) {
    if (ev?.type !== "prompt_suggestion" || typeof ev.suggestion !== "string")
        return "";
    const text = ev.suggestion.trim();
    return text.length <= MAX ? text : "";
}
/** 最近一个回合（最后一条用户消息之后）留下的提示；回合还没结束就没有。 */
export function latestSuggestion(entries) {
    for (let i = entries.length - 1; i >= 0; i--) {
        const e = entries[i];
        if (e.kind === "user")
            return "";
        const text = e.kind === "event" ? suggestionOf(e.event) : "";
        if (text)
            return text;
    }
    return "";
}
export function createSuggestion(hooks) {
    let text = "";
    const current = () => suggestionsEnabled() ? text : "";
    const render = () => hooks.button().classList.toggle("hidden", !current() || !!hooks.input().value);
    return {
        current,
        render,
        set(next) { if (next === text)
            return; text = next; hooks.changed(); render(); },
        refresh() { hooks.changed(); render(); },
        /** 输入框为空且有提示时填入并返回 true；其余情况让按键照常处理。 */
        accept() {
            const input = hooks.input(), value = current();
            if (!value || input.value)
                return false;
            input.value = value;
            input.setSelectionRange(value.length, value.length);
            hooks.filled();
            render();
            return true;
        },
    };
}
