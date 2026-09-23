import { showView } from "../shell.js";
import { S, bus } from "../state.js";
import { initChat } from "../chat.js";
import { initSettings } from "../settings.js";
import { termTeardown, termSpendPolling } from "../term.js";
import { stopPolling } from "../data.js";
import { stopPing } from "../ping.js";
import { settingsState } from "../features/settings/state.js";
/** App-owned feature lifetime; each init can be safely repeated. */
export function initApplication() {
    const lifetime = new AbortController();
    let disposers = [];
    const stop = () => {
        disposers.splice(0).reverse().forEach(dispose => dispose());
        stopPolling();
        stopPing();
        termSpendPolling(false);
        termTeardown();
        settingsState.value = null;
        S.current = null;
        S.sessions = [];
        S.accounts = [];
        S.user = "";
        S.role = "";
        showView("work");
    };
    const start = () => {
        disposers.splice(0).reverse().forEach(dispose => dispose());
        disposers = [initChat(), initSettings()];
    };
    bus.addEventListener("signed-in", start, { signal: lifetime.signal });
    bus.addEventListener("signed-out", stop, { signal: lifetime.signal });
    window.addEventListener("pagehide", stop, { signal: lifetime.signal });
    // bfcache restores the same JS heap. Revalidate the login on restoration.
    window.addEventListener("pageshow", e => { if (e.persisted)
        location.reload(); }, { signal: lifetime.signal });
    return () => { stop(); lifetime.abort(); };
}
