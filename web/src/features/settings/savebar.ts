/* 系统设置的统一保存条。
 *
 * 以前每张卡片各有一个「保存」，一页五六个，只有两处会提示未保存；有的卡片
 * （Agent 镜像、权限模式）自己没有按钮，要靠隔壁卡片的「保存」顺带提交。更糟的
 * 是每次保存都会用服务端返回值重填整页表单，别的卡片里没保存的改动被悄悄冲掉。
 *
 * 现在各卡片的配置项登记成「组」：与上次载入的值比较，有改动时页面底部出现
 * 「放弃 / 保存」，一次 PUT 把所有改动合并提交。切换分区前若有未保存的改动先确认。 */
import type { Settings } from "../../types.js";
import { $, askConfirm } from "../../util.js";
import { setSelectValue } from "../../select.js";

export type SettingsPatch = Partial<Settings> & { expected_agent_image?: string };

export interface SaveGroup {
  id: string;
  /** 出现在保存条与提示里，用用户认得的卡片名 */
  label: string;
  /** 参与比较的表单控件 id */
  fields: string[];
  /** 生成这一组的补丁；表单校验不通过时返回 null（控件自己会提示） */
  patch: () => SettingsPatch | null;
  /** 保存成功后的附加提示（需重启、隧道启动失败等） */
  done?: (saved: Settings) => void;
}

const groups: SaveGroup[] = [];
const baseline = new Map<string, string>();

function valueOf(id: string) {
  const el = document.getElementById(id) as HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement | null;
  if (!el) return "";
  if (el instanceof HTMLInputElement && el.type === "checkbox") return String(el.checked);
  return el.value;
}

export function defineGroups(list: SaveGroup[]) {
  groups.splice(0, groups.length, ...list);
}

/** 以当前表单值为基准。不传 ids 时整页重置（载入、保存、放弃之后）。 */
export function rebaseline(ids?: string[]) {
  const all = ids || groups.flatMap((g) => g.fields);
  for (const id of all) baseline.set(id, valueOf(id));
  renderBar();
}

/* 别处的即时保存（模型列表、默认模型）也会用服务端返回值重填整页表单。
 * 重填前先记下还没保存的改动，重填后放回去，不让它们被悄悄冲掉。 */
export function holdDirty() {
  const held = new Map<string, string>();
  for (const g of dirtyGroups()) for (const id of g.fields) held.set(id, valueOf(id));
  return () => {
    for (const [id, v] of held) {
      const el = document.getElementById(id);
      if (el instanceof HTMLInputElement && el.type === "checkbox") el.checked = v === "true";
      else if (el instanceof HTMLSelectElement) setSelectValue(el, v);
      else if (el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement) el.value = v;
    }
    renderBar();
  };
}

export function dirtyGroups() {
  return groups.filter((g) => g.fields.some((id) => baseline.has(id) && valueOf(id) !== baseline.get(id)));
}

export function renderBar() {
  const dirty = dirtyGroups();
  const bar = $("set-savebar");
  bar.hidden = !dirty.length;
  $("set-savebar-text").textContent = dirty.length ? "未保存的修改：" + dirty.map((g) => g.label).join("、") : "";
}

/** 合并所有改动过的组，一次提交。put 由设置页提供（负责请求、重填表单与提示）。 */
export async function saveDirty(put: (patch: SettingsPatch, okMsg: string) => Promise<Settings | null>) {
  const dirty = dirtyGroups();
  if (!dirty.length) return;
  const patch: SettingsPatch = {};
  for (const g of dirty) {
    const part = g.patch();
    if (!part) return; // 校验没过：控件已经弹出提示，整次不提交
    Object.assign(patch, part);
  }
  const saved = await put(patch, "已保存：" + dirty.map((g) => g.label).join("、"));
  if (saved) for (const g of dirty) g.done?.(saved);
}

/** 有未保存的改动时先确认；确认放弃则调 discard 还原表单。 */
export async function confirmDiscard(discard: () => void, action = "切换分区") {
  const dirty = dirtyGroups();
  if (!dirty.length) return true;
  const ok = await askConfirm(`「${dirty.map((g) => g.label).join("、")}」有未保存的修改，${action}后会丢失。`, {
    title: "放弃未保存的修改？", okLabel: "放弃修改", icon: "undo", danger: true,
  });
  if (ok) discard();
  return ok;
}

export function bindSaveBar(root: HTMLElement, signal: AbortSignal, onSave: () => void, onDiscard: () => void) {
  root.addEventListener("input", renderBar, { signal });
  root.addEventListener("change", renderBar, { signal });
  $("set-save").addEventListener("click", onSave, { signal });
  $("set-discard").addEventListener("click", onDiscard, { signal });
  window.addEventListener("beforeunload", (e) => {
    if (dirtyGroups().length) e.preventDefault();
  }, { signal });
}
