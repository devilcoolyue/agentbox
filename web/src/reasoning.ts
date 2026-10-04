import { t as i18nText } from "./i18n.js";
import type { ReasoningCapability } from "./types.js";

export const BUDGETS: Record<string, number> = { low: 4096, medium: 13000, high: 24000, xhigh: 31999 };
export const EFFORT_LABELS: Record<string, string> = { get none() { return i18nText("无推理"); }, get minimal() { return i18nText("最低"); }, get low() { return i18nText("轻度"); }, get medium() { return i18nText("中"); }, get high() { return i18nText("高"); }, get xhigh() { return i18nText("极高"); }, get max() { return i18nText("最大"); }, get ultra() { return i18nText("超高"); } };

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
