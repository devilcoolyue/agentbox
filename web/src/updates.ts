import { api } from "./api.js";
import { S, emit } from "./state.js";
import { $ } from "./util.js";
import { hideTip } from "./tip.js";
import type { UpdateInfo } from "./types.js";

const interval = 4 * 60 * 60 * 1000;
const releasesURL = "https://github.com/devilcoolyue/agentbox-releases/releases";
const versionLabel = (version: string) => /^\d/.test(version) ? "v" + version : version;

/** App-owned state: logout aborts requests, listeners and scheduled checks. */
export function initUpdates() {
  const badge = $<HTMLButtonElement>("version-badge");
  const menu = $("version-menu");
  badge.classList.toggle("hidden", S.role !== "admin");
  if (S.role !== "admin") return () => {};
  const lifetime = new AbortController();
  const { signal } = lifetime;
  let info: UpdateInfo | undefined;
  let checking = false;
  let error = "";
  let lastAttempt = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;

  function render() {
    const available = !!info?.available;
    const version = info ? versionLabel(info.current_version) : "—";
    let status = "尚未检查更新";
    if (checking) status = "正在检查更新…";
    else if (error) status = available ? `新版本 ${versionLabel(info!.latest_version)} 可用（上次检查结果）` : "检查未完成，请重试";
    else if (available) status = `新版本 ${versionLabel(info!.latest_version)} 可用`;
    else if (info?.checked_at) {
      status = !info.latest_version ? "暂无正式发布的版本" : !info.comparable ? "开发构建，无法比较版本" : "已是最新版本";
    }
    badge.classList.toggle("has-update", available);
    $("version-label").textContent = info ? version : "版本";
    badge.ariaLabel = `当前版本 ${version}，${status}，查看版本与更新`;
    for (const el of document.querySelectorAll<HTMLElement>("[data-update-version]")) el.textContent = version;
    for (const el of document.querySelectorAll<HTMLElement>("[data-update-status]")) {
      el.textContent = status; el.classList.toggle("available", available);
    }
    for (const el of document.querySelectorAll<HTMLElement>("[data-update-error]")) {
      el.textContent = error; el.classList.toggle("hidden", !error);
    }
    for (const el of document.querySelectorAll<HTMLButtonElement>("[data-update-check]")) {
      el.disabled = checking; el.classList.toggle("checking", checking);
    }
    for (const el of document.querySelectorAll<HTMLAnchorElement>("[data-update-release]")) {
      // Release links are constrained even when metadata is stale or malformed.
      const url = info?.release_url || releasesURL;
      el.href = url === releasesURL || url.startsWith(releasesURL + "/tag/") ? url : releasesURL;
    }
    const checked = info?.checked_at ? "上次检查 " + new Date(info.checked_at).toLocaleString("zh-CN", { timeZone: S.timeZone || "Asia/Shanghai", month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false }) : "尚未检查";
    for (const el of document.querySelectorAll<HTMLElement>("[data-update-time]")) el.textContent = checked;
    $("version-current").classList.toggle("hidden", !(info?.checked_at && info.latest_version && info.comparable && !available && !error && !checking));
    $("version-upgrade").classList.toggle("hidden", !available);
    $("update-build").textContent = info ? [info.built_at !== "unknown" ? "构建于 " + info.built_at : "", info.revision !== "unknown" ? "提交 " + info.revision.slice(0, 12) : ""].filter(Boolean).join(" · ") : "";
    $("update-notes").textContent = info?.notes || "";
    $("update-notes-wrap").classList.toggle("hidden", !info?.notes);
    positionMenu();
  }

  function schedule() {
    clearTimeout(timer);
    if (signal.aborted || document.hidden || !navigator.onLine) return;
    timer = setTimeout(() => void check(false), Math.max(0, lastAttempt + interval - Date.now()));
  }

  async function check(force: boolean) {
    if (checking || signal.aborted) return;
    checking = true; error = ""; lastAttempt = Date.now(); render();
    try {
      const result = await api<UpdateInfo>("/updates/check" + (force ? "?force=1" : ""), { method: "POST", signal });
      if (signal.aborted) return;
      info = result; error = result.error;
      lastAttempt = result.attempted_at || lastAttempt;
    } catch (e) {
      if (!signal.aborted) error = "检查更新失败：" + (e as Error).message;
    } finally {
      checking = false;
      if (!signal.aborted) { render(); schedule(); }
    }
  }

  function positionMenu() {
    if (!menu.matches(":popover-open")) return;
    const rect = badge.getBoundingClientRect();
    menu.style.left = Math.max(12, Math.min(rect.right - menu.offsetWidth, window.innerWidth - menu.offsetWidth - 12)) + "px";
    menu.style.top = Math.max(12, Math.min(rect.bottom + 10, window.innerHeight - menu.offsetHeight - 12)) + "px";
  }
  badge.addEventListener("click", () => {
    hideTip();
    menu.togglePopover(); positionMenu();
    if (menu.matches(":popover-open")) menu.querySelector<HTMLButtonElement>("button")?.focus();
  }, { signal });
  menu.addEventListener("keydown", e => {
    if (e.key === "Escape") {
      e.preventDefault(); e.stopPropagation();
      menu.hidePopover(); badge.focus();
    }
  }, { signal });
  menu.addEventListener("toggle", () => { badge.ariaExpanded = String(menu.matches(":popover-open")); }, { signal });
  window.addEventListener("resize", positionMenu, { signal });
  for (const el of document.querySelectorAll("[data-update-check]")) el.addEventListener("click", () => void check(true), { signal });
  for (const el of document.querySelectorAll("[data-update-about]")) el.addEventListener("click", () => {
    menu.hidePopover(); S.sec = "about"; emit("open-settings");
  }, { signal });
  document.addEventListener("visibilitychange", schedule, { signal });
  window.addEventListener("online", schedule, { signal });
  window.addEventListener("offline", schedule, { signal });
  render();
  // Render local build metadata before making the (potentially slow) upstream request.
  void (async () => {
    try {
      const result = await api<UpdateInfo>("/updates", { signal });
      if (signal.aborted || checking || lastAttempt) return;
      info = result; error = result.error; lastAttempt = result.attempted_at;
      render();
    } catch { /* The scheduled check surfaces errors and allows a retry. */ }
    if (!signal.aborted) schedule();
  })();
  return () => {
    lifetime.abort(); clearTimeout(timer); menu.hidePopover();
    badge.classList.add("hidden"); badge.ariaExpanded = "false";
  };
}
