import { api } from "../../api.js";
import { S, bus } from "../../state.js";
import { $, fmtDateTime } from "../../util.js";
import { setText, setTextRender, t } from "../../i18n.js";
/** Observe only while the about page is visible; update actions remain owned
 * by the existing server/image/native desktop updaters. */
export function initUpdateComponents() {
    const lifetime = new AbortController(), owner = S.token, options = { signal: lifetime.signal };
    const refresh = $("component-refresh");
    let request, visible = false, queued = false;
    const current = () => !lifetime.signal.aborted && S.token === owner && S.role === "admin";
    const shown = () => S.view === "settings" && S.sec === "about";
    const clear = () => { for (const id of ["component-server-version", "component-image-version", "component-desktop-version", "component-image-state", "component-desktop-state"])
        $(id).textContent = ""; };
    const load = async () => {
        request?.abort();
        const controller = request = new AbortController();
        refresh.disabled = true;
        clear();
        setText($("component-read-status"), "正在读取本地版本信息…");
        try {
            const v = await api("/updates/components", { signal: AbortSignal.any([lifetime.signal, controller.signal, AbortSignal.timeout(8000)]) });
            if (!current() || !shown() || request !== controller)
                return;
            if (v.version !== 1 || !v.server || !v.image || !v.desktop)
                throw new Error(t("版本信息格式不受支持，请更新服务端后重试。"));
            setTextRender($("component-server-version"), () => t("当前 {version} · 数据库 schema {schema}", { version: v.server.version, schema: v.server.schema }));
            const img = v.image;
            setTextRender($("component-image-version"), () => img.reference + (img.state === "labels_only" ? "\n" + t("镜像标签：Claude {claude} / Codex {codex}", { claude: img.claude || t("未知"), codex: img.codex || t("未知") }) : ""));
            setText($("component-image-state"), img.state === "labels_only" ? "已读取当前配置镜像的标签；协议兼容性与真实调用未检查，不代表运行中空间的版本。" : img.state === "changed" ? "读取期间镜像配置已变化，请刷新本地版本信息。" : "无法读取配置镜像，版本与兼容性未知；请检查 Docker 和镜像是否存在。");
            setText($("component-desktop-version"), "网页无法读取本机桌面应用版本；请在桌面应用的「设置 → 关于与更新」中查看。");
            setTextRender($("component-desktop-state"), () => t("本服务提供桌面协议 {protocol}；{sync}。实际连接能力请在桌面应用中查看。", { protocol: v.desktop.server_protocol, sync: t(v.desktop.sync_enabled ? "目录同步已启用" : "目录同步未启用") }));
            setTextRender($("component-read-status"), () => t("读取于 {time}；这是本地版本观察，远端更新请使用各自的检查入口。", { time: fmtDateTime(v.observed_at) }));
        }
        catch (error) {
            if (current() && shown() && request === controller)
                setTextRender($("component-read-status"), () => t("读取版本信息失败：") + error.message);
        }
        finally {
            if (request === controller) {
                request = undefined;
                if (current())
                    refresh.disabled = false;
            }
        }
    };
    const navigation = () => {
        if (queued)
            return;
        queued = true;
        queueMicrotask(() => {
            queued = false;
            if (!current())
                return;
            const next = shown();
            if (next && !visible)
                void load();
            if (!next) {
                request?.abort();
                request = undefined;
                refresh.disabled = false;
            }
            visible = next;
        });
    };
    refresh.addEventListener("click", () => { if (current() && shown())
        void load(); }, options);
    $("component-image-open").addEventListener("click", () => { if (current())
        document.querySelector('#set-nav [data-sec="container"]')?.click(); }, options);
    for (const event of ["navigation-changed", "view-changed", "app-ready"])
        bus.addEventListener(event, navigation, options);
    navigation();
    return () => { lifetime.abort(); request?.abort(); clear(); $("component-read-status").textContent = ""; };
}
