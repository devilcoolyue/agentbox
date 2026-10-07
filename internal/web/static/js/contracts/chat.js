export const chatStates = ["accepted", "starting", "running", "completed", "failed", "interrupted", "uncertain", "reviewed", "abandoned", "deleted"];
/** Pure wire validation, independent of UI and local storage lifetimes. */
export function readChatReceipt(value, session, id) {
    const c = value;
    if (!c || typeof c !== "object" || c.session_id !== session || !(/^[a-f0-9]{32}$/).test(c.request_id) || id && c.request_id !== id || !chatStates.includes(c.state) || String(c.state) === "unconfirmed" || !Number.isSafeInteger(c.revision) || c.revision < 1 || typeof c.thread_id !== "string" || typeof c.turn_id !== "string" || typeof c.created_at !== "string" || typeof c.updated_at !== "string" || c.error_code !== undefined && typeof c.error_code !== "string")
        throw Error("invalid chat receipt");
    if (c.request !== null && (!c.request || typeof c.request.text !== "string" || typeof c.request.scope !== "string" || c.request.thread_id !== c.thread_id || typeof c.request.model !== "string" || typeof c.request.effort !== "string" || typeof c.request.effort_control !== "string"))
        throw Error("invalid chat receipt");
    if (c.request && (c.request.text.length > 1_048_576 || !(/^[a-f0-9]{64}$/).test(c.request.scope) || [c.request.model, c.request.effort, c.request.effort_control].some(v => v.length > 512) || c.request.attachments !== undefined && (!Array.isArray(c.request.attachments) || c.request.attachments.length > 64 || c.request.attachments.some(p => typeof p !== "string" || !/^\/shared\/\.(images|file)\/[A-Za-z0-9][A-Za-z0-9._-]{0,255}$/.test(p)))))
        throw Error("invalid chat receipt");
    if (c.request === null && !["abandoned", "deleted"].includes(c.state))
        throw Error("invalid chat receipt");
    return c;
}
