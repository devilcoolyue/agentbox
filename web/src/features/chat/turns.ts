import type { ChatTurnCost, HistoryEntry } from "../../types.js";
import { answerFooter, observeAnswer } from "../../chat-footer.js";
import type { AnswerContext } from "../../chat-footer.js";
import { renderEntry } from "../../chat-render.js";
import { agentIcon } from "../../brand.js";

type Footer = ReturnType<typeof answerFooter>;
const footers = new WeakMap<HTMLElement, Footer>();

/* Agent 回合分组：两条用户消息之间的 agent 输出（文字/工具行/思考…）
 * 归入同一个 .turn 容器，左侧挂品牌头像（OpenWebUI 式对话流）。
 * 用户消息与分割线打断分组；容器被清空后旧回合不在其中，自动重开。
 * 实时对话流一个实例；向上加载的较早一页另用一个实例写进游离容器，再整体插到顶部。 */
export class TurnWriter {
  body: HTMLElement | null = null; // 当前回合的内容列（.turn-body）
  footer: Footer | null = null;
  context: AnswerContext = {};
  constructor(private log: () => HTMLElement, private agent: () => string) {}

  reset() {
    this.body = null;
    this.footer = null;
    this.context = {};
  }

  place(node: HTMLElement) {
    if (node.classList.contains("user") || node.classList.contains("chat-divider")) {
      this.reset();
      this.log().appendChild(node);
      return;
    }
    const body = this.turnBody(), footer = this.footer!;
    if (node.classList.contains("result") && body.querySelector(".msg.agent")) footer.setReceipt(node);
    else body.insertBefore(node, footer.el);
    footer.update();
  }

  private turnBody() {
    const log = this.log();
    if (this.body && log.contains(this.body)) return this.body;
    const turn = document.createElement("div");
    turn.className = "turn";
    const av = document.createElement("span");
    av.className = "turn-avatar";
    av.appendChild(agentIcon(this.agent(), 24));
    const body = document.createElement("div");
    body.className = "turn-body";
    this.footer = answerFooter(body, this.context);
    footers.set(body, this.footer);
    body.appendChild(this.footer.el);
    turn.append(av, body);
    log.appendChild(turn);
    this.body = body;
    return body;
  }
}

/** 按历史记录重建对话流；place 决定节点落在哪（实时流还要处理贴底与执行指示）。 */
export function replayHistory(writer: TurnWriter, entries: HistoryEntry[],
  costs: Record<string, ChatTurnCost> | undefined, place: (node: HTMLElement) => void) {
  for (const raw of entries) {
    if (raw.kind === "event") observeAnswer(writer.context, raw.event, raw.ts);
    for (const n of renderEntry(raw)) place(n);
    if (raw.kind === "user" || raw.kind === "turn_context") {
      writer.context.turn = raw.turn;
      writer.context.cost = raw.turn ? costs?.[raw.turn.id] : undefined;
    }
    writer.context.historical = true;
    if (raw.kind === "status" && !writer.context.ts) writer.context.ts = raw.ts;
    writer.footer?.update();
  }
}

/** 服务端只在超长回合里切页：较早一页以回合中段结尾时，把这段并进下方已显示的
 * 同一回合，免得一个回合出现两个头像。两个 .turn 之间没有用户消息就是同一回合，
 * 与一次性渲染的分组结果一致；回合的底栏、金额和上下文仍归下方那段。 */
export function joinTurn(older: Element | null, newer: Element | null) {
  if (!older?.classList.contains("turn") || !newer?.classList.contains("turn") || newer.classList.contains("working")) return;
  const from = older.querySelector<HTMLElement>(":scope > .turn-body");
  const to = newer.querySelector<HTMLElement>(":scope > .turn-body");
  if (!from || !to) return;
  const skip = footers.get(from)?.el, footer = footers.get(to);
  to.prepend(...[...from.children].filter(node => node !== skip));
  older.remove();
  // 回合完成章到达时这段还没有正文，当时留在了正文里；补上正文后照常收进详情
  const result = to.querySelector<HTMLElement>(":scope > .result");
  if (footer && result && to.querySelector(".msg.agent")) footer.setReceipt(result);
  footer?.update();
}
