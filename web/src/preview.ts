import "./responsive.js";
/* preview：文件预览 / 在线编辑弹窗（HTML 实时渲染、Markdown 渲染、文本编辑、
 * 图片查看、二进制/超大文件提示）。
 *
 * HTML 走 iframe 真实渲染而不是贴源码：产品改原型时要看的是页面本身。渲染用的
 * 直链由服务端另发一张限权通行证（见 internal/server/preview.go），iframe 再叠一层
 * sandbox —— 原型里的脚本能跑，但拿不到控制台的登录令牌。
 *
 * Markdown 不用 iframe：源码本来就要拉下来编辑，直接用对话流那套轻量渲染
 * （chat-render.formatText）画在弹窗里，画的是编辑器里的当前内容，所以改一行
 * 切过去就能看到，不必先保存。 */
"use strict";

import { actionButton } from "./icons.js";

import { S } from "./state.js";
import type { FileScope } from "./state.js";
import type { FileEntry } from "./types.js";
import { $, withSpin, fmtSize, askConfirm, startDownload, fmtClock } from "./util.js";
import { api, fileDownloadURL } from "./api.js";
import { loadFiles } from "./files.js";
import { formatText, splitFrontMatter, frontMatterChips } from "./chat-render.js";
import { SourceEditor } from "./source-editor.js";

const sourceEditor = new SourceEditor();

type FvMode = "preview" | "source";
/** html / md 有「预览 · 源码」两态，plain 只有源码 */
type FvKind = "html" | "md" | "plain";
type Viewport = "desktop" | "tablet" | "phone";

const FV = {
  path: "",
  scope: "workspace" as FileScope,
  name: "",
  dirty: false,
  blobURL: "",
  kind: "plain" as FvKind,
  mode: "preview" as FvMode,
  /** 服务端签发的预览直链，刷新时加 cache-buster 重新载入 */
  url: "",
  /** 源码已经拉过一次，切回「源码」不再重复请求 */
  srcLoaded: false,
  /** 上次看到的 mtime，用来发现 Agent 改了文件 */
  mtime: "",
  poll: 0 as ReturnType<typeof setInterval> | 0,
  reloadSeq: 0,
};

const IMG_EXT = ["png", "jpg", "jpeg", "gif", "svg", "webp", "ico", "bmp"];
const HTML_EXT = ["html", "htm"];
const MD_EXT = ["md", "markdown", "mdx"];
const EDIT_MAX = 2 << 20; // 编辑器最大 2MB
const POLL_MS = 2500;

/* 弹窗自己的 scope：从对话里点开的 HTML 可能落在共享目录，不能跟着文件页的
 * 全局 S.fileScope 走，否则会去错误的根下找文件。 */
const qs = () => (FV.scope === "shared" ? "&scope=shared" : "");
const fileAPI = (extra = "") =>
  `/api/sessions/${S.current!.id}/file?path=${encodeURIComponent(FV.path)}${qs()}${extra}`;

function fvShow(which: "editor" | "frame" | "md" | "img" | "notice") {
  $("fv-source").classList.toggle("hidden", which !== "editor");
  if (which === "editor") sourceEditor.refresh();
  $("fv-framewrap").classList.toggle("hidden", which !== "frame");
  $("fv-mdwrap").classList.toggle("hidden", which !== "md");
  $("fv-imgwrap").classList.toggle("hidden", which !== "img");
  $("fv-notice").classList.toggle("hidden", which !== "notice");
}

function notice(text: string) {
  $("fv-notice").textContent = text;
  fvShow("notice");
}

/* 头部按钮的显隐：HTML / Markdown 提供模式切换与刷新，源码也能全屏；
 * 视口模拟和「新标签页」是 iframe 独有的。 */
function syncChrome() {
  const dual = FV.kind !== "plain";
  const preview = dual && FV.mode === "preview";
  const frame = FV.kind === "html" && preview;
  $("fv-modes").classList.toggle("hidden", !dual);
  $("fv-vps").classList.toggle("hidden", !frame);
  $("fv-auto-wrap").classList.toggle("hidden", !preview);
  $("fv-reload").classList.toggle("hidden", !preview);
  $("fv-newtab").classList.toggle("hidden", !frame);
  $("fv-full").classList.toggle("hidden", !dual && !FV.srcLoaded);
  $("fv-mode-view").classList.toggle("active", FV.mode === "preview");
  $("fv-mode-src").classList.toggle("active", FV.mode === "source");
  // iframe 预览态没有「保存」可言，避免和 fv-state 的提示打架；Markdown 预览
  // 画的就是编辑器内容，留着保存键才能改完直接存。
  $("fv-save").classList.toggle("hidden", frame);
}

/* ---------------- 打开 ---------------- */

export async function openPreview(fullRel: string, ent?: FileEntry, scope: FileScope = S.fileScope) {
  FV.path = fullRel;
  FV.scope = scope;
  FV.name = fullRel.split("/").pop() || fullRel;
  FV.dirty = false;
  FV.srcLoaded = false;
  FV.url = "";
  FV.mtime = ent?.mtime || "";
  if (FV.blobURL) { URL.revokeObjectURL(FV.blobURL); FV.blobURL = ""; }

  const ext = (FV.name.split(".").pop() || "").toLowerCase();
  FV.kind = HTML_EXT.includes(ext) ? "html" : MD_EXT.includes(ext) ? "md" : "plain";
  FV.mode = FV.kind === "plain" ? "source" : "preview";

  $("fv-name").textContent = FV.name;
  setMeta(ent);
  $("fv-state").textContent = "";
  $<HTMLButtonElement>("fv-save").disabled = true;
  $("fv-notice").replaceChildren(withSpin("加载中…"));
  fvShow("notice");
  syncChrome();
  $<HTMLDialogElement>("dlg-file").showModal();

  // 从对话点进来时没有 ent，补一次 stat 把大小/权限填上
  const fillMeta = () => {
    if (ent) return;
    statFile().then((e) => { if (e && FV.path === fullRel) { setMeta(e); FV.mtime = e.mtime; } });
  };

  if (FV.kind === "html") {
    await mountFrame();
    startPoll();
    fillMeta();
    return;
  }
  if (FV.kind === "md") {
    // 源码本来就要拉：预览和编辑共用同一份文本。没有 ent 时先 stat 一次拿大小，
    // 顺手把元信息也补了，不用再发第二次列目录。
    let info: FileEntry | null = ent || null;
    if (!info) {
      info = await statFile();
      if (FV.path !== fullRel) return; // 期间又点开了别的文件
      if (info) { setMeta(info); FV.mtime = info.mtime; }
    }
    await loadSource(info?.size ?? 0);
    if (FV.path !== fullRel) return;
    if (FV.srcLoaded) {
      renderMD();
      $("fv-state").textContent = ""; // loadSource 留下的「可编辑」是源码态的说明
      startPoll();
    } else {
      // 太大 / 是二进制：loadSource 已经把原因写在提示区了，别再留一个点不开的「预览」
      FV.kind = "plain";
      FV.mode = "source";
    }
    syncChrome();
    return;
  }
  if (IMG_EXT.includes(ext)) {
    try {
      const res = await authFetch(fileAPI());
      FV.blobURL = URL.createObjectURL(await res.blob());
      $<HTMLImageElement>("fv-img").src = FV.blobURL;
      fvShow("img");
    } catch (e) {
      notice("图片加载失败：" + (e as Error).message);
    }
    return;
  }
  await loadSource(ent?.size ?? 0);
}

function setMeta(ent?: FileEntry) {
  const rootName = FV.scope === "shared" ? "/shared" : "/workspace";
  const tail = ent ? ` · ${ent.mode || ""} · ${fmtSize(ent.size)}` : "";
  $("fv-meta").textContent = `${rootName}/${FV.path}${tail}`;
}

async function authFetch(url: string) {
  const res = await fetch(url, { headers: { Authorization: "Bearer " + S.token } });
  if (!res.ok) throw new Error(res.statusText);
  return res;
}

/* 目录列表里捞出本文件的条目：既用于补元信息，也用于轮询 mtime */
async function statFile(): Promise<FileEntry | null> {
  const cut = FV.path.lastIndexOf("/");
  const dir = cut < 0 ? "" : FV.path.slice(0, cut);
  const name = cut < 0 ? FV.path : FV.path.slice(cut + 1);
  try {
    const list = await api<FileEntry[]>(
      `/sessions/${S.current!.id}/files?path=${encodeURIComponent(dir)}${qs()}`,
    );
    return list.find((e) => e.name === name) || null;
  } catch (_) {
    return null;
  }
}

/* ---------------- 源码态 ---------------- */

async function loadSource(size: number) {
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
    $<HTMLTextAreaElement>("fv-editor").value = new TextDecoder("utf-8").decode(buf);
    sourceEditor.load(FV.name);
    FV.srcLoaded = true;
    syncChrome();
    fvShow("editor");
    $("fv-state").textContent = "可编辑";
  } catch (e) {
    notice("读取失败：" + (e as Error).message);
  }
}

/* ---------------- Markdown 渲染态 ---------------- */

/* 画的是编辑器里的当前文本（含未保存的改动），改一行切过来就能看到效果。
 * front matter 单独列成小标签，免得开头的 --- 被当成分隔线加几行碎文字。
 * 重画保留滚动位置：Agent 正在往长文档里追加时，自动刷新不该把人踢回开头。 */
function renderMD() {
  const wrap = $("fv-mdwrap");
  const top = wrap.scrollTop;
  const { meta, body } = splitFrontMatter($<HTMLTextAreaElement>("fv-editor").value);
  const parts: Node[] = [];
  if (meta.length) parts.push(frontMatterChips(meta));
  parts.push(formatText(body, { img: resolveMDImg }));
  $("fv-md").replaceChildren(...parts);
  fvShow("md");
  wrap.scrollTop = top;
}

/* md 里的图片地址 → 能取到的 URL。相对路径按 md 文件所在目录解析成文件接口
 * 直链（token 走查询串：<img> 发不了 Authorization 头）；http(s)、data:image
 * 原样放行；javascript: 之类的伪协议和够不着的容器绝对路径一律返回空串，
 * 由渲染层退回成原样文字。 */
function resolveMDImg(src: string): string {
  const raw = src.trim();
  if (!raw || !S.current) return "";
  if (/^(https?:\/\/|data:image\/)/i.test(raw)) return raw;
  if (/^[a-z][a-z0-9+.-]*:/i.test(raw)) return "";
  let scope = FV.scope;
  let rel = "";
  if (raw.startsWith("/workspace/")) { scope = "workspace"; rel = raw.slice("/workspace/".length); }
  else if (raw.startsWith("/shared/")) { scope = "shared"; rel = raw.slice("/shared/".length); }
  else if (raw.startsWith("/")) return ""; // 容器里的其它绝对路径，文件接口够不着
  else rel = joinFrom(FV.path, raw);
  if (!rel) return "";
  const sq = scope === "shared" ? "&scope=shared" : "";
  return `/api/sessions/${S.current.id}/file?path=${encodeURIComponent(rel)}` +
    `&token=${encodeURIComponent(S.token)}${sq}`;
}

/* 相对 base 文件所在目录解析 src，顺手把 . / .. 折掉；爬出根目录返回空串 */
function joinFrom(base: string, src: string) {
  const dir = base.split("/").slice(0, -1);
  const out: string[] = [];
  for (const seg of dir.concat(src.split(/[/\\]/))) {
    if (!seg || seg === ".") continue;
    if (seg === "..") { if (!out.length) return ""; out.pop(); continue; }
    out.push(seg);
  }
  return out.join("/");
}

/* 重新从磁盘拉一遍源码再画（「刷新」按钮和自动刷新都走这里） */
async function reloadMD(known?: FileEntry | null) {
  const ent = known === undefined ? await statFile() : known;
  if (ent) { FV.mtime = ent.mtime; setMeta(ent); }
  await loadSource(ent?.size ?? 0);
  if (!FV.srcLoaded) return; // 读失败/被换成二进制：提示区已经说明原因
  FV.dirty = false;
  $<HTMLButtonElement>("fv-save").disabled = true;
  if (FV.mode === "preview") renderMD();
}

/* ---------------- 预览态 ---------------- */

async function mountFrame() {
  try {
    const g = await api<{ url: string }>(
      `/sessions/${S.current!.id}/preview?path=${encodeURIComponent(FV.path)}${qs()}`,
    );
    FV.url = g.url;
    reloadFrame();
    fvShow("frame");
    $("fv-state").textContent = "";
  } catch (e) {
    notice("预览失败：" + (e as Error).message);
  }
}

/* 通行证响应带 no-store，重新赋 src 就是一次真刷新；查询串不参与相对路径解析，
 * 所以 cache-buster 不会打乱页面里 ./xxx 的引用。 */
function reloadFrame() {
  if (!FV.url) return;
  $<HTMLIFrameElement>("fv-frame").src = FV.url + "?_=" + ++FV.reloadSeq;
}

function setViewport(vp: Viewport) {
  const wrap = $("fv-framewrap");
  wrap.dataset.vp = vp;
  for (const b of $("fv-vps").querySelectorAll<HTMLButtonElement>("[data-vp]")) {
    b.classList.toggle("active", b.dataset.vp === vp);
  }
}

/* Agent 写完文件 → mtime 变 → 自动重渲染。勾掉自动刷新就只提示不动画面，
 * 免得正在里面点着看的时候被踹回首屏；本地有未保存的改动时同样只提示，
 * 重新拉源码会把人正在写的内容冲掉。 */
function startPoll() {
  stopPoll();
  FV.poll = setInterval(async () => {
    if (FV.kind === "plain" || FV.mode !== "preview") return;
    const ent = await statFile();
    if (!ent || !ent.mtime || ent.mtime === FV.mtime) return;
    FV.mtime = ent.mtime;
    setMeta(ent);
    // md 自动刷新要重拉源码，本地有未保存改动时只提示不覆盖；iframe 重渲染不碰
    // 编辑器，照刷不误
    const keep = FV.kind === "md" && FV.dirty;
    if ($<HTMLInputElement>("fv-auto").checked && !keep) {
      if (FV.kind === "md") await reloadMD(ent);
      else reloadFrame();
      $("fv-state").textContent = "已更新 " + fmtClock(Date.now());
    } else {
      $("fv-state").textContent = keep
        ? "文件已在外部更新，保存会覆盖"
        : "文件已更新，点「刷新」查看";
    }
  }, POLL_MS);
}

function stopPoll() {
  if (FV.poll) clearInterval(FV.poll);
  FV.poll = 0;
}

async function setMode(mode: FvMode) {
  if (FV.kind === "plain" || FV.mode === mode) return;
  FV.mode = mode;
  syncChrome();
  if (mode === "preview") {
    if (FV.kind === "md") {
      renderMD();
      $("fv-state").textContent = FV.dirty ? "未保存" : "";
      return;
    }
    if (FV.url) { fvShow("frame"); reloadFrame(); } else await mountFrame();
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
  $<HTMLButtonElement>("fv-save").disabled = false;
  $("fv-state").textContent = "未保存";
}

$("fv-editor").addEventListener("input", markDirty);

/* Tab 键在编辑器里插入两个空格 */
$("fv-editor").addEventListener("keydown", (e) => {
  if (e.key === "Tab") {
    e.preventDefault();
    const t = e.target as HTMLTextAreaElement, s = t.selectionStart;
    t.setRangeText("  ", s, t.selectionEnd, "end");
    sourceEditor.update();
    markDirty();
  }
  if ((e.ctrlKey || e.metaKey) && e.key === "s") {
    e.preventDefault();
    saveFile();
  }
});

async function saveFile() {
  if (!FV.path || $<HTMLButtonElement>("fv-save").disabled) return;
  $<HTMLButtonElement>("fv-save").disabled = true;
  $("fv-state").replaceChildren(withSpin("保存中…"));
  try {
    const res = await fetch(fileAPI(), {
      method: "PUT",
      headers: { Authorization: "Bearer " + S.token },
      body: $<HTMLTextAreaElement>("fv-editor").value,
    });
    if (!res.ok) {
      let msg = res.statusText;
      try { msg = ((await res.json()) as { error?: string }).error || msg; } catch (_) {}
      throw new Error(msg);
    }
    FV.dirty = false;
    $("fv-state").textContent = "已保存 " + fmtClock(Date.now());
    // 自己刚写的内容不该在下一次轮询里再被当成「外部改动」提示一遍
    const ent = await statFile();
    if (ent) { FV.mtime = ent.mtime; setMeta(ent); }
    loadFiles(); // 刷新大小/时间
  } catch (e) {
    $<HTMLButtonElement>("fv-save").disabled = false;
    $("fv-state").textContent = "保存失败：" + (e as Error).message;
  }
}

/* ---------------- 头部动作 ---------------- */

$("fv-save").addEventListener("click", saveFile);
$("fv-mode-view").addEventListener("click", () => setMode("preview"));
$("fv-mode-src").addEventListener("click", () => setMode("source"));
$("fv-reload").addEventListener("click", async () => {
  if (FV.kind === "md") {
    // Markdown 刷新 = 重新拉源码，会盖掉编辑器里没保存的东西，先问一句
    if (FV.dirty) {
      const ok = await askConfirm("重新载入会丢弃未保存的修改，确定继续？", {
        title: "重新载入", hint: "弹窗里改的内容会被磁盘上的版本覆盖。",
        okLabel: "丢弃并重载", icon: "refresh", danger: true,
      });
      if (!ok) return;
    }
    await reloadMD();
  } else {
    reloadFrame();
  }
  $("fv-state").textContent = "已刷新 " + fmtClock(Date.now());
});
$("fv-newtab").addEventListener("click", () => {
  if (FV.url) window.open(FV.url, "_blank", "noopener");
});
$("fv-full").addEventListener("click", () => {
  const full = $("dlg-file").classList.toggle("fv-max");
  actionButton($("fv-full"), full ? "还原" : "全屏", full ? "collapse" : "expand", full ? "退出全屏，恢复预览窗口" : "全屏预览");
});
$("fv-vps").addEventListener("click", (e) => {
  const b = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-vp]");
  if (b) setViewport(b.dataset.vp as Viewport);
});

$("fv-download").addEventListener("click", () => {
  if (FV.path) startDownload(fileDownloadURL(FV.path, FV.scope));
});

/* ---------------- 关闭 ---------------- */

async function closePreview() {
  if (FV.dirty) {
    const ok = await askConfirm("有未保存的修改，确定关闭？", {
      title: "放弃修改", hint: "关闭后未保存的内容会丢失。", okLabel: "放弃并关闭", icon: "close", danger: true,
    });
    if (!ok) return;
  }
  stopPoll();
  FV.dirty = false;
  FV.url = "";
  if (FV.blobURL) { URL.revokeObjectURL(FV.blobURL); FV.blobURL = ""; }
  $<HTMLTextAreaElement>("fv-editor").value = "";
  sourceEditor.load("");
  $("fv-md").replaceChildren();
  $<HTMLIFrameElement>("fv-frame").src = "about:blank"; // 别让原型在后台继续跑
  $("dlg-file").classList.remove("fv-max");
  actionButton($("fv-full"), "全屏", "expand", "全屏预览");
  $<HTMLDialogElement>("dlg-file").close();
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
function previewTarget(raw: string): { rel: string; scope: FileScope } | null {
  const p = String(raw || "").trim();
  if (!/\.html?$/i.test(p)) return null;
  if (p.startsWith("/workspace/")) return { rel: p.slice("/workspace/".length), scope: "workspace" };
  if (p.startsWith("/shared/")) return { rel: p.slice("/shared/".length), scope: "shared" };
  if (!p.startsWith("/")) return { rel: p.replace(/^\.\//, ""), scope: "workspace" };
  return null;
}

/* 对话流是流式追加的，用事件委托而不是逐条挂监听 */
$("chat-log").addEventListener("click", (e) => {
  const el = (e.target as HTMLElement).closest<HTMLElement>("[data-html-preview]");
  if (!el || !S.current) return;
  const t = previewTarget(el.dataset.htmlPreview || "");
  if (t) openPreview(t.rel, undefined, t.scope);
});
