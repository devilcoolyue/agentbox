/** One explicit send owns its frozen input, validation and optional legacy wake.
 * Cancel invalidates in-flight completions; it never replays a delivery. */
export class ChatSender {
    hooks;
    wakeLimit;
    generation = 0;
    pending;
    busy = false;
    constructor(hooks, wakeLimit = 60_000) {
        this.hooks = hooks;
        this.wakeLimit = wakeLimit;
    }
    cancel() {
        ++this.generation;
        this.busy = false;
        if (this.pending) {
            clearTimeout(this.pending.timer);
            this.pending.resolve();
            this.pending = undefined;
        }
    }
    async send() {
        if (this.busy || !this.hooks.allowed())
            return;
        const snapshot = this.hooks.read();
        if (!snapshot)
            return;
        const generation = ++this.generation;
        const owns = () => generation === this.generation && this.hooks.read()?.owner === snapshot.owner;
        const current = () => owns() && this.hooks.read()?.content === snapshot.content;
        this.busy = true;
        this.hooks.changed();
        try {
            if (!this.hooks.durable() && !this.hooks.connected()) {
                this.hooks.waking();
                this.hooks.connect();
                const deadline = Date.now() + this.wakeLimit;
                while (owns() && !this.hooks.connected() && Date.now() < deadline) {
                    await new Promise(resolve => { this.pending = { resolve, timer: setTimeout(() => { this.pending = undefined; resolve(); }, 400) }; });
                }
                if (!owns())
                    return;
                if (!this.hooks.connected()) {
                    this.hooks.timeout();
                    return;
                }
                this.hooks.awake();
            }
            if (!await this.hooks.validate() || !owns() || !this.hooks.allowed())
                return;
            if (!current()) {
                this.hooks.edited();
                return;
            }
            await this.hooks.deliver(snapshot, current);
        }
        finally {
            if (generation === this.generation) {
                this.busy = false;
                this.hooks.changed();
            }
        }
    }
}
