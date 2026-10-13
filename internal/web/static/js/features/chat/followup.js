const KEY = "agentbox_chat_followup";
export function followupDefault() {
    try {
        return localStorage.getItem(KEY) === "steer" ? "steer" : "queue";
    }
    catch {
        return "queue";
    }
}
export function setFollowupDefault(mode) {
    try {
        if (mode === "steer")
            localStorage.setItem(KEY, "steer");
        else
            localStorage.removeItem(KEY);
    }
    catch { /* 存储不可用时只影响本页 */ }
}
/** 这次发送用哪种：alt 是按住 Ctrl/Cmd 的那一种；不能引导的 Agent 总是排队。 */
export function followMode(agent, alt = false) {
    if (agent !== "claude")
        return "queue";
    const preferred = followupDefault();
    return alt ? (preferred === "queue" ? "steer" : "queue") : preferred;
}
