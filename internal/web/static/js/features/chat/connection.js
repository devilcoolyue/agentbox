/** Owns the socket and retry generation. It does not read app state or touch DOM. */
export class ChatConnection {
    hooks;
    socket = null;
    generation = 0;
    retry;
    opened = false;
    attempt = 0;
    url = "";
    constructor(hooks) {
        this.hooks = hooks;
    }
    get ready() { return this.socket?.readyState === WebSocket.OPEN; }
    send(data) { if (!this.ready)
        return false; this.socket.send(data); return true; }
    connect(url) {
        if (this.url && this.url !== url)
            this.dispose();
        this.url = url;
        clearTimeout(this.retry);
        const gen = ++this.generation;
        this.socket?.close();
        this.hooks.state("connecting", this.attempt);
        const id = Array.from(crypto.getRandomValues(new Uint8Array(16)), value => value.toString(16).padStart(2, "0")).join("");
        const target = new URL(url);
        target.searchParams.set("connection_id", id);
        const ws = this.socket = new WebSocket(target.href);
        ws.onopen = () => {
            if (gen !== this.generation)
                return;
            const reconnect = this.opened;
            this.opened = true;
            this.attempt = 0;
            this.hooks.state("connected", 0);
            if (reconnect)
                this.hooks.reconnect();
        };
        ws.onmessage = e => { if (gen === this.generation)
            this.hooks.message(e.data); };
        ws.onclose = e => {
            if (gen !== this.generation)
                return;
            this.socket = null;
            this.hooks.state("closed", this.attempt, id);
            // Application refusals require user action, never an infinite reconnect loop.
            if (e.code >= 4000 && e.code < 5000)
                return;
            const delay = [1000, 2000, 4000, 8000, 15000][Math.min(this.attempt++, 4)];
            this.retry = setTimeout(() => { if (gen === this.generation)
                this.connect(url); }, delay);
        };
    }
    dispose() {
        ++this.generation;
        clearTimeout(this.retry);
        this.retry = undefined;
        this.socket?.close();
        this.socket = null;
        this.opened = false;
        this.attempt = 0;
        this.url = "";
    }
}
