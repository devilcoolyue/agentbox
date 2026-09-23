/* data：sessions / accounts 的拉取与轮询。拉到新数据后广播 data-updated，
 * 由 shell（侧栏）、sessions（工作台头部）、settings（账号池）各自刷新。 */
"use strict";

import { Poller } from "./shared/poller.js";
import { S, emit } from "./state.js";
import { api } from "./api.js";
import type { Account, Me, Session } from "./types.js";

export async function refreshAll(signal?: AbortSignal) {
  try {
    // 顺带把 /me 拉一遍：余额只在回合结束时变，而 chat.ts 正是在回合收尾
    // 调 refreshAll，所以额度显示会紧跟着扣款更新。
    const [sessions, accounts, me] = await Promise.all([
      api<Session[]>("/sessions", { signal }), api<Account[]>("/accounts", { signal }), api<Me>("/me", { signal }),
    ]);
    if (signal?.aborted) return;
    S.sessions = sessions;
    S.accounts = accounts;
    S.quota = me.quota || null;
    if (me.timezone && me.timezone !== S.timeZone) {
      S.timeZone = me.timezone;
      emit("timezone-updated");
    }
    if (S.current) {
      const cur = sessions.find((x) => x.id === S.current!.id);
      if (cur) S.current = cur;
    }
    emit("data-updated");
  } catch (_) { /* 网络抖动时保持现状 */ }
}

const polling = new Poller();
export function startPolling() { polling.start(8000, signal => refreshAll(signal)); }
export function stopPolling() { polling.stop(); }
