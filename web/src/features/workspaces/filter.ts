import type { Session } from "../../types.js";
import { sessionState } from "../../session-state.js";
import { setSelectValue } from "../../select.js";
import { $ } from "../../util.js";

// Page-local, owned by the signed-in lifetime. Never persist project searches.
let query = "";
let status = "all";
const normalize = (value: string) => value.normalize("NFKC").trim().toLowerCase();

export function filterWorkspaces(sessions: Session[]): Session[] {
  return sessions.filter(sess => (!query || normalize(sess.name).includes(query))
    && (status === "all" || sessionState(sess).cls === status));
}

export function workspaceFilterActive(): boolean { return !!query || status !== "all"; }

export function initWorkspaceFilter(render: () => void) {
  const lifetime = new AbortController();
  const options = { signal: lifetime.signal };
  const input = $<HTMLInputElement>("session-search");
  const select = $<HTMLSelectElement>("session-filter");
  let composing = false;
  const reset = () => { query = ""; status = "all"; input.value = ""; setSelectValue(select, "all"); };
  const update = () => {
    query = normalize(input.value); status = select.value;
    $("session-list").scrollTop = 0;
    render();
  };
  reset();
  input.addEventListener("compositionstart", () => { composing = true; }, options);
  input.addEventListener("compositionend", () => { composing = false; update(); }, options);
  input.addEventListener("input", () => { if (!composing) update(); }, options);
  input.addEventListener("keydown", e => {
    if (e.key !== "Escape") return;
    if (composing || e.isComposing) { e.stopPropagation(); return; }
    if (!input.value) return;
    e.preventDefault(); e.stopPropagation(); input.value = ""; update();
  }, options);
  select.addEventListener("change", update, options);
  $("session-filter-clear").addEventListener("click", () => { reset(); update(); input.focus(); }, options);
  $("session-search-open").addEventListener("click", () => {
    $("btn-sidebar-toggle").click();
    input.focus();
  }, options);
  render();
  return () => { lifetime.abort(); reset(); };
}
