const key = "agentbox.chat-identity.v1", scope = /^[a-f0-9]{64}$/;
/** Only opaque actor scopes, never a token or prompt. The page replacing the
 * browser login clears the prior identity before initializing the new editor.
 * Suspended peer pages must not repeat that clear after the new editor writes. */
export function rememberChatIdentity(storage, draftScope, chatScope) {
    try {
        storage.setItem(key, JSON.stringify({ draft: scope.test(draftScope) ? draftScope : "", chat: scope.test(chatScope) ? chatScope : "" }));
    }
    catch { }
}
export function clearPreviousChatIdentity(storage, hints) {
    try {
        const raw = storage.getItem(key);
        if (!raw || raw.length > 1024)
            return;
        const value = JSON.parse(raw);
        const prefixes = [scope.test(value.draft || "") ? `agentbox.chat-draft.v1.${value.draft}.` : "", scope.test(value.chat || "") ? `agentbox.chat-outbox.v1.${value.chat}.` : ""].filter(Boolean);
        for (let i = storage.length - 1; i >= 0; i--) {
            const name = storage.key(i);
            if (name && !name.endsWith('.enabled') && prefixes.some(p => name.startsWith(p)))
                storage.removeItem(name);
        }
        if (scope.test(value.draft || "")) {
            const prefix = `agentbox.chat-hint.v1.${value.draft}.`;
            for (let i = hints.length - 1; i >= 0; i--) {
                const name = hints.key(i);
                if (name?.startsWith(prefix))
                    hints.removeItem(name);
            }
        }
        if (storage.getItem(key) === raw)
            storage.removeItem(key);
    }
    catch { }
}
