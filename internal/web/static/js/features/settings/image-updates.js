import { api } from "../../api.js";
import { S } from "../../state.js";
import { $, toast, fmtTime, askConfirm } from "../../util.js";
import { setSelectValue } from "../../select.js";
import { Poller } from "../../shared/poller.js";
import { settingsState } from "./state.js";
const poller = new Poller();
let dirty = false, pending = false;
let latest = null;
export function fillImageUpdateSettings(p) {
    p ??= { enabled: false, channel: "stable", time: "04:00", update_codex: false };
    setSelectValue($("image-update-enabled"), p.enabled ? "on" : "off");
    setSelectValue($("image-update-channel"), p.channel);
    $("image-update-time").value = p.time;
    setSelectValue($("image-update-codex"), p.update_codex ? "on" : "off");
    dirty = false;
    buttons();
}
function buttons() {
    $("image-update-dirty").textContent = dirty ? "有未保存的设置，请先保存后执行操作。" : "设置保存后即时生效。";
    for (const action of ["check", "update", "rollback"]) {
        $("image-update-" + action).disabled = pending || dirty || !!latest?.status.running || !latest || (action === "rollback" && !latest.previous_image);
    }
    $("image-update-save").disabled = pending;
}
function render(v) {
    latest = v;
    const input = $("set-image");
    if (settingsState.value && input.value === settingsState.value.agent_image) {
        input.value = v.agent_image;
        settingsState.value.agent_image = v.agent_image;
    }
    if (!dirty)
        fillImageUpdateSettings(v.settings);
    const st = v.status;
    const labels = { checking: "正在检查", building: "正在构建并验证镜像", failed: "任务失败", done: "任务完成" };
    $("image-update-status").textContent = (labels[st.phase] || "尚未检查客户端版本") + (st.available ? " · 有可用更新" : "") + (st.finished_at ? " · " + fmtTime(st.finished_at) : "") + (st.error ? "：" + st.error : "");
    $("image-update-versions").textContent = st.current.claude ? `上次检查：Claude ${st.current.claude} / Codex ${st.current.codex}` + (st.target.claude ? ` → 目标 Claude ${st.target.claude} / Codex ${st.target.codex}` : "") : "点击“立即检查”读取镜像版本和所选渠道的最新版本。";
    $("image-update-active").textContent = "当前镜像：" + v.agent_image;
    $("image-update-schedule").textContent = `系统时区 ${v.timezone} · ${v.settings.enabled ? `每天 ${v.settings.time} 自动检查并应用，当天错过后补跑` : "自动更新已关闭"}。stable 可能落后于 latest；不会自动降级。`;
    $("image-update-log").textContent = st.log || "暂无日志";
    buttons();
}
async function refresh(signal) {
    const v = await api("/image-updates", { signal });
    if (!signal.aborted && S.view === "settings" && S.sec === "container")
        render(v);
}
export function startImageUpdates() {
    poller.start(3000, async (signal) => {
        if (S.view !== "settings" || S.sec !== "container" || S.role !== "admin") {
            stopImageUpdates();
            return;
        }
        if (document.hidden)
            return;
        try {
            await refresh(signal);
        }
        catch (e) {
            if (!signal.aborted)
                $("image-update-status").textContent = "读取更新状态失败：" + e.message;
        }
    });
}
export function stopImageUpdates() { poller.stop(); }
export function initImageUpdates(signal) {
    latest = null;
    dirty = false;
    pending = false;
    buttons();
    for (const field of ["enabled", "channel", "time", "codex"]) {
        $("image-update-" + field).addEventListener("input", () => { dirty = true; buttons(); }, { signal });
    }
    $("image-update-save").addEventListener("click", async () => {
        const clock = $("image-update-time");
        if (!clock.reportValidity())
            return;
        pending = true;
        buttons();
        const image_updates = {
            enabled: $("image-update-enabled").value === "on",
            channel: $("image-update-channel").value,
            time: clock.value, update_codex: $("image-update-codex").value === "on",
        };
        try {
            const saved = await api("/settings", { method: "PUT", body: JSON.stringify({ image_updates }), signal });
            if (signal.aborted)
                return;
            // Keep the separately edited container form's image baseline unchanged.
            if (settingsState.value)
                settingsState.value.image_updates = saved.image_updates;
            fillImageUpdateSettings(saved.image_updates);
            await refresh(signal);
            toast("客户端更新设置已保存");
        }
        catch (e) {
            if (!signal.aborted)
                toast(e.message, true);
        }
        finally {
            if (!signal.aborted) {
                pending = false;
                buttons();
            }
        }
    }, { signal });
    for (const action of ["check", "update", "rollback"]) {
        $("image-update-" + action).addEventListener("click", async () => {
            if (pending || dirty || latest?.status.running)
                return;
            pending = true;
            buttons();
            try {
                if (action !== "check" && !await askConfirm(action === "rollback" ? "回退到上次镜像并暂停自动更新？" : "按已保存的渠道检查、构建并应用客户端更新？", { title: "客户端更新", hint: "运行中的空间保持不变，停止再启动后使用切换后的镜像。", okLabel: action === "rollback" ? "回退" : "更新" }))
                    return;
                if (signal.aborted)
                    return;
                const v = await api("/image-updates/" + action, { method: "POST", signal });
                if (!signal.aborted) {
                    render(v);
                    toast("任务已启动，可在此查看进度");
                }
            }
            catch (e) {
                if (!signal.aborted)
                    toast(e.message, true);
            }
            finally {
                if (!signal.aborted) {
                    pending = false;
                    buttons();
                }
            }
        }, { signal });
    }
}
