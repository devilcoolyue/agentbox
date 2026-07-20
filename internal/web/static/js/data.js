/* data：sessions / accounts 的拉取与轮询。拉到新数据后广播 data-updated，
 * 由 shell（侧栏）、sessions（工作台头部）、settings（账号池）各自刷新。 */
"use strict";

import { S, emit } from "./state.js";
import { api } from "./api.js";

export async function refreshAll() {
  try {
    const [sessions, accounts] = await Promise.all([api("/sessions"), api("/accounts")]);
    S.sessions = sessions;
    S.accounts = accounts;
    if (S.current) {
      const cur = sessions.find((x) => x.id === S.current.id);
      if (cur) S.current = cur;
    }
    emit("data-updated");
  } catch (_) { /* 网络抖动时保持现状 */ }
}

export function startPolling() {
  if (S.refreshTimer) clearInterval(S.refreshTimer);
  S.refreshTimer = setInterval(refreshAll, 8000);
}
