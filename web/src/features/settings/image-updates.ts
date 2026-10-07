import { setText, setTextRender, t as i18nText } from "../../i18n.js";
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
  validation?: {version:number;image_id:string;checks:{name:string;passed:boolean}[];failure?:string};
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
 setTextRender($("image-update-dirty"), () => dirty ? i18nText("上方的更新设置还没保存，保存后才能执行下面的操作。") : "");
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
 const labels: Record<string, string> = { get checking() { return i18nText("正在检查"); }, get building() { return i18nText("正在构建并验证镜像"); }, get validating() { return i18nText("正在验证 CLI 行为"); }, get failed() { return i18nText("任务失败"); }, get done() { return i18nText("任务完成"); } };
 setTextRender($("image-update-status"), () => (labels[st.phase] || i18nText("尚未检查镜像内 CLI 版本")) + (st.available ? i18nText(" · 有可用更新") : "") + (st.finished_at ? " · " + fmtTime(st.finished_at) : "") + (st.error ? "：" + st.error : ""));
 setTextRender($("image-update-versions"), () => st.current.claude ? i18nText("任务开始时：Claude {p0} / Codex {p1}", { p0: String(st.current.claude), p1: String(st.current.codex) }) + (st.target.claude ? i18nText(" → 目标 Claude {p0} / Codex {p1}", { p0: String(st.target.claude), p1: String(st.target.codex) }) : "") : i18nText("点击“立即检查”读取镜像版本和所选渠道的最新版本。"));
 setTextRender($("image-update-active"), () => i18nText("当前镜像：") + v.agent_image);
 setTextRender($("image-update-schedule"), () => i18nText("系统时区 {p0} · {p1}。stable 可能落后于 latest；不会自动降级。", { p0: String(v.timezone), p1: String(v.settings.enabled ? i18nText("每天 {p0} 自动检查并应用，当天错过后补跑", { p0: String(v.settings.time) }) : i18nText("自动更新已关闭")) }));
 setTextRender($("image-update-validation"), () => {
  const report=st.validation;
  if(!report) return i18nText("候选镜像尚未完成行为验证；版本检查不代表协议兼容。");
  if(report.version!==1) return i18nText("无法识别候选镜像验证结果，请升级服务端后重新检查。");
  if(report.failure||!report.checks?.length||report.checks.some(check=>!check.passed)) return i18nText("候选镜像行为验证未通过，未切换镜像。请查看任务日志。");
  return i18nText("候选镜像的 {p0} 项行为检查通过（合成上游）。这不代表真实账号或模型可用。",{p0:String(report.checks.length)});
 });
 setTextRender($("image-update-log"), () => st.log || i18nText("暂无日志"));
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
  try { await refresh(signal); } catch (e) { if (!signal.aborted) setTextRender($("image-update-status"), () => i18nText("读取更新状态失败：") + (e as Error).message); }
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
    if (action !== "check" && !await askConfirm(() => action === "rollback" ? i18nText("回退到上次镜像并暂停自动更新？") : i18nText("按已保存的渠道检查、构建并应用 Agent 镜像更新？"), {get title() { return i18nText("Agent 镜像更新"); },get hint() { return i18nText("运行中的空间保持不变，停止再启动后使用切换后的镜像。"); },get okLabel() { return action === "rollback" ? i18nText("回退") : i18nText("更新"); }})) return;
    if (signal.aborted) return;
    const v = await api<UpdateView>("/image-updates/" + action, {method:"POST",signal});
    if (!signal.aborted) { render(v); toast(i18nText("任务已启动，可在此查看进度")); }
   } catch (e) { if (!signal.aborted) toast((e as Error).message, true); }
   finally { if (!signal.aborted) { pending = false; buttons(); } }
  }, {signal});
 }
}
