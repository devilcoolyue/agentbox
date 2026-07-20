/* api：REST / WebSocket 地址 / 附件上传等与服务端通信的底座。
 * 401 时通过 bus 广播 unauthorized（login.js 负责跳回登录页），避免反向依赖。 */
"use strict";

import { S, emit } from "./state.js";

export async function api(path, opts = {}) {
  const res = await fetch("/api" + path, {
    ...opts,
    headers: { ...(opts.headers || {}), Authorization: "Bearer " + S.token },
  });
  if (res.status === 401) {
    emit("unauthorized");
    throw new Error("unauthorized");
  }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch (_) {}
    throw new Error(msg);
  }
  return res.json();
}

export function wsURL(path) {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${location.host}/api${path}?token=${encodeURIComponent(S.token)}`;
}

/* 当前文件范围的查询串（工作区 / 共享目录） */
export function scopeQS() {
  return S.fileScope === "shared" ? "&scope=shared" : "";
}

/* /shared/ 下任意文件（.images 图片、.file 附件）转成可访问的 URL */
export function imgURLFromPath(containerPath) {
  const rel = String(containerPath).replace(/^\/shared\//, "");
  return `/api/sessions/${S.current.id}/file?path=${encodeURIComponent(rel)}` +
    `&scope=shared&token=${encodeURIComponent(S.token)}`;
}

/* 上传附件（对话/终端粘贴图片、文件），落到共享目录，48h 后过期 */
export async function uploadAttachment(blob) {
  const ext = ((blob.type || "").split("/")[1] || "bin").replace("jpeg", "jpg").replace("svg+xml", "svg");
  const fd = new FormData();
  fd.append("file", blob, blob.name || "paste." + ext);
  return api(`/sessions/${S.current.id}/images`, { method: "POST", body: fd });
}
