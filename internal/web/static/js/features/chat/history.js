/** Owns requests and deadlines across reload, thread/workspace changes and logout.
 * Session identity uses an ID; polling may replace the current session object. */
export class ChatHistory {
    read;
    session;
    timeout;
    generation = 0;
    pending;
    constructor(read, session, timeout = 15_000) {
        this.read = read;
        this.session = session;
        this.timeout = timeout;
    }
    cancel() {
        ++this.generation;
        if (this.pending) {
            clearTimeout(this.pending.timer);
            this.pending.controller.abort();
            this.pending = undefined;
        }
    }
    async load(session, path, prepare, apply) {
        this.cancel();
        const generation = this.generation, controller = new AbortController();
        const current = () => generation === this.generation && this.session() === session;
        let timedOut = false;
        const timer = setTimeout(() => { timedOut = true; controller.abort(); }, this.timeout);
        this.pending = { controller, timer };
        try {
            const value = await this.read(path, controller.signal);
            if (!current())
                return { state: "stale", current };
            if (timedOut)
                throw new Error("history timeout");
            await prepare(value);
            if (!current())
                return { state: "stale", current };
            if (timedOut)
                throw new Error("history timeout");
            apply(value);
            return { state: "loaded", current };
        }
        catch (error) {
            return current() ? { state: "failed", error, timedOut, current } : { state: "stale", current };
        }
        finally {
            clearTimeout(timer);
            if (this.pending?.controller === controller)
                this.pending = undefined;
        }
    }
}
