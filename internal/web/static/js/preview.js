/* preview：文件预览 / 在线编辑弹窗（HTML 实时渲染、文本编辑、图片查看、
 * 二进制/超大文件提示）。
 *
 * HTML 走 iframe 真实渲染而不是贴源码：产品改原型时要看的是页面本身。渲染用的
 * 直链由服务端另发一张限权通行证（见 internal/server/preview.go），iframe 再叠一层
 * sandbox —— 原型里的脚本能跑，但拿不到控制台的登录令牌。 */
"use strict";
import { S } from "./state.js";
import { $, withSpin, fmtSize, askConfirm, startDownload } from "./util.js";
import { api, fileDownloadURL } from "./api.js";
import { loadFiles } from "./files.js";
const FV = {
    path: "",
    scope: "workspace",
    name: "",
    dirty: false,
    blobURL: "",
    /** HTML 才有「预览 / 源码」两态；其余文件恒为 source */
    html: false,
    mode: "preview",
    /** 服务端签发的预览直链，刷新时加 cache-buster 重新载入 */
    url: "",
    /** 源码已经拉过一次，切回「源码」不再重复请求 */
    srcLoaded: false,
    /** 上次看到的 mtime，用来发现 Agent 改了文件 */
    mtime: "",
    poll: 0,
    reloadSeq: 0,
};
const IMG_EXT = ["png", "jpg", "jpeg", "gif", "svg", "webp", "ico", "bmp"];
const HTML_EXT = ["html", "htm"];
const EDIT_MAX = 2 << 20; // 编辑器最大 2MB
const POLL_MS = 2500;
/* 弹窗自己的 scope：从对话里点开的 HTML 可能落在共享目录，不能跟着文件页的
 * 全局 S.fileScope 走，否则会去错误的根下找文件。 */
const qs = () => (FV.scope === "shared" ? "&scope=shared" : "");
const fileAPI = (extra = "") => `/api/sessions/${S.current.id}/file?path=${encodeURIComponent(FV.path)}${qs()}${extra}`;
function fvShow(which) {
    $("fv-editor").classList.toggle("hidden", which !== "editor");
    $("fv-framewrap").classList.toggle("hidden", which !== "frame");
    $("fv-imgwrap").classList.toggle("hidden", which !== "img");
    $("fv-notice").classList.toggle("hidden", which !== "notice");
}
function notice(text) {
    $("fv-notice").textContent = text;
    fvShow("notice");
}
/* 头部按钮的显隐：只有 HTML 才有模式切换、视口模拟、刷新、全屏这一排 */
function syncChrome() {
    const preview = FV.html && FV.mode === "preview";
    $("fv-modes").classList.toggle("hidden", !FV.html);
    $("fv-vps").classList.toggle("hidden", !preview);
    $("fv-auto-wrap").classList.toggle("hidden", !preview);
    $("fv-reload").classList.toggle("hidden", !preview);
    $("fv-newtab").classList.toggle("hidden", !preview);
    $("fv-full").classList.toggle("hidden", !FV.html);
    $("fv-mode-view").classList.toggle("active", FV.mode === "preview");
    $("fv-mode-src").classList.toggle("active", FV.mode === "source");
    // 预览态没有「保存」可言，避免和 fv-state 的提示打架
    $("fv-save").classList.toggle("hidden", preview);
}
/* ---------------- 打开 ---------------- */
export async function openPreview(fullRel, ent, scope = S.fileScope) {
    FV.path = fullRel;
    FV.scope = scope;
    FV.name = fullRel.split("/").pop() || fullRel;
    FV.dirty = false;
    FV.srcLoaded = false;
    FV.url = "";
    FV.mtime = ent?.mtime || "";
    if (FV.blobURL) {
        URL.revokeObjectURL(FV.blobURL);
        FV.blobURL = "";
    }
    const ext = (FV.name.split(".").pop() || "").toLowerCase();
    FV.html = HTML_EXT.includes(ext);
    FV.mode = FV.html ? "preview" : "source";
    $("fv-name").textContent = FV.name;
    setMeta(ent);
    $("fv-state").textContent = "";
    $("fv-save").disabled = true;
    $("fv-notice").replaceChildren(withSpin("加载中…"));
    fvShow("notice");
    syncChrome();
    $("dlg-file").showModal();
    if (FV.html) {
        await mountFrame();
        startPoll();
        // 从对话点进来时没有 ent，补一次 stat 把大小/权限填上
        if (!ent)
            statFile().then((e) => { if (e && FV.path === fullRel) {
                setMeta(e);
                FV.mtime = e.mtime;
            } });
        return;
    }
    if (IMG_EXT.includes(ext)) {
        try {
            const res = await authFetch(fileAPI());
            FV.blobURL = URL.createObjectURL(await res.blob());
            $("fv-img").src = FV.blobURL;
            fvShow("img");
        }
        catch (e) {
            notice("图片加载失败：" + e.message);
        }
        return;
    }
    await loadSource(ent?.size ?? 0);
}
function setMeta(ent) {
    const rootName = FV.scope === "shared" ? "/shared" : "/workspace";
    const tail = ent ? ` · ${ent.mode || ""} · ${fmtSize(ent.size)}` : "";
    $("fv-meta").textContent = `${rootName}/${FV.path}${tail}`;
}
async function authFetch(url) {
    const res = await fetch(url, { headers: { Authorization: "Bearer " + S.token } });
    if (!res.ok)
        throw new Error(res.statusText);
    return res;
}
/* 目录列表里捞出本文件的条目：既用于补元信息，也用于轮询 mtime */
async function statFile() {
    const cut = FV.path.lastIndexOf("/");
    const dir = cut < 0 ? "" : FV.path.slice(0, cut);
    const name = cut < 0 ? FV.path : FV.path.slice(cut + 1);
    try {
        const list = await api(`/sessions/${S.current.id}/files?path=${encodeURIComponent(dir)}${qs()}`);
        return list.find((e) => e.name === name) || null;
    }
    catch (_) {
        return null;
    }
}
/* ---------------- 源码态 ---------------- */
async function loadSource(size) {
    if (size > EDIT_MAX) {
        notice(`文件超过 ${fmtSize(EDIT_MAX)}，不支持在线编辑，请下载查看。`);
        return;
    }
    try {
        const buf = new Uint8Array(await (await authFetch(fileAPI())).arrayBuffer());
        // 前 8KB 含 NUL 视为二进制
        for (let i = 0; i < Math.min(buf.length, 8192); i++) {
            if (buf[i] === 0) {
                notice("二进制文件，无法预览，请下载查看。");
                return;
            }
        }
        $("fv-editor").value = new TextDecoder("utf-8").decode(buf);
        FV.srcLoaded = true;
        fvShow("editor");
        $("fv-state").textContent = "可编辑";
    }
    catch (e) {
        notice("读取失败：" + e.message);
    }
}
/* ---------------- 预览态 ---------------- */
async function mountFrame() {
    try {
        const g = await api(`/sessions/${S.current.id}/preview?path=${encodeURIComponent(FV.path)}${qs()}`);
        FV.url = g.url;
        reloadFrame();
        fvShow("frame");
        $("fv-state").textContent = "";
    }
    catch (e) {
        notice("预览失败：" + e.message);
    }
}
/* 通行证响应带 no-store，重新赋 src 就是一次真刷新；查询串不参与相对路径解析，
 * 所以 cache-buster 不会打乱页面里 ./xxx 的引用。 */
function reloadFrame() {
    if (!FV.url)
        return;
    $("fv-frame").src = FV.url + "?_=" + ++FV.reloadSeq;
}
function setViewport(vp) {
    const wrap = $("fv-framewrap");
    wrap.dataset.vp = vp;
    for (const b of $("fv-vps").querySelectorAll("[data-vp]")) {
        b.classList.toggle("active", b.dataset.vp === vp);
    }
}
/* Agent 写完文件 → mtime 变 → 自动重渲染。勾掉自动刷新就只提示不动画面，
 * 免得正在里面点着看的时候被踹回首屏。 */
function startPoll() {
    stopPoll();
    FV.poll = setInterval(async () => {
        if (!FV.html || FV.mode !== "preview")
            return;
        const ent = await statFile();
        if (!ent || !ent.mtime || ent.mtime === FV.mtime)
            return;
        FV.mtime = ent.mtime;
        setMeta(ent);
        if ($("fv-auto").checked) {
            reloadFrame();
            $("fv-state").textContent = "已更新 " + new Date().toLocaleTimeString();
        }
        else {
            $("fv-state").textContent = "文件已更新，点「刷新」查看";
        }
    }, POLL_MS);
}
function stopPoll() {
    if (FV.poll)
        clearInterval(FV.poll);
    FV.poll = 0;
}
async function setMode(mode) {
    if (!FV.html || FV.mode === mode)
        return;
    FV.mode = mode;
    syncChrome();
    if (mode === "preview") {
        if (FV.url) {
            fvShow("frame");
            reloadFrame();
        }
        else
            await mountFrame();
        $("fv-state").textContent = "";
        return;
    }
    if (FV.srcLoaded) {
        fvShow("editor");
        $("fv-state").textContent = FV.dirty ? "未保存" : "可编辑";
        return;
    }
    $("fv-notice").replaceChildren(withSpin("加载中…"));
    fvShow("notice");
    const ent = await statFile();
    await loadSource(ent?.size ?? 0);
}
/* ---------------- 编辑 / 保存 ---------------- */
function markDirty() {
    FV.dirty = true;
    $("fv-save").disabled = false;
    $("fv-state").textContent = "未保存";
}
$("fv-editor").addEventListener("input", markDirty);
/* Tab 键在编辑器里插入两个空格 */
$("fv-editor").addEventListener("keydown", (e) => {
    if (e.key === "Tab") {
        e.preventDefault();
        const t = e.target, s = t.selectionStart;
        t.setRangeText("  ", s, t.selectionEnd, "end");
        markDirty();
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
        const res = await fetch(fileAPI(), {
            method: "PUT",
            headers: { Authorization: "Bearer " + S.token },
            body: $("fv-editor").value,
        });
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
        // 自己刚写的内容不该在下一次轮询里再被当成「外部改动」提示一遍
        const ent = await statFile();
        if (ent) {
            FV.mtime = ent.mtime;
            setMeta(ent);
        }
        loadFiles(); // 刷新大小/时间
    }
    catch (e) {
        $("fv-save").disabled = false;
        $("fv-state").textContent = "保存失败：" + e.message;
    }
}
/* ---------------- 头部动作 ---------------- */
$("fv-save").addEventListener("click", saveFile);
$("fv-mode-view").addEventListener("click", () => setMode("preview"));
$("fv-mode-src").addEventListener("click", () => setMode("source"));
$("fv-reload").addEventListener("click", () => {
    reloadFrame();
    $("fv-state").textContent = "已刷新 " + new Date().toLocaleTimeString();
});
$("fv-newtab").addEventListener("click", () => {
    if (FV.url)
        window.open(FV.url, "_blank", "noopener");
});
$("fv-full").addEventListener("click", () => {
    const full = $("dlg-file").classList.toggle("fv-max");
    $("fv-full").textContent = full ? "还原" : "全屏";
});
$("fv-vps").addEventListener("click", (e) => {
    const b = e.target.closest("[data-vp]");
    if (b)
        setViewport(b.dataset.vp);
});
$("fv-download").addEventListener("click", () => {
    if (FV.path)
        startDownload(fileDownloadURL(FV.path, FV.scope));
});
/* ---------------- 关闭 ---------------- */
async function closePreview() {
    if (FV.dirty) {
        const ok = await askConfirm("有未保存的修改，确定关闭？", {
            title: "放弃修改", hint: "关闭后未保存的内容会丢失。", okLabel: "放弃并关闭", danger: true,
        });
        if (!ok)
            return;
    }
    stopPoll();
    FV.dirty = false;
    FV.url = "";
    if (FV.blobURL) {
        URL.revokeObjectURL(FV.blobURL);
        FV.blobURL = "";
    }
    $("fv-editor").value = "";
    $("fv-frame").src = "about:blank"; // 别让原型在后台继续跑
    $("dlg-file").classList.remove("fv-max");
    $("fv-full").textContent = "全屏";
    $("dlg-file").close();
}
$("fv-close").addEventListener("click", closePreview);
$("dlg-file").addEventListener("cancel", (e) => {
    // Esc
    e.preventDefault();
    closePreview();
});
/* ---------------- 对话里的 HTML 入口 ---------------- */
/* Agent 报出来的多是容器内绝对路径，映射回文件页的「范围 + 相对路径」。
 * 认不出来的（/etc/... 之类）返回 null，chip 就不做成可点。 */
function previewTarget(raw) {
    const p = String(raw || "").trim();
    if (!/\.html?$/i.test(p))
        return null;
    if (p.startsWith("/workspace/"))
        return { rel: p.slice("/workspace/".length), scope: "workspace" };
    if (p.startsWith("/shared/"))
        return { rel: p.slice("/shared/".length), scope: "shared" };
    if (!p.startsWith("/"))
        return { rel: p.replace(/^\.\//, ""), scope: "workspace" };
    return null;
}
/* 对话流是流式追加的，用事件委托而不是逐条挂监听 */
$("chat-log").addEventListener("click", (e) => {
    const el = e.target.closest("[data-html-preview]");
    if (!el || !S.current)
        return;
    const t = previewTarget(el.dataset.htmlPreview || "");
    if (t)
        openPreview(t.rel, undefined, t.scope);
});
