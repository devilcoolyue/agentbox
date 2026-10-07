/** A login owns requests and cleanup. Explicit writes are serialized; cancelling
 * the client never claims that an already submitted server write rolled back. */
export class SettingsRequests {
    send;
    owns;
    lifetime = new AbortController();
    reading;
    revision = 0;
    writes = Promise.resolve();
    cleanups = new Set();
    constructor(send, owns) {
        this.send = send;
        this.owns = owns;
    }
    current() { return !this.lifetime.signal.aborted && this.owns(); }
    cleanup(fn) { this.cleanups.add(fn); return () => this.cleanups.delete(fn); }
    dispose() {
        this.lifetime.abort();
        this.reading?.abort();
        ++this.revision;
        for (const cleanup of this.cleanups)
            cleanup();
        this.cleanups.clear();
    }
    async request(path, options = {}) {
        if (!this.current())
            return;
        try {
            const value = await this.send(path, { ...options, signal: options.signal ? AbortSignal.any([options.signal, this.lifetime.signal]) : this.lifetime.signal });
            return this.current() ? value : undefined;
        }
        catch (error) {
            if (this.current() && !options.signal?.aborted)
                throw error;
        }
    }
    async read() {
        this.reading?.abort();
        const controller = new AbortController();
        this.reading = controller;
        const revision = ++this.revision;
        const value = await this.request('/settings', { signal: controller.signal });
        return revision === this.revision ? value : undefined;
    }
    save(body) {
        ++this.revision;
        this.reading?.abort();
        const operation = this.writes.catch(() => { }).then(async () => {
            ++this.revision;
            this.reading?.abort();
            try {
                return await this.request('/settings', { method: 'PUT', body });
            }
            finally {
                ++this.revision;
                this.reading?.abort();
            }
        });
        this.writes = operation;
        return operation;
    }
}
