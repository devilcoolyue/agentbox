import { api } from "../../api.js";
import { S } from "../../state.js";
import { $, toast, fmtTime, askConfirm } from "../../util.js";
import { setSelectValue } from "../../select.js";
import { Poller } from "../../shared/poller.js";
import { settingsState } from "./state.js";
import { dirtyGroups, rebaseline } from "./savebar.js";
import type { ImageUpdateSettings } from "../../types.js";

type UpdateView = {
 settings: ImageUpdateSettings; agent_image: string; previous_image: string; timezone: string;
 status: { running: boolean; phase: string; action: string; error: string; log: string; base_image: string;
  started_at: number; finished_at: number; checked_at: number; available: boolean; result_image: string;
  current: { claude: string; codex: string }; target: { claude: string; codex: string } };
};
const poller = new Poller();
let pending = false;
// 是否有没保存的更新设置，以统一保存条的比较结果为准
const isDirty = () => dirtyGroups().some((g) => g.id === "image-updates");
let latest: UpdateView | null = null;

export function fillImageUpdateSettings(p?: ImageUpdateSettings) {
 p ??= {enabled:false, channel:"stable", time:"04:00", update_codex:false};
 setSelectValue($<HTMLSelectElement>("image-update-enabled"), p.enabled ? "on" : "off");
 setSelectValue($<HTMLSelectElement>("image-update-channel"), p.channel);
 $<HTMLInputElement>("image-update-time").value = p.time;
 setSelectValue($<HTMLSelectElement>("image-update-codex"), p.update_codex ? "on" : "off");
 rebaseline(["image-update-enabled", "image-update-channel", "image-update-time", "image-update-codex"]);
 buttons();
}
function buttons() {
 const dirty = isDirty();
 $("image-update-dirty").textContent = dirty ? "上方的更新设置还没保存，保存后才能执行下面的操作。" : "";
 $("image-update-dirty").hidden = !dirty;
 for (const action of ["check", "update", "rollback"]) {
  $<HTMLButtonElement>("image-update-" + action).disabled = pending || dirty || !!latest?.status.running || !latest || (action === "rollback" && !latest.previous_image);
 }
}
function render(v: UpdateView) {
 latest = v;
 const input = $<HTMLInputElement>("set-image");
 if (settingsState.value && input.value === settingsState.value.agent_image) {
  input.value = v.agent_image; settingsState.value.agent_image = v.agent_image;
  rebaseline(["set-image"]); // 镜像是更新任务换的，不算用户的未保存改动
 }
 if (!isDirty()) fillImageUpdateSettings(v.settings);
 const st = v.status;
 const labels: Record<string, string> = {checking:"正在检查", building:"正在构建并验证镜像", failed:"任务失败", done:"任务完成"};
 $("image-update-status").textContent = (labels[st.phase] || "尚未检查客户端版本") + (st.available ? " · 有可用更新" : "") + (st.finished_at ? " · " + fmtTime(st.finished_at) : "") + (st.error ? "：" + st.error : "");
 $("image-update-versions").textContent = st.current.claude ? `上次检查：Claude ${st.current.claude} / Codex ${st.current.codex}` + (st.target.claude ? ` → 目标 Claude ${st.target.claude} / Codex ${st.target.codex}` : "") : "点击“立即检查”读取镜像版本和所选渠道的最新版本。";
 $("image-update-active").textContent = "当前镜像：" + v.agent_image;
 $("image-update-schedule").textContent = `系统时区 ${v.timezone} · ${v.settings.enabled ? `每天 ${v.settings.time} 自动检查并应用，当天错过后补跑` : "自动更新已关闭"}。stable 可能落后于 latest；不会自动降级。`;
 $("image-update-log").textContent = st.log || "暂无日志";
 buttons();
}
async function refresh(signal: AbortSignal) {
 const v = await api<UpdateView>("/image-updates", {signal});
 if (!signal.aborted && S.view === "settings" && S.sec === "container") render(v);
}
export function startImageUpdates() {
 poller.start(3000, async signal => {
  if (S.view !== "settings" || S.sec !== "container" || S.role !== "admin") { stopImageUpdates(); return; }
  if (document.hidden) return;
  try { await refresh(signal); } catch (e) { if (!signal.aborted) $("image-update-status").textContent = "读取更新状态失败：" + (e as Error).message; }
 });
}
export function stopImageUpdates() { poller.stop(); }

export function initImageUpdates(signal: AbortSignal) {
 latest = null; pending = false; buttons();
 for (const field of ["enabled", "channel", "time", "codex"]) {
  for (const ev of ["input", "change"]) $("image-update-" + field).addEventListener(ev, () => buttons(), {signal});
 }
 for (const action of ["check", "update", "rollback"]) {
  $("image-update-" + action).addEventListener("click", async () => {
   if (pending || isDirty() || latest?.status.running) return;
   pending = true; buttons();
   try {
    if (action !== "check" && !await askConfirm(action === "rollback" ? "回退到上次镜像并暂停自动更新？" : "按已保存的渠道检查、构建并应用客户端更新？", {title:"客户端更新",hint:"运行中的空间保持不变，停止再启动后使用切换后的镜像。",okLabel:action === "rollback" ? "回退" : "更新"})) return;
    if (signal.aborted) return;
    const v = await api<UpdateView>("/image-updates/" + action, {method:"POST",signal});
    if (!signal.aborted) { render(v); toast("任务已启动，可在此查看进度"); }
   } catch (e) { if (!signal.aborted) toast((e as Error).message, true); }
   finally { if (!signal.aborted) { pending = false; buttons(); } }
  }, {signal});
 }
}
