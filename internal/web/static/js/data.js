/* data：sessions / accounts 的拉取与轮询。拉到新数据后广播 data-updated，
 * 由 shell（侧栏）、sessions（工作台头部）、settings（账号池）各自刷新。 */
"use strict";

import { S, emit } from "./state.js";
import { api } from "./api.js";

export async function refreshAll() {
  try {
    // 顺带把 /me 拉一遍：余额只在回合结束时变，而 chat.js 正是在回合收尾
    // 调 refreshAll，所以额度显示会紧跟着扣款更新。
    const [sessions, accounts, me] = await Promise.all([
      api("/sessions"), api("/accounts"), api("/me"),
    ]);
    S.sessions = sessions;
    S.accounts = accounts;
    S.quota = me.quota || null;
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
