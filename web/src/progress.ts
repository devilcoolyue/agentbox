/* progress：进度条组件。
 *
 * 知道总量时（下载了多少字节、Git 报的百分比）画实心填充；还不知道时画一小段来回滑，
 * 告诉人「在进行，只是说不出到哪」。进行中的实心填充再走一层斜纹，阶段长时间停在同一个
 * 百分比时也看得出没卡死。样式在 base.css 的 .progress。 */
"use strict";

import { setAttrRender } from "./i18n.js";

export type ProgressTone = "" | "ok" | "error";

export class ProgressBar {
  readonly el: HTMLElement;
  private value: number | null = null;

  /** label 是给读屏的名字（如「升级进度」），可随语言切换 */
  constructor(label?: () => string) {
    this.el = document.createElement("div");
    this.el.className = "progress indeterminate";
    this.el.setAttribute("role", "progressbar");
    this.el.setAttribute("aria-valuemin", "0");
    this.el.setAttribute("aria-valuemax", "100");
    if (label) setAttrRender(this.el, "aria-label", label);
    const fill = document.createElement("span");
    fill.className = "progress-fill";
    this.el.append(fill);
  }

  /** 0～1 是确定进度；null 是不确定。active 控制斜纹（进行中才有）。 */
  set(value: number | null, active = true) {
    this.value = value === null || !Number.isFinite(value) ? null : Math.max(0, Math.min(1, value));
    this.el.classList.toggle("indeterminate", this.value === null);
    this.el.classList.toggle("active", active && this.value !== null);
    if (this.value === null) {
      this.el.style.removeProperty("--progress");
      this.el.removeAttribute("aria-valuenow");
    } else {
      this.el.style.setProperty("--progress", String(this.value));
      this.el.setAttribute("aria-valuenow", String(Math.round(this.value * 100)));
    }
    return this;
  }

  get(): number | null { return this.value; }

  tone(kind: ProgressTone) {
    this.el.classList.toggle("ok", kind === "ok");
    this.el.classList.toggle("error", kind === "error");
    return this;
  }
}

/** 按「每秒多少字节」算速率：两次采样的差，第一次或时间没动时返回 0。 */
export class RateMeter {
  private last?: { at: number; bytes: number };
  private rate = 0;
  sample(bytes: number, at = Date.now()) {
    if (this.last && at > this.last.at && bytes >= this.last.bytes) {
      const now = (bytes - this.last.bytes) / ((at - this.last.at) / 1000);
      // 轻微平滑：轮询间隔抖动时速率不至于一跳一跳
      this.rate = this.rate ? this.rate * 0.4 + now * 0.6 : now;
    } else if (this.last && bytes < this.last.bytes) this.rate = 0;
    this.last = { at, bytes };
    return this.rate;
  }
  reset() { this.last = undefined; this.rate = 0; }
}
