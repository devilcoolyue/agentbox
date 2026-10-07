import type { History } from "../../types.js";

type Result = ({state: "loaded" | "stale"} | {state: "failed"; error: unknown; timedOut: boolean}) & {current: () => boolean};
type Read = (path: string, signal: AbortSignal) => Promise<History>;

/** Owns requests and deadlines across reload, thread/workspace changes and logout.
 * Session identity uses an ID; polling may replace the current session object. */
export class ChatHistory {
  private generation = 0;
  private pending?: {controller: AbortController; timer: ReturnType<typeof setTimeout>};
  constructor(private read: Read, private session: () => string | undefined, private timeout = 15_000) {}

  cancel() {
    ++this.generation;
    if (this.pending) {
      clearTimeout(this.pending.timer);
      this.pending.controller.abort();
      this.pending = undefined;
    }
  }

  async load(session: string, path: string, prepare: (value: History) => Promise<void>, apply: (value: History) => void): Promise<Result> {
    this.cancel();
    const generation = this.generation, controller = new AbortController();
    const current = () => generation === this.generation && this.session() === session;
    let timedOut = false;
    const timer = setTimeout(() => { timedOut = true; controller.abort(); }, this.timeout);
    this.pending = {controller, timer};
    try {
      const value = await this.read(path, controller.signal);
      if (!current()) return {state: "stale", current};
      if (timedOut) throw new Error("history timeout");
      await prepare(value);
      if (!current()) return {state: "stale", current};
      if (timedOut) throw new Error("history timeout");
      apply(value);
      return {state: "loaded", current};
    } catch (error) {
      return current() ? {state: "failed", error, timedOut, current} : {state: "stale", current};
    } finally {
      clearTimeout(timer);
      if (this.pending?.controller === controller) this.pending = undefined;
    }
  }
}
