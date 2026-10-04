/* session-state：工作空间运行状态的文字与样式。侧栏、工作台头部、首页最近空间共用一套口径。
 * 休眠 = 空闲回收自动停机，发消息或开终端会自动唤醒；已停止 = 用户手动停的。 */
import { t as i18nText } from "./i18n.js";
import type { Session } from "./types.js";

export interface SessionStateView { cls: "run" | "idle" | "off"; label: string; tip: string; }

export function sessionState(sess: Session): SessionStateView {
  if (sess.status === "running") return { cls: "run", label: i18nText("运行中"), tip: i18nText("容器运行中") };
  if (sess.stop_reason === "idle") return { cls: "idle", label: i18nText("休眠"), tip: i18nText("空闲自动停机，发消息或打开终端会自动唤醒") };
  return { cls: "off", label: i18nText("已停止"), tip: i18nText("已停止，文件与对话仍保留；发消息或打开终端会自动启动") };
}
