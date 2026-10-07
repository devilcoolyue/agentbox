/* api：REST / WebSocket 地址 / 附件上传等与服务端通信的底座。
 * 401 时通过 bus 广播 unauthorized（login.ts 负责跳回登录页），避免反向依赖。 */
import { t as i18nText } from "./i18n.js";
"use strict";
import { responseError } from "./problems.js";
import { S, emit } from "./state.js";
/* api 的返回类型由调用点用类型参数指定，例如 api<Session[]>("/sessions")。
 * 默认 unknown 而不是 any：忘了标注时，一用到返回值就会报错，逼着把接口形状
 * 写进 types.d.ts，而不是悄悄退化成无类型。 */
export async function api(path, opts = {}) {
    const token = S.token;
    const res = await fetch("/api" + path, {
        ...opts,
        headers: { ...(opts.headers || {}), Authorization: "Bearer " + S.token },
    });
    if (token !== S.token)
        throw new Error(i18nText("登录状态已变化"));
    if (!res.ok) {
        const error = await responseError(res, i18nText);
        if (token !== S.token)
            throw new Error(i18nText("登录状态已变化"));
        if (res.status === 401)
            emit("unauthorized", error.message);
        throw error;
    }
    const data = await res.json();
    if (token !== S.token)
        throw new Error(i18nText("登录状态已变化"));
    return data;
}
export function wsURL(path) {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    return `${proto}//${location.host}/api${path}?token=${encodeURIComponent(S.token)}`;
}
/* 当前文件范围的查询串（工作区 / 共享目录） */
export function scopeQS() {
    return S.fileScope === "shared" ? "&scope=shared" : "";
}
/* 单个文件的下载直链（默认当前文件范围；预览弹窗可能停在另一个范围，故可显式
 * 指定）。`dl=1` 让服务端加 Content-Disposition，浏览器才会存盘而不是内联打开。 */
export function fileDownloadURL(rel, scope) {
    const sq = scope ? (scope === "shared" ? "&scope=shared" : "") : scopeQS();
    return `/api/sessions/${S.current.id}/file?path=${encodeURIComponent(rel)}` +
        `&dl=1&token=${encodeURIComponent(S.token)}${sq}`;
}
/* 技能目录里某个文件的直链（`raw=1` 直出原始字节）：图片预览用它当 img.src，
 * `dl=1` 则让浏览器存盘。技能页的文本预览走 JSON 接口，不用这个。 */
export function skillFileURL(skill, path, scope, dl = false) {
    return `/api/sessions/${S.current.id}/skills/${encodeURIComponent(skill)}/file` +
        `?path=${encodeURIComponent(path)}&scope=${encodeURIComponent(scope)}&raw=1` +
        (dl ? "&dl=1" : "") + `&token=${encodeURIComponent(S.token)}`;
}
/* 整个范围打包成 zip 的下载直链 */
export function archiveDownloadURL(scope) {
    const sq = scope ? (scope === "shared" ? "&scope=shared" : "") : scopeQS();
    return `/api/sessions/${S.current.id}/archive?token=${encodeURIComponent(S.token)}${sq}`;
}
/* /shared/ 下任意文件（.images 图片、.file 附件）转成可访问的 URL。
 * 这几个走会话资源的函数都只在有打开会话时才被调用（附件条、终端链接、
 * 上传按钮都挂在工作台里），故 S.current 直接断言非空。 */
export function imgURLFromPath(containerPath) {
    const rel = String(containerPath).replace(/^\/shared\//, "");
    return `/api/sessions/${S.current.id}/file?path=${encodeURIComponent(rel)}` +
        `&scope=shared&token=${encodeURIComponent(S.token)}`;
}
/* 上传附件（对话/终端粘贴图片、文件），落到共享目录，48h 后过期 */
export async function uploadAttachment(blob, options) {
    const ext = ((blob.type || "").split("/")[1] || "bin").replace("jpeg", "jpg").replace("svg+xml", "svg");
    const fd = new FormData();
    fd.append("file", blob, blob.name || "paste." + ext);
    return api(`/sessions/${options?.session || S.current.id}/images`, { method: "POST", body: fd, signal: options?.signal });
}
