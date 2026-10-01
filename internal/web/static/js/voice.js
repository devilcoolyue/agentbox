/* voice：语音输入。用浏览器内置的 Web Speech API 识别（Chrome/Edge/Safari；
 * Firefox 不支持则隐藏入口）。点击开始录音，边说边把文字实时写进输入框，
 * 再点一次停止并直接发送；Agent 执行中则把文字留在输入框里。 */
"use strict";
import { S } from "./state.js";
import { $, toast, isImeEnter, enterInsertsNewline } from "./util.js";
import { sendChat, autoGrow } from "./chat.js";
import { setTip } from "./tip.js";
const SR = window.SpeechRecognition || window.webkitSpeechRecognition;
const btn = $("btn-voice");
const input = $("chat-input");
const IDLE_TITLE = "语音输入：点击说话，再点一次停止并发送";
let rec = null; // 当前识别实例，非 null 即录音中
let base = ""; // 录音开始时输入框已有的内容
let finalText = ""; // 已定稿的识别结果（interim 部分每次整体重写）
let sendOnEnd = false; // 用户主动点停：识别收尾后自动发送
if (!SR)
    btn.classList.add("hidden");
btn.addEventListener("click", () => {
    if (rec)
        stopAndSend();
    else
        start();
});
function start() {
    const r = new SR();
    // 界面是中文产品，识别语言跟随浏览器的中文区域设置，非中文环境兜底 zh-CN
    r.lang = /^zh/i.test(navigator.language || "") ? navigator.language : "zh-CN";
    r.continuous = true;
    r.interimResults = true;
    base = input.value && !/\s$/.test(input.value) ? input.value + " " : input.value;
    finalText = "";
    sendOnEnd = false;
    let failed = false; // 出错后 onend 不再自动续听
    r.onresult = (e) => {
        let interim = "";
        for (let i = e.resultIndex; i < e.results.length; i++) {
            const res = e.results[i];
            if (res.isFinal)
                finalText += res[0].transcript;
            else
                interim += res[0].transcript;
        }
        input.value = base + finalText + interim;
        autoGrow();
    };
    r.onerror = (e) => {
        if (e.error === "no-speech" || e.error === "aborted")
            return;
        failed = true;
        const msg = {
            "not-allowed": "麦克风权限被拒绝，请在浏览器允许（非 localhost 访问需 HTTPS）",
            "service-not-allowed": "浏览器禁用了语音识别服务（非 localhost 访问需 HTTPS）",
            "audio-capture": "未检测到可用的麦克风",
            network: "语音识别网络异常（识别由浏览器云端服务完成）",
        }[e.error] || "语音识别出错：" + e.error;
        toast(msg, true);
    };
    r.onend = () => {
        if (rec !== r)
            return; // 已被手动发送等路径掐掉
        // 静音等原因自动结束：用户没点停就悄悄续上
        if (!sendOnEnd && !failed) {
            try {
                r.start();
                return;
            }
            catch (_) { }
        }
        rec = null;
        setRecUI(false);
        const text = input.value.trim();
        if (!sendOnEnd || !text)
            return;
        if (S.chatState === "running")
            toast("Agent 执行中，识别文字已放入输入框");
        else
            sendChat();
    };
    try {
        r.start();
    }
    catch (e) {
        toast("无法启动语音识别：" + e.message, true);
        return;
    }
    rec = r;
    setRecUI(true);
    toast("正在聆听，再点一次 🎤 停止并发送");
}
function stopAndSend() {
    sendOnEnd = true;
    try {
        rec.stop();
    }
    catch (_) { }
}
function setRecUI(on) {
    btn.classList.toggle("rec", on);
    setTip(btn, on ? "停止录音并发送" : IDLE_TITLE);
    btn.ariaLabel = on ? "停止录音" : "语音输入"; // 纯图标按钮，可访问名称得跟着状态走
}
/* 录音途中手动发送（Enter / 发送键）：先掐掉识别，
 * 免得迟到的识别结果把已发送的旧文字又写回输入框 */
function abortIfRecording() {
    if (!rec)
        return;
    const r = rec;
    rec = null;
    setRecUI(false);
    try {
        r.abort();
    }
    catch (_) { }
}
$("chat-send").addEventListener("click", abortIfRecording, true);
input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !isImeEnter(e) && !enterInsertsNewline())
        abortIfRecording();
}, true);
