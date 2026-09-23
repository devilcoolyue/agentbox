/** Sequential, cancellable polling: stale responses cannot paint a new view. */
export class Poller {
    controller = null;
    timer;
    start(interval, task) {
        this.stop();
        const controller = this.controller = new AbortController();
        const tick = async () => {
            try {
                await task(controller.signal);
            }
            catch (e) {
                if (!controller.signal.aborted)
                    console.error(e);
            }
            if (!controller.signal.aborted)
                this.timer = setTimeout(tick, interval);
        };
        void tick();
    }
    stop() { this.controller?.abort(); this.controller = null; clearTimeout(this.timer); }
}
