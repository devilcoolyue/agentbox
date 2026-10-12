export function cleanWorkflow(value) {
    if (!value || typeof value !== "object" || Array.isArray(value))
        return;
    const v = value;
    if (v.version !== 1 || !['direct', 'reviewed'].includes(String(v.mode)) || Object.keys(v).some(key => key !== "version" && key !== "mode"))
        return;
    return { version: 1, mode: v.mode };
}
/** Absent workflow retains the original reviewed pipeline, including old drafts. */
export const isDirect = (view) => (view.configuration?.workflow || view.run?.request?.workflow)?.mode === "direct";
export const activeRoles = (view) => isDirect(view) ? ["executor"] : ["coordinator", "executor"];
export const completionLabel = (view) => isDirect(view) ? "执行完成，结果待应用" : "评审通过，结果待应用";
