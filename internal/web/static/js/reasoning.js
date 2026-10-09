export const BUDGETS = { low: 4096, medium: 13000, high: 24000, xhigh: 31999 };
/** 档位名与各家 API 一致，三种界面语言都直接显示英文。 */
export const EFFORT_LABELS = { none: "None", minimal: "Minimal", low: "Low", medium: "Medium", high: "High", xhigh: "Extra High", max: "Max", ultra: "Ultra" };
export function defaultReasoning(agent) {
    return { support: "unknown", control: agent === "claude" ? "budget" : "effort" };
}
export function allowedLevels(agent, control) {
    if (agent === "claude")
        return control === "effort" ? ["low", "medium", "high", "xhigh", "max"] : Object.keys(BUDGETS);
    return ["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"];
}
export function canKeepEffort(previous, next, effort) {
    return !effort || (next.support === "supported" && previous.control === next.control && !!next.levels?.includes(effort));
}
