import type { ReasoningCapability } from "./types.js";

export const BUDGETS: Record<string, number> = { low: 4096, medium: 13000, high: 24000, xhigh: 31999 };
export const EFFORT_LABELS: Record<string, string> = { none: "无推理", minimal: "最低", low: "轻度", medium: "中", high: "高", xhigh: "极高", max: "最大", ultra: "超高" };

export function defaultReasoning(agent: string): ReasoningCapability {
  return { support: "unknown", control: agent === "claude" ? "budget" : "effort" };
}
export function allowedLevels(agent: string, control?: string): string[] {
  if (agent === "claude") return control === "effort" ? ["low", "medium", "high", "xhigh", "max"] : Object.keys(BUDGETS);
  return ["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"];
}
export function canKeepEffort(previous: ReasoningCapability, next: ReasoningCapability, effort: string): boolean {
  return !effort || (next.support === "supported" && previous.control === next.control && !!next.levels?.includes(effort));
}
