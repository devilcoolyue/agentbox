/* chat-archive：历史对话的折叠列表。服务端把 chat.jsonl 按「新对话」分割线
 * 切成段，默认只回传最近一段全文；更早的段在对话流顶部折叠成行（时间 ·
 * 轮数 · 首条消息预览），点击才拉取 ?seg=N 渲染进该行下方。段内容渲染后
 * 缓存在 DOM 里，再点只做收合/展开，不重复请求。 */
"use strict";

import { spinEl, fmtTime, toast } from "./util.js";
import { api } from "./api.js";
import { renderEntry, svgIcon, USER_ATTACH_RE } from "./chat-render.js";
import { agentAvatar } from "./brand.js";
import { S } from "./state.js";

/* 折叠列表：分节线 + 每段一行，由 loadHistory 插在对话流顶部 */
export function archiveList(sessID, archives) {
  const wrap = document.createElement("div");
  wrap.className = "arch-list";
  const head = document.createElement("div");
  head.className = "chat-divider mono";
  const t = document.createElement("span");
  t.textContent = `历史对话 · ${archives.length} 段`;
  head.appendChild(t);
  wrap.appendChild(head);
  for (const a of archives) wrap.appendChild(archiveItem(sessID, a));
  return wrap;
}

function archiveItem(sessID, a) {
  const box = document.createElement("div");
  box.className = "arch";
  const head = document.createElement("button");
  head.type = "button";
  head.className = "arch-head";
  head.title = "展开这段对话";

  const caret = document.createElement("span");
  caret.className = "arch-caret";
  caret.appendChild(svgIcon("caret", 11));
  const time = document.createElement("span");
  time.className = "arch-time mono";
  time.textContent = a.ts ? fmtTime(a.ts) : "";
  const cnt = document.createElement("span");
  cnt.className = "arch-cnt mono";
  cnt.textContent = `${a.turns || 0} 轮`;
  const pv = document.createElement("span");
  pv.className = "arch-preview";
  // 附件占位符只留类别名，预览行不必展示容器内路径
  pv.textContent = String(a.preview || "").replace(USER_ATTACH_RE, "[$1]") || "（无文字消息）";
  head.append(caret, time, cnt, pv);

  const body = document.createElement("div");
  body.className = "arch-body hidden";
  box.append(head, body);

  let loaded = false, busy = false;
  head.addEventListener("click", async () => {
    if (busy) return;
    if (loaded) { // 已缓存在 DOM，只切换收合状态
      const open = box.classList.toggle("open");
      body.classList.toggle("hidden", !open);
      head.title = open ? "收起这段对话" : "展开这段对话";
      return;
    }
    busy = true;
    caret.replaceChildren(spinEl());
    try {
      const { entries } = await api(`/sessions/${sessID}/history?seg=${a.seg}`);
      if (!box.isConnected) return; // 拉取途中切走了会话，列表已被清空
      renderEntries(body, entries);
      loaded = true;
      box.classList.add("open");
      body.classList.remove("hidden");
      head.title = "收起这段对话";
    } catch (e) {
      toast("加载历史对话失败：" + e.message, true);
    } finally {
      busy = false;
      caret.replaceChildren(svgIcon("caret", 11));
    }
  });
  return box;
}

/* 与 chat.js 实时流同款分组规则：用户消息打断回合，agent 输出并入
 * 带品牌头像的 .turn；分割线本身不再渲染（折叠头已标注起始时间）。 */
function renderEntries(box, entries) {
  let turn = null;
  const agent = S.current ? S.current.agent : "claude";
  for (const raw of entries) {
    if (raw.kind === "divider") continue;
    for (const node of renderEntry(raw)) {
      if (node.classList.contains("user")) {
        turn = null;
        box.appendChild(node);
        continue;
      }
      if (!turn) {
        const t = document.createElement("div");
        t.className = "turn";
        const av = document.createElement("span");
        av.className = "turn-avatar";
        av.appendChild(agentAvatar(agent, { icon: 14 }));
        turn = document.createElement("div");
        turn.className = "turn-body";
        t.append(av, turn);
        box.appendChild(t);
      }
      turn.appendChild(node);
    }
  }
}
