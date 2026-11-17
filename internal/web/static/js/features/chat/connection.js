/** Owns the socket and retry generation. It does not read app state or touch DOM.
 *
 * A socket can die without a close: after the machine sleeps or the network
 * changes the browser still reports it open, and since messages are sent over
 * HTTP nothing is ever written to find out. With a server that answers pings
 * (see `heartbeat`) the connection pings on a timer and on `probe()`, and
 * replaces a socket that stays silent. */
export class ChatConnection {
    hooks;
    timing;
    socket = null;
    generation = 0;
    retry;
    beat;
    probing;
    opened = false;
    attempt = 0;
    url = "";
    id = "";
    answers = false;
    heard = 0;
    constructor(hooks, timing = { interval: 20_000, silence: 45_000, probe: 4_000 }) {
        this.hooks = hooks;
        this.timing = timing;
    }
    get ready() { return this.socket?.readyState === WebSocket.OPEN; }
    send(data) { if (!this.ready)
        return false; this.socket.send(data); return true; }
    /** The server said whether it answers pings; older ones do not, and are left alone. */
    heartbeat(on) {
        this.answers = on;
        clearInterval(this.beat);
        this.beat = undefined;
        if (!on) {
            clearTimeout(this.probing);
            this.probing = undefined;
            return;
        }
        this.beat = setInterval(() => {
            if (!this.ready)
                return;
            // Timers stall while the page is suspended, so on waking the silence is already long.
            if (Date.now() - this.heard > this.timing.silence)
                this.lost();
            else
                this.ping();
        }, this.timing.interval);
    }
    /** Check the socket now: called when the page returns, the network is back, or a
     * message was sent. A socket that does not answer in time is replaced at once. */
    probe() {
        if (!this.url)
            return;
        if (!this.socket) {
            if (this.retry !== undefined) {
                this.attempt = 0;
                this.connect(this.url);
            }
            return;
        }
        if (!this.ready || !this.answers || this.probing !== undefined)
            return;
        const gen = this.generation;
        if (!this.ping()) {
            this.lost();
            return;
        }
        this.probing = setTimeout(() => { this.probing = undefined; if (gen === this.generation)
            this.lost(); }, this.timing.probe);
    }
    ping() { try {
        return this.send('{"type":"ping"}');
    }
    catch {
        return false;
    } }
    /** The socket is gone, with or without a close event. */
    lost(code = 0) {
        const socket = this.socket, id = this.id;
        ++this.generation; // late events of the dead socket are ignored
        this.socket = null;
        clearTimeout(this.probing);
        this.probing = undefined;
        try {
            socket?.close();
        }
        catch { }
        this.hooks.state("closed", this.attempt, id);
        // Application refusals require user action, never an infinite reconnect loop.
        if (code >= 4000 && code < 5000)
            return;
        // A silent socket was open a moment ago: reconnect without the backoff.
        const delay = code === 0 && this.attempt === 0 ? 0 : [1000, 2000, 4000, 8000, 15000][Math.min(this.attempt, 4)];
        this.attempt++;
        const gen = this.generation, url = this.url;
        this.retry = setTimeout(() => { if (gen === this.generation)
            this.connect(url); }, delay);
    }
    connect(url) {
        if (this.url && this.url !== url)
            this.dispose();
        this.url = url;
        clearTimeout(this.retry);
        this.retry = undefined;
        clearTimeout(this.probing);
        this.probing = undefined;
        const gen = ++this.generation;
        this.socket?.close();
        this.hooks.state("connecting", this.attempt);
        const id = this.id = Array.from(crypto.getRandomValues(new Uint8Array(16)), value => value.toString(16).padStart(2, "0")).join("");
        const target = new URL(url);
        target.searchParams.set("connection_id", id);
        const ws = this.socket = new WebSocket(target.href);
        ws.onopen = () => {
            if (gen !== this.generation)
                return;
            const reconnect = this.opened;
            this.opened = true;
            this.attempt = 0;
            this.heard = Date.now();
            this.hooks.state("connected", 0);
            if (reconnect)
                this.hooks.reconnect();
        };
        ws.onmessage = e => {
            if (gen !== this.generation)
                return;
            this.heard = Date.now();
            clearTimeout(this.probing);
            this.probing = undefined;
            this.hooks.message(e.data);
        };
        ws.onclose = e => { if (gen === this.generation)
            this.lost(e.code || 1006); };
    }
    dispose() {
        ++this.generation;
        clearTimeout(this.retry);
        this.retry = undefined;
        clearTimeout(this.probing);
        this.probing = undefined;
        clearInterval(this.beat);
        this.beat = undefined;
        this.socket?.close();
        this.socket = null;
        this.opened = false;
        this.attempt = 0;
        this.url = "";
        this.answers = false;
    }
}
