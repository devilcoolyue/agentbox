import { openDiagnostics } from "../../diagnostics.js";
import { $, toast, fmtTime, fmtBytes, askConfirm } from "../../util.js";
import { t, setTextRender } from "../../i18n.js";
/** Administration actions share the settings login lifetime, including modal
 * confirmations and downloads. A late response may not act for the next user. */
export function initSettingsMaintenance(signal, scope) {
    $("btn-environment-check").addEventListener("click", () => openDiagnostics(), { signal });
    $("btn-diagnostics").addEventListener("click", async () => {
        try {
            const data = await scope.request('/diagnostics');
            if (!scope.current())
                return;
            const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }));
            const link = document.createElement('a');
            link.href = url;
            link.download = 'agentbox-diagnostics.json';
            link.click();
            setTimeout(() => URL.revokeObjectURL(url), 1000);
        }
        catch (error) {
            if (scope.current())
                toast(error.message, true);
        }
    }, { signal });
    $("btn-storage").addEventListener("click", async () => {
        try {
            const report = await scope.request('/storage');
            if (!report)
                return;
            setTextRender($("storage-report"), () => {
                if (!report.data.scanned_at)
                    return t("统计中，请稍后刷新");
                const labels = { users: t("用户数据"), backups: t("备份"), other: t("其他数据"), tools_public: t("公共工具"), tools_private: t("用户缓存"), releases: t("发布版本") };
                const policy = report.storage_policy;
                const lines = [t("数据文件 {p0} · 磁盘可用 {p1} · {p2}{p3}\n", { p0: String(fmtBytes(report.data.bytes)), p1: String(fmtBytes(report.disk_available)), p2: String(fmtTime(report.data.scanned_at)), p3: report.data.partial ? t("（统计未完整）") : "" })];
                lines.push(Object.entries(report.data.categories || {}).map(([kind, bytes]) => `${labels[kind] || kind}: ${fmtBytes(bytes)}`).join(' · '));
                for (const [name, bytes] of Object.entries(report.data.users || {})) {
                    const limit = policy?.users?.[name] || policy?.user_bytes || 0;
                    lines.push(`${name}: ${fmtBytes(bytes)}` + (limit ? ` / ${t("数据限额")} ${fmtBytes(limit)} · ${t("缓存限额")} ${fmtBytes(policy?.cache_users?.[name] || policy?.cache_bytes || 0)}` : ''));
                }
                if (policy?.release_keep)
                    lines.push(t("保留最近 {p0} 个发布版本", { p0: String(policy.release_keep) }));
                if (policy?.cache_max_age_days)
                    lines.push(t("可重建缓存保留 {p0} 天，仅在空间停止时回收", { p0: String(policy.cache_max_age_days) }));
                return lines.join('\n');
            });
        }
        catch (error) {
            if (scope.current())
                toast(error.message, true);
        }
    }, { signal });
    $("btn-clear-cache").addEventListener("click", async () => {
        if (!await askConfirm(() => t("清理官方市场的下载缓存？下次打开市场会重新下载。"), { icon: 'trash', get okLabel() { return t("清理"); } }) || !scope.current())
            return;
        try {
            await scope.request('/cache/marketplace', { method: 'DELETE' });
            if (scope.current())
                toast(t("缓存已清理"));
        }
        catch (error) {
            if (scope.current())
                toast(error.message, true);
        }
    }, { signal });
}
