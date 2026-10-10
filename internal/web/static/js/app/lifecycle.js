import { showView, renderSidebar } from "../shell.js";
import { S, bus } from "../state.js";
import { initWorkspaceFilter } from "../features/workspaces/filter.js";
import { initWorkspaceCreation } from "../features/workspaces/create.js";
import { initFirstTask } from "../features/workspaces/first-task.js";
import { initOnboarding } from "../onboarding.js";
import { initChat } from "../chat.js";
import { initSettings } from "../settings.js";
import { termTeardown, termSpendPolling } from "../term.js";
import { stopPolling } from "../data.js";
import { stopPing } from "../ping.js";
import { settingsState } from "../features/settings/state.js";
import { initRouter } from "./router.js";
import { initPricing } from "../pricing.js";
import { initRemoteBrowser } from "../remote-browser.js";
import { initMCP } from "../mcp.js";
import { initUpdates } from "../updates.js";
import { initThemes } from "../themes.js";
/** App-owned feature lifetime; each init can be safely repeated. */
export function initApplication() {
    const lifetime = new AbortController();
    const disposeRouter = initRouter();
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
        S.draftScope = "";
        S.draftProtocol = 0;
        S.chatScope = "";
        S.chatProtocol = 0;
        showView("work");
        renderSidebar();
    };
    const start = () => {
        disposers.splice(0).reverse().forEach(dispose => dispose());
        disposers = [initWorkspaceFilter(renderSidebar), initOnboarding(), initWorkspaceCreation(), initChat(), initFirstTask(), initMCP(), initSettings(), initUpdates(), initPricing(), initRemoteBrowser(), initThemes()];
    };
    bus.addEventListener("signed-in", start, { signal: lifetime.signal });
    bus.addEventListener("signed-out", stop, { signal: lifetime.signal });
    window.addEventListener("pagehide", stop, { signal: lifetime.signal });
    // bfcache restores the same JS heap. Revalidate the login on restoration.
    window.addEventListener("pageshow", e => { if (e.persisted)
        location.reload(); }, { signal: lifetime.signal });
    return () => { disposeRouter(); stop(); lifetime.abort(); };
}
