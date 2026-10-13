import type { AgentEvent } from "../../types.js";
import { t as i18nText } from "../../i18n.js";

/* 后台任务（Bash 的 run_in_background 等）：模型这一段答完以后 CLI 仍在等它们，
 * 结束时把结果交回模型接着说。执行指示据此说明在等什么，而不是一直停在
 * 「生成回复…」；模型还在干活时只附带一个计数。 */
export function createBackground(changed: () => void) {
  const tasks = new Map<string, string>(); // task_id → 模型写的描述
  let answered = false; // 已有这一段的 result，回合只因后台任务还开着
  let waitingShown = false;
  const snapshot = () => (answered ? "1|" : "0|") + [...tasks.keys()].join(",");

  function observe(ev: AgentEvent | undefined) {
    if (!ev || typeof ev !== "object") return;
    const before = snapshot();
    if (ev.type === "result") answered = true;
    else if (ev.type === "assistant" || ev.type === "stream_event" || ev.type === "system" && ev.subtype === "init") answered = false;
    else if (ev.type === "system") {
      const id = typeof ev.task_id === "string" ? ev.task_id : "";
      switch (ev.subtype) {
        case "background_tasks_changed": // 带当前全部后台任务，以它为准
          tasks.clear();
          for (const task of Array.isArray(ev.tasks) ? ev.tasks : []) {
            if (task && typeof task.task_id === "string") tasks.set(task.task_id, String(task.description || ""));
          }
          break;
        case "task_started":
          if (id && ev.is_backgrounded === true) tasks.set(id, String(ev.description || ""));
          break;
        case "task_notification":
          tasks.delete(id);
          break;
        case "task_updated":
          if (ev.patch?.status && ev.patch.status !== "running") tasks.delete(id);
          break;
      }
    }
    if (snapshot() !== before) changed();
  }

  return {
    observe,
    reset() { tasks.clear(); answered = false; waitingShown = false; },
    /** 执行指示的阶段文案：只剩后台任务时说明在等哪个；等完交回「思考中」；其余不管 */
    label(): (() => string) | null {
      if (answered && tasks.size) {
        waitingShown = true;
        const [only] = tasks.values();
        return tasks.size === 1 && only ? () => i18nText("等待后台任务结束：{p0}", { p0: only })
          : () => i18nText("等待 {p0} 个后台任务结束…", { p0: String(tasks.size) });
      }
      if (waitingShown) { waitingShown = false; return () => i18nText("思考中…"); }
      return null;
    },
    /** 模型还在干活时，执行指示旁的计数 */
    badge(): (() => string) | null {
      if (answered || !tasks.size) return null;
      return () => i18nText("后台任务 {p0} 个运行中", { p0: String(tasks.size) });
    },
    names() { return [...tasks.values()].filter(Boolean); },
  };
}
