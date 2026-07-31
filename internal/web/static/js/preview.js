/* preview：文件预览 / 在线编辑弹窗（文本编辑、图片查看、二进制/超大文件提示）。 */
"use strict";
import { S } from "./state.js";
import { $, withSpin, fmtSize, askConfirm, startDownload } from "./util.js";
import { fileDownloadURL, scopeQS } from "./api.js";
import { loadFiles } from "./files.js";
const FV = { path: "", dirty: false, blobURL: "" };
const IMG_EXT = ["png", "jpg", "jpeg", "gif", "svg", "webp", "ico", "bmp"];
const EDIT_MAX = 2 << 20; // 编辑器最大 2MB
function fvShow(which) {
    $("fv-editor").classList.toggle("hidden", which !== "editor");
    $("fv-imgwrap").classList.toggle("hidden", which !== "img");
    $("fv-notice").classList.toggle("hidden", which !== "notice");
}
export async function openPreview(fullRel, ent) {
    FV.path = fullRel;
    FV.dirty = false;
    if (FV.blobURL) {
        URL.revokeObjectURL(FV.blobURL);
        FV.blobURL = "";
    }
    $("fv-name").textContent = ent.name;
    const rootName = S.fileScope === "shared" ? "/shared" : "/workspace";
    $("fv-meta").textContent = `${rootName}/${fullRel} · ${ent.mode || ""} · ${fmtSize(ent.size)}`;
    $("fv-state").textContent = "";
    $("fv-save").disabled = true;
    $("fv-notice").replaceChildren(withSpin("加载中…"));
    fvShow("notice");
    $("dlg-file").showModal();
    const ext = (ent.name.split(".").pop() || "").toLowerCase();
    const url = `/api/sessions/${S.current.id}/file?path=${encodeURIComponent(fullRel)}${scopeQS()}`;
    if (IMG_EXT.includes(ext)) {
        try {
            const res = await fetch(url, { headers: { Authorization: "Bearer " + S.token } });
            if (!res.ok)
                throw new Error(res.statusText);
            FV.blobURL = URL.createObjectURL(await res.blob());
            $("fv-img").src = FV.blobURL;
            fvShow("img");
        }
        catch (e) {
            $("fv-notice").textContent = "图片加载失败：" + e.message;
        }
        return;
    }
    if (ent.size > EDIT_MAX) {
        $("fv-notice").textContent = `文件超过 ${fmtSize(EDIT_MAX)}，不支持在线编辑，请下载查看。`;
        return;
    }
    try {
        const res = await fetch(url, { headers: { Authorization: "Bearer " + S.token } });
        if (!res.ok)
            throw new Error(res.statusText);
        const buf = new Uint8Array(await res.arrayBuffer());
        // 前 8KB 含 NUL 视为二进制
        for (let i = 0; i < Math.min(buf.length, 8192); i++) {
            if (buf[i] === 0) {
                $("fv-notice").textContent = "二进制文件，无法预览，请下载查看。";
                return;
            }
        }
        $("fv-editor").value = new TextDecoder("utf-8").decode(buf);
        fvShow("editor");
        $("fv-state").textContent = "可编辑";
    }
    catch (e) {
        $("fv-notice").textContent = "读取失败：" + e.message;
    }
}
$("fv-editor").addEventListener("input", () => {
    FV.dirty = true;
    $("fv-save").disabled = false;
    $("fv-state").textContent = "未保存";
});
/* Tab 键在编辑器里插入两个空格 */
$("fv-editor").addEventListener("keydown", (e) => {
    if (e.key === "Tab") {
        e.preventDefault();
        const t = e.target, s = t.selectionStart;
        t.setRangeText("  ", s, t.selectionEnd, "end");
        FV.dirty = true;
        $("fv-save").disabled = false;
        $("fv-state").textContent = "未保存";
    }
    if ((e.ctrlKey || e.metaKey) && e.key === "s") {
        e.preventDefault();
        saveFile();
    }
});
async function saveFile() {
    if (!FV.path || $("fv-save").disabled)
        return;
    $("fv-save").disabled = true;
    $("fv-state").replaceChildren(withSpin("保存中…"));
    try {
        const res = await fetch(`/api/sessions/${S.current.id}/file?path=${encodeURIComponent(FV.path)}${scopeQS()}`, { method: "PUT", headers: { Authorization: "Bearer " + S.token }, body: $("fv-editor").value });
        if (!res.ok) {
            let msg = res.statusText;
            try {
                msg = (await res.json()).error || msg;
            }
            catch (_) { }
            throw new Error(msg);
        }
        FV.dirty = false;
        $("fv-state").textContent = "已保存 " + new Date().toLocaleTimeString();
        loadFiles(); // 刷新大小/时间
    }
    catch (e) {
        $("fv-save").disabled = false;
        $("fv-state").textContent = "保存失败：" + e.message;
    }
}
$("fv-save").addEventListener("click", saveFile);
$("fv-download").addEventListener("click", () => {
    if (!FV.path)
        return;
    startDownload(fileDownloadURL(FV.path));
});
async function closePreview() {
    if (FV.dirty) {
        const ok = await askConfirm("有未保存的修改，确定关闭？", {
            title: "放弃修改", hint: "关闭后未保存的内容会丢失。", okLabel: "放弃并关闭", danger: true,
        });
        if (!ok)
            return;
    }
    FV.dirty = false;
    if (FV.blobURL) {
        URL.revokeObjectURL(FV.blobURL);
        FV.blobURL = "";
    }
    $("fv-editor").value = "";
    $("dlg-file").close();
}
$("fv-close").addEventListener("click", closePreview);
$("dlg-file").addEventListener("cancel", (e) => {
    // Esc
    e.preventDefault();
    closePreview();
});
