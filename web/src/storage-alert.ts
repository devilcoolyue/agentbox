import { t as i18nText } from "./i18n.js";
import { fmtBytes, toast } from "./util.js";
import type { Me } from "./types.js";

export function diskLabel(roles: string[]): string {
  const names: Record<string, string> = { system: i18nText("系统盘"), data: i18nText("用户数据"), tools: i18nText("工具与缓存") };
  return roles.map((role) => names[role] || role).join(" / ");
}

let warned = "";

/* /me 每个回合结束都会刷新，所以只在偏低的磁盘变化时提示一次，不反复打断。 */
export function warnLowStorage(low: Me["storage_low"]): void {
  const key = low?.disks.map((disk) => disk.roles.join("/")).join(",") || "";
  if (low && key && key !== warned) {
    const disk = low.disks[0]!;
    toast(i18nText("磁盘空间偏低：{p0} 仅剩 {p1}。低于 {p2} 后将停止新建空间和上传，请尽快清理。", {
      p0: diskLabel(disk.roles), p1: String(fmtBytes(disk.available)), p2: String(fmtBytes(low.reserve_bytes)),
    }), true);
  }
  warned = key;
}
