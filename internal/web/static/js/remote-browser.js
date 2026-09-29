import { agentIcon } from "./brand.js";
import { S, bus } from "./state.js";
import { api, wsURL } from "./api.js";
import { enhanceSelects, setSelectValue } from "./select.js";
import { $, toast } from "./util.js";
let desktop = null;
let generation = 0;
let controller = null;
let connected = false;
let busy = false;
const qualityModes = {
    smooth: { quality: 3, compression: 2 },
    balanced: { quality: 6, compression: 2 },
    clear: { quality: 9, compression: 2 },
};
function applyQuality(view) {
    const mode = qualityModes[$("browser-quality").value] || qualityModes.smooth;
    view.qualityLevel = mode.quality;
    view.compressionLevel = mode.compression;
}
function state(text) {
    $("browser-state").textContent = text;
    $("browser-dot").dataset.state = busy ? "busy" : connected ? "connected" : "idle";
}
function controls() {
    $("browser-dot").dataset.state = busy ? "busy" : connected ? "connected" : "idle";
    $("browser-stop").disabled = busy;
    $("browser-start").disabled = busy;
    $("browser-open").disabled = busy;
    $("browser-paste").disabled = !connected;
    $("browser-copy").disabled = !connected;
}
export function browserDisconnect() {
    generation++;
    const old = desktop;
    desktop = null;
    connected = false;
    busy = false;
    old?.disconnect();
    $("browser-screen").replaceChildren();
    $("browser-empty").classList.remove("hidden");
    controls();
}
async function connect(id, ticket, info) {
    // Keep native module imports so noVNC assets share Agentbox's content hash.
    const { default: RFB } = await import("../vendor/novnc/core/rfb.js");
    if (ticket !== generation || S.current?.id !== id || S.tab !== "browser")
        return;
    const view = new RFB($("browser-screen"), wsURL(`/sessions/${id}/browser/desktop`));
    desktop = view;
    view.scaleViewport = true;
    view.resizeSession = false;
    applyQuality(view);
    view.addEventListener("connect", () => {
        if (desktop !== view)
            return;
        connected = true;
        state(`已连接 · ${info.browser || "Chrome"} · ${info.proxy ? "账号代理出口" : "服务器出口"}`);
        $("browser-empty").classList.add("hidden");
        controls();
    });
    view.addEventListener("disconnect", () => {
        if (desktop !== view)
            return;
        desktop = null;
        connected = false;
        state("连接已断开，可点击连接浏览器重试");
        $("browser-screen").replaceChildren();
        $("browser-empty").classList.remove("hidden");
        controls();
    });
    view.addEventListener("securityfailure", () => { if (desktop === view)
        state("远程桌面连接失败，请重新启动浏览器"); });
}
export async function showBrowser() {
    browserDisconnect();
    const id = S.current?.id;
    if (!id)
        return;
    const ticket = generation;
    state("正在读取浏览器状态…");
    try {
        const info = await api(`/sessions/${id}/browser`);
        if (ticket !== generation || S.current?.id !== id)
            return;
        if (info.running) {
            state(`正在连接 ${info.browser}…`);
            await connect(id, ticket, info);
        }
        else
            state("浏览器未启动 · 点击启动后继续上次的登录状态");
    }
    catch (e) {
        if (ticket === generation)
            state(e.message);
    }
}
async function startBrowser(url = "") {
    if (busy || !S.current)
        return;
    const id = S.current.id;
    const ticket = generation;
    busy = true;
    controls();
    state("正在启动浏览器…");
    try {
        const info = await api(`/sessions/${id}/browser`, { method: "POST", body: JSON.stringify({ url }) });
        if (ticket !== generation || S.current?.id !== id || S.tab !== "browser")
            return;
        state(`${info.browser || "Chrome"} · ${info.proxy ? "账号代理出口" : "服务器出口"}`);
        if (!desktop)
            await connect(id, ticket, info);
    }
    catch (e) {
        if (ticket === generation) {
            state(e.message);
            toast(e.message, true);
        }
    }
    finally {
        if (ticket === generation) {
            busy = false;
            controls();
        }
    }
}
export function initRemoteBrowser() {
    controller?.abort();
    controller = new AbortController();
    const { signal } = controller;
    for (const button of document.querySelectorAll("[data-browser-brand]")) {
        if (!button.querySelector(".agent-ico"))
            button.prepend(agentIcon(button.dataset.browserBrand, 16));
    }
    const quality = $("browser-quality");
    let saved = "smooth";
    try {
        saved = localStorage.getItem("agentbox_browser_quality") || saved;
    }
    catch { /* private mode */ }
    setSelectValue(quality, qualityModes[saved] ? saved : "smooth");
    enhanceSelects($("tab-browser"));
    quality.addEventListener("change", () => {
        if (desktop)
            applyQuality(desktop);
        try {
            localStorage.setItem("agentbox_browser_quality", quality.value);
        }
        catch { /* private mode */ }
    }, { signal });
    bus.addEventListener("view-changed", () => {
        if (S.view !== "work")
            browserDisconnect();
        else if (S.current && S.tab === "browser")
            void showBrowser();
    }, { signal });
    $("browser-start").addEventListener("click", () => void startBrowser(), { signal });
    $("browser-address").addEventListener("submit", e => {
        e.preventDefault();
        const input = $("browser-url");
        let url = input.value.trim();
        if (!url)
            return;
        if (!/^[a-z][a-z\d+.-]*:/i.test(url))
            url = "https://" + url;
        try {
            const parsed = new URL(url);
            if (!["http:", "https:"].includes(parsed.protocol) || parsed.username || parsed.password)
                throw new Error();
        }
        catch {
            toast("请输入有效的 HTTP / HTTPS 地址", true);
            return;
        }
        void startBrowser(url);
    }, { signal });
    for (const button of document.querySelectorAll("[data-browser-url]")) {
        button.addEventListener("click", () => void startBrowser(button.dataset.browserUrl), { signal });
    }
    $("browser-stop").addEventListener("click", async () => {
        if (!S.current || busy)
            return;
        const id = S.current.id;
        browserDisconnect();
        const ticket = generation;
        busy = true;
        controls();
        state("正在关闭浏览器…");
        try {
            await api(`/sessions/${id}/browser`, { method: "DELETE" });
            if (ticket === generation)
                state("浏览器已关闭 · 登录状态已保留");
        }
        catch (e) {
            if (ticket === generation)
                state(e.message);
        }
        finally {
            if (ticket === generation) {
                busy = false;
                controls();
            }
        }
    }, { signal });
    $("browser-fullscreen").addEventListener("click", async () => {
        try {
            if (document.fullscreenElement)
                await document.exitFullscreen();
            else
                await $("tab-browser").requestFullscreen();
        }
        catch {
            toast("当前浏览器不支持全屏", true);
        }
    }, { signal });
    $("browser-copy").addEventListener("click", async () => {
        const id = S.current?.id, ticket = generation;
        if (!id || !connected)
            return;
        try {
            const result = await api(`/sessions/${id}/browser/clipboard`);
            if (ticket === generation)
                $("browser-clipboard").value = result.text;
        }
        catch (e) {
            toast(e.message, true);
        }
    }, { signal });
    $("browser-paste").addEventListener("click", async () => {
        const view = desktop, id = S.current?.id;
        if (!view || !connected || !id)
            return;
        try {
            await api(`/sessions/${id}/browser/clipboard`, { method: "POST", body: JSON.stringify({ text: $("browser-clipboard").value }) });
        }
        catch (e) {
            toast(e.message, true);
            return;
        }
        if (desktop !== view)
            return;
        view.focus();
        // Deliberate click only: never read the local clipboard automatically.
        view.sendKey(0xffe3, "ControlLeft", true);
        view.sendKey(0x76, "KeyV");
        view.sendKey(0xffe3, "ControlLeft", false);
    }, { signal });
    return () => { controller?.abort(); controller = null; browserDisconnect(); $("browser-clipboard").value = ""; };
}
