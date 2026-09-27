/* 日期范围组件：草稿与已应用值分离。所有值都是指定时区的墙上时间，
 * 不交给浏览器本地时区解析；只有快捷范围的相对时长按绝对时刻计算。 */
import { buttonLabel, decorateIcons } from "./icons.js";

export interface DateRangeValue {
  since: string;
  until: string;
  followNow: boolean;
  preset: string;
}

const pad = (n: number) => String(n).padStart(2, "0");
const emptyRange = (): DateRangeValue => ({ since: "", until: "", followNow: false, preset: "" });
const presets = [
  ["today", "当天"], ["yesterday", "昨日"], ["1", "24 小时"],
  ["7", "7d"], ["14", "14d"], ["30", "30d"], ["all", "全部时间"],
] as const;

function wallStamp(date: Date, timeZone: string): string {
  const parts = new Intl.DateTimeFormat("en-CA", {
    timeZone, year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", hourCycle: "h23",
  }).formatToParts(date);
  const p = Object.fromEntries(parts.map(x => [x.type, x.value]));
  return `${p.year}-${p.month}-${p.day}T${p.hour}:${p.minute}`;
}

function dayString(d: Date): string {
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}`;
}

function shiftDay(day: string, count: number): string {
  const d = new Date(day + "T12:00:00Z");
  d.setUTCDate(d.getUTCDate() + count);
  return dayString(d);
}

function parseDay(value: string): string | null {
  if (!value.trim()) return "";
  const match = /^(\d{4})[-/]?(\d{2})[-/]?(\d{2})$/.exec(value.trim());
  if (!match) return null;
  const day = `${match[1]}-${match[2]}-${match[3]}`;
  const d = new Date(day + "T12:00:00Z");
  return Number(match[1]) >= 1000 && !isNaN(d.getTime()) && dayString(d) === day ? day : null;
}

export class DateRangePicker {
  private applied = emptyRange();
  private draft = emptyRange();
  private active: "since" | "until" = "since";
  private month = "";
  private focusDay = "";
  private panel: HTMLDivElement;
  private field = <T extends HTMLElement>(name: string) => this.panel.querySelector<T>(`[data-dr="${name}"]`)!;

  constructor(private trigger: HTMLButtonElement, private zone: () => string, private onChange: () => void,
    private defaultPreset: typeof presets[number][0] | "" = "") {
    this.applied = defaultPreset ? this.rangeFor(defaultPreset) : emptyRange();
    this.panel = document.createElement("div");
    this.panel.className = "date-range-popover";
    this.panel.id = trigger.id + "-popover";
    this.panel.setAttribute("popover", "auto");
    this.panel.setAttribute("role", "dialog");
    this.panel.setAttribute("aria-label", "选择时间范围");
    this.panel.innerHTML = `
      <form novalidate>
        <div class="dr-presets" aria-label="快捷时间范围"></div>
        <div class="dr-body">
          <div class="dr-editor">
            <div class="dr-caption">日期与时间 <span data-dr="zone"></span></div>
            ${(["since", "until"] as const).map(which => `
              <section class="dr-bound" data-dr="${which}-box">
                <button class="dr-bound-title" type="button" data-icon="clock" data-dr="${which}-select">${which === "since" ? "开始时间" : "结束时间"}</button>
                <div class="dr-inputs">
                  <input data-dr="${which}-day" type="text" inputmode="numeric" placeholder="YYYY/MM/DD"
                    autocomplete="off" aria-label="${which === "since" ? "开始" : "结束"}日期" maxlength="10">
                  <div class="dr-clock">
                    <input data-dr="${which}-h" type="number" min="0" max="23" step="1" inputmode="numeric" aria-label="${which === "since" ? "开始" : "结束"}时" placeholder="${which === "since" ? "00" : "23"}">
                    <span>:</span>
                    <input data-dr="${which}-m" type="number" min="0" max="59" step="1" inputmode="numeric" aria-label="${which === "since" ? "开始" : "结束"}分" placeholder="${which === "since" ? "00" : "59"}">
                  </div>
                </div>
              </section>`).join("")}
            <label class="dr-follow"><input data-dr="follow" type="checkbox">结束时间跟随当前时刻</label>
            <p class="dr-error" data-dr="error" role="alert"></p>
            <div class="dr-actions">
              <button type="button" class="btn btn-ghost" data-dr="cancel" data-icon="close">取消</button>
              <button type="submit" class="btn btn-primary" data-icon="check">确定</button>
            </div>
          </div>
          <div class="dr-calendar">
            <div class="dr-month-nav">
              <button type="button" data-dr="prev" data-icon="chevron-left" aria-label="上个月"></button>
              <strong data-dr="month" aria-live="polite"></strong>
              <button type="button" data-dr="next" data-icon="chevron-right" aria-label="下个月"></button>
            </div>
            <div class="dr-weekdays" aria-hidden="true">${["日", "一", "二", "三", "四", "五", "六"].map(d => `<span>${d}</span>`).join("")}</div>
            <div class="dr-days" data-dr="days" role="group" aria-label="日期，方向键移动，回车选择"></div>
          </div>
        </div>
      </form>`;
    decorateIcons(this.panel);
    document.body.append(this.panel);
    trigger.setAttribute("aria-haspopup", "dialog");
    trigger.setAttribute("aria-controls", this.panel.id);
    trigger.setAttribute("aria-expanded", "false");
    // Use the native invoker relationship: otherwise light-dismiss closes on pointerdown,
    // and a click handler immediately reopens the popover when clicking its own trigger.
    trigger.setAttribute("popovertarget", this.panel.id);
    this.panel.addEventListener("beforetoggle", e => {
      if ((e as ToggleEvent).newState === "open") this.prepareOpen();
    });
    this.panel.addEventListener("toggle", () => {
      const open = this.panel.matches(":popover-open");
      trigger.setAttribute("aria-expanded", String(open));
      if (open) { this.position(); this.field("since-select").focus(); }
    });
    this.panel.addEventListener("keydown", e => {
      if (e.key === "Escape") { e.preventDefault(); this.close(); }
    });
    this.field("cancel").addEventListener("click", () => this.close());
    this.panel.querySelector("form")!.addEventListener("submit", e => {
      e.preventDefault();
      if (!this.readDraft()) return;
      if (this.draft.since && this.draft.until && this.draft.since > this.draft.until) {
        this.error("结束时间不能早于开始时间");
        return;
      }
      this.applied = { ...this.draft };
      this.syncTrigger();
      this.close();
      this.onChange();
    });
    for (const [key, label] of presets) {
      const b = document.createElement("button");
      b.type = "button";
      buttonLabel(b, label, "calendar");
      b.dataset.preset = key;
      b.addEventListener("click", () => {
        this.draft = this.rangeFor(key);
        this.active = "since";
        this.month = (this.draft.since || this.now()).slice(0, 7);
        this.syncInputs();
      });
      this.panel.querySelector(".dr-presets")!.append(b);
    }
    for (const which of ["since", "until"] as const) {
      this.field(which + "-select").addEventListener("click", () => this.selectBound(which));
      for (const suffix of ["day", "h", "m"]) {
        const input = this.field<HTMLInputElement>(which + "-" + suffix);
        input.addEventListener("focus", () => this.selectBound(which));
        input.addEventListener("input", () => { this.draft.preset = ""; this.syncPresets(); this.error(""); });
        input.addEventListener("change", () => {
          if (suffix !== "day" && input.value !== "") {
            input.value = pad(Math.max(0, Math.min(Number(input.max), Math.trunc(Number(input.value)) || 0)));
          }
          if (this.readDraft()) {
            if (this.draft[which]) this.month = this.draft[which].slice(0, 7);
            this.renderCalendar();
          }
        });
      }
    }
    this.field<HTMLInputElement>("follow").addEventListener("change", () => {
      this.draft.followNow = this.field<HTMLInputElement>("follow").checked;
      this.draft.preset = "";
      if (this.draft.followNow) {
        this.draft.until = this.now();
        this.active = "since";
      }
      // Keep an unfinished start-date edit intact when toggling the end bound.
      this.writeInput("until");
      this.syncSelection();
      this.syncPresets();
      this.renderCalendar();
    });
    this.field("prev").addEventListener("click", () => this.moveMonth(-1));
    this.field("next").addEventListener("click", () => this.moveMonth(1));
    this.field("days").addEventListener("keydown", e => this.calendarKey(e));
    window.addEventListener("resize", () => { if (this.panel.matches(":popover-open")) this.position(); });
    // Popovers live in the top layer. Follow the toolbar when its scroll container moves.
    document.addEventListener("scroll", e => {
      if (this.panel.matches(":popover-open") && !this.panel.contains(e.target as Node)) this.position();
    }, true);
    this.syncTrigger();
  }

  private now() { return wallStamp(new Date(), this.zone()); }

  private rangeFor(key: string): DateRangeValue {
    const now = new Date();
    const today = wallStamp(now, this.zone()).slice(0, 10);
    if (key === "all") return { ...emptyRange(), preset: "all" };
    if (key === "today" || key === "yesterday") {
      const day = key === "today" ? today : shiftDay(today, -1);
      return { since: day + "T00:00", until: day + "T23:59", followNow: false, preset: key };
    }
    return {
      since: wallStamp(new Date(now.getTime() - Number(key) * 86400000), this.zone()),
      until: wallStamp(now, this.zone()), followNow: true, preset: key,
    };
  }

  value(): DateRangeValue {
    // 日历快捷范围跟随系统时区与日期，跨天后再次进入也仍是「当天」。
    if (this.applied.preset === "today" || this.applied.preset === "yesterday") return this.rangeFor(this.applied.preset);
    if (!this.applied.followNow) return { ...this.applied };
    if (/^(1|7|14|30)$/.test(this.applied.preset)) return this.rangeFor(this.applied.preset);
    return { ...this.applied, until: this.now() };
  }

  reset() { this.applied = this.defaultPreset ? this.rangeFor(this.defaultPreset) : emptyRange(); this.syncTrigger(); }

  refresh() {
    this.syncTrigger();
    if (this.panel.matches(":popover-open")) {
      this.field("zone").textContent = this.zone();
      this.renderCalendar();
    }
  }

  label(): string {
    const v = this.value();
    if (!v.since && !v.until) return "全部时间";
    const label = presets.find(([key]) => key === v.preset)?.[1];
    if (label) return label === "当天" ? "当天" : /^\d+d$/.test(label) ? `最近 ${label.slice(0, -1)} 天` : label;
    const fmt = (s: string) => s.replaceAll("-", "/").replace("T", " ");
    return `${v.since ? fmt(v.since) : "不限起始"} — ${v.followNow ? "现在" : v.until ? fmt(v.until) : "不限截止"}`;
  }

  private syncTrigger() {
    this.trigger.querySelector("[data-range-label]")!.textContent = this.label();
    this.trigger.classList.toggle("has-range", !!(this.applied.since || this.applied.until));
    this.trigger.title = this.label() + `（${this.zone()}）`;
  }

  private prepareOpen() {
    this.draft = this.value();
    this.active = "since";
    this.focusDay = (this.draft.since || this.now()).slice(0, 10);
    this.month = this.focusDay.slice(0, 7);
    this.syncInputs();
  }

  private close() { this.panel.hidePopover(); this.trigger.focus(); }

  private position() {
    const rect = this.trigger.getBoundingClientRect();
    const width = this.panel.offsetWidth;
    const height = this.panel.offsetHeight;
    const left = Math.max(12, Math.min(rect.right - width, window.innerWidth - width - 12));
    const below = rect.bottom + 8;
    const top = below + height <= window.innerHeight - 12 ? below
      : Math.max(12, rect.top - height - 8);
    this.panel.style.left = `${left}px`;
    this.panel.style.top = `${top}px`;
  }

  private writeInput(which: "since" | "until") {
    const value = this.draft[which];
    this.field<HTMLInputElement>(which + "-day").value = value.slice(0, 10).replaceAll("-", "/");
    this.field<HTMLInputElement>(which + "-h").value = value.slice(11, 13);
    this.field<HTMLInputElement>(which + "-m").value = value.slice(14, 16);
  }

  private syncInputs() {
    this.writeInput("since");
    this.writeInput("until");
    this.field<HTMLInputElement>("follow").checked = this.draft.followNow;
    this.field("zone").textContent = this.zone();
    this.error("");
    this.syncSelection();
    this.syncPresets();
    this.renderCalendar();
  }

  private readDraft(): boolean {
    const next = { ...this.draft };
    for (const which of ["since", "until"] as const) {
      if (which === "until" && next.followNow) { next.until = this.now(); continue; }
      const day = parseDay(this.field<HTMLInputElement>(which + "-day").value);
      if (day === null) { this.error("请输入有效日期，例如 2026/09/22"); return false; }
      const time = ["h", "m"].map((part, i) => {
        const input = this.field<HTMLInputElement>(which + "-" + part);
        const fallback = which === "since" ? 0 : i === 0 ? 23 : 59;
        return pad(Math.max(0, Math.min(i === 0 ? 23 : 59, Math.trunc(Number(input.value || fallback)) || 0)));
      });
      next[which] = day ? `${day}T${time.join(":")}` : "";
    }
    this.draft = next;
    this.error("");
    return true;
  }

  private error(message: string) { this.field("error").textContent = message; }

  private syncPresets() {
    for (const button of this.panel.querySelectorAll<HTMLButtonElement>("[data-preset]")) {
      button.setAttribute("aria-pressed", String(button.dataset.preset === this.draft.preset));
    }
  }

  private syncSelection() {
    for (const which of ["since", "until"] as const) {
      const box = this.field(which + "-box");
      const disabled = which === "until" && this.draft.followNow;
      box.classList.toggle("active", which === this.active);
      box.classList.toggle("disabled", disabled);
      this.field(which + "-select").setAttribute("aria-pressed", String(which === this.active));
      for (const input of box.querySelectorAll<HTMLInputElement | HTMLButtonElement>("input, button")) input.disabled = disabled;
    }
  }

  private selectBound(which: "since" | "until") {
    this.active = which;
    const day = parseDay(this.field<HTMLInputElement>(which + "-day").value);
    if (day) { this.month = day.slice(0, 7); this.focusDay = day; }
    this.syncSelection();
    this.renderCalendar();
  }

  private moveMonth(delta: number) {
    const d = new Date(this.month + "-01T12:00:00Z");
    d.setUTCMonth(d.getUTCMonth() + delta);
    if (d.getUTCFullYear() < 1000 || d.getUTCFullYear() > 9999) return;
    this.month = dayString(d).slice(0, 7);
    this.focusDay = this.month + "-01";
    this.renderCalendar();
  }

  private chooseDay(day: string) {
    const which = this.active;
    this.field<HTMLInputElement>(which + "-day").value = day.replaceAll("-", "/");
    this.draft.preset = "";
    if (!this.readDraft()) return;
    this.month = day.slice(0, 7);
    this.focusDay = day;
    if (which === "since" && !this.draft.followNow) this.active = "until";
    this.syncSelection();
    this.syncPresets();
    this.renderCalendar();
    this.field("days").querySelector<HTMLButtonElement>(`[data-day="${day}"]`)?.focus();
  }

  private renderCalendar() {
    const [year, month] = this.month.split("-").map(Number);
    this.field("month").textContent = `${year}年${month}月`;
    const first = this.month + "-01";
    const start = shiftDay(first, -new Date(first + "T12:00:00Z").getUTCDay());
    const today = this.now().slice(0, 10);
    const since = this.draft.since.slice(0, 10);
    const until = this.draft.until.slice(0, 10);
    if (this.focusDay.slice(0, 7) !== this.month) this.focusDay = first;
    const days = this.field("days");
    // Reuse day buttons. Replacing them on an input's change (blur) would remove
    // the pointer target between mousedown and click when picking a date.
    if (!days.childElementCount) {
      for (let i = 0; i < 42; i++) {
        const button = document.createElement("button");
        button.type = "button";
        button.addEventListener("click", () => this.chooseDay(button.dataset.day!));
        days.append(button);
      }
    }
    for (let i = 0; i < 42; i++) {
      const day = shiftDay(start, i);
      const b = days.children[i] as HTMLButtonElement;
      b.dataset.day = day;
      b.textContent = String(Number(day.slice(8)));
      b.setAttribute("aria-label", day.replaceAll("-", "/"));
      b.setAttribute("aria-pressed", String(day === since || day === until));
      if (day === today) b.setAttribute("aria-current", "date");
      else b.removeAttribute("aria-current");
      b.classList.toggle("outside", day.slice(0, 7) !== this.month);
      b.classList.toggle("in-range", !!since && !!until && day > since && day < until);
      b.classList.toggle("endpoint", day === since || day === until);
      b.tabIndex = day === this.focusDay ? 0 : -1;
    }
  }

  private calendarKey(e: KeyboardEvent) {
    const b = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-day]");
    if (!b) return;
    const day = b.dataset.day!;
    const weekday = new Date(day + "T12:00:00Z").getUTCDay();
    const shifts: Record<string, number> = { ArrowLeft: -1, ArrowRight: 1, ArrowUp: -7, ArrowDown: 7, Home: -weekday, End: 6 - weekday };
    if (e.key in shifts) {
      e.preventDefault();
      this.focusDay = shiftDay(day, shifts[e.key]);
      this.month = this.focusDay.slice(0, 7);
      this.renderCalendar();
    } else if (e.key === "PageUp" || e.key === "PageDown") {
      e.preventDefault();
      this.moveMonth(e.key === "PageUp" ? -1 : 1);
    } else return;
    this.field("days").querySelector<HTMLButtonElement>(`[data-day="${this.focusDay}"]`)?.focus();
  }
}
