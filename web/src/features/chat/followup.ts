/* 任务运行中发送的消息（跟进）：排队＝这一轮结束后作为新一轮执行；引导＝插进正在
 * 跑的这一轮，当前步骤结束后生效。默认方式是此浏览器的偏好，Ctrl/Cmd+Enter 用另一种。
 * 只有 Claude 能引导，Codex 一律排队；引导不进去时服务端也会退回排队。 */
export type FollowMode = "queue" | "steer";
const KEY = "agentbox_chat_followup";

export function followupDefault(): FollowMode {
  try { return localStorage.getItem(KEY) === "steer" ? "steer" : "queue"; } catch { return "queue"; }
}
export function setFollowupDefault(mode: FollowMode) {
  try { if (mode === "steer") localStorage.setItem(KEY, "steer"); else localStorage.removeItem(KEY); } catch { /* 存储不可用时只影响本页 */ }
}
/** 这次发送用哪种：alt 是按住 Ctrl/Cmd 的那一种；不能引导的 Agent 总是排队。 */
export function followMode(agent: string, alt = false): FollowMode {
  if (agent !== "claude") return "queue";
  const preferred = followupDefault();
  return alt ? (preferred === "queue" ? "steer" : "queue") : preferred;
}
