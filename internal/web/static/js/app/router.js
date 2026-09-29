import { S, bus, emit } from "../state.js";
import { openHome, openSession } from "../sessions.js";
import { SET_SECS } from "../settings.js";
import { toast } from "../util.js";
const GIT_SECS = ["guide", "profile", "connections"];
const TABS = ["chat", "term", "files", "changes", "skills", "browser"];
function currentHash() {
    if (S.view === "git")
        return `#/git/${S.gitSec}`;
    if (S.view === "settings")
        return `#/settings/${S.sec}`;
    if (S.view !== "work")
        return `#/${S.view}`;
    return S.current ? `#/sessions/${encodeURIComponent(S.current.id)}/${S.tab}` : "#/";
}
/** Hash routes work with the embedded static server and existing reverse proxies. */
export function initRouter() {
    const lifetime = new AbortController();
    const options = { signal: lifetime.signal };
    let ready = false;
    let applying = false;
    let queued = false;
    function write(replace) {
        const hash = currentHash();
        if (location.hash !== hash) {
            history[replace ? "replaceState" : "pushState"](null, "", hash);
        }
    }
    function restore() {
        if (!ready)
            return; // Keep the destination while waiting for login and session data.
        applying = true;
        try {
            let parts = [];
            try {
                parts = location.hash.slice(1).split("/").slice(1).map(decodeURIComponent);
            }
            catch { /* invalid URL → home */ }
            const [page, id, tab] = parts;
            if ((page === "usage" || page === "tunnel") && parts.length === 1) {
                emit(`open-${page}`);
            }
            else if (page === "git" && parts.length <= 2) {
                emit("open-git", GIT_SECS.includes(id) ? id : "guide");
            }
            else if (page === "settings" && parts.length <= 2 && S.role === "admin") {
                S.sec = SET_SECS.includes(id) ? id : "accounts";
                emit("open-settings");
            }
            else if (page === "sessions" && id && parts.length <= 3) {
                const session = S.sessions.find(s => s.id === id);
                if (session) {
                    const target = TABS.includes(tab) ? tab : "chat";
                    void openSession(session, target);
                }
                else {
                    openHome();
                    toast("该工作空间不存在或无权访问", true);
                }
            }
            else {
                openHome();
            }
            write(true); // Normalize invalid/inaccessible destinations without adding history.
        }
        finally {
            applying = false;
        }
    }
    bus.addEventListener("app-ready", () => { ready = true; restore(); }, options);
    bus.addEventListener("signed-out", () => { ready = false; }, options);
    window.addEventListener("pagehide", () => { ready = false; }, options);
    window.addEventListener("hashchange", restore, options);
    bus.addEventListener("navigation-changed", () => {
        if (!ready || applying || queued)
            return;
        queued = true;
        // Opening a workspace updates view, session and tab together. Record only the final state.
        queueMicrotask(() => {
            queued = false;
            if (ready)
                write(false);
        });
    }, options);
    return () => { ready = false; lifetime.abort(); };
}
