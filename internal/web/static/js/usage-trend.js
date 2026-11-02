/* usage-trend：使用记录页的时序柱状图，形态和分桶思路参考 cc-switch 的 UsageTrendChart。
 *
 * 前端没有图表依赖，柱子是普通 div：宽度跟着 flex 走，改窗口大小不用重算，配色取主题
 * 令牌。只有横轴刻度要按宽度决定疏密，由 ResizeObserver 重排。桶由服务端按系统时区切好，
 * start 是墙上时间字符串（见 usagetrend.go），这里只做字符串切片，不碰浏览器时区。
 *
 * 取数归 usage.ts（它持有筛选条件和翻页），这里只管画和交互；换指标不用重新请求。 */
"use strict";
import { t as i18nText, setText, setTextRender } from "./i18n.js";
import { fmtUSD } from "./quota.js";
import { $ } from "./util.js";
const compact = new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 });
const plain = (n) => n.toLocaleString("en-US");
/* 金额全程是微美元整数，只有落到文字这一步才换成美元（同 quota.ts）。 */
const METRICS = {
    // Token 与表格的「合计」、汇总条的「总 Token」同一口径：四个桶相加。
    tokens: {
        label: () => "Token",
        value: (b) => b.input_tokens + b.output_tokens + b.cache_read_tokens + b.cache_write_tokens,
        tick: (v) => compact.format(v),
        exact: plain,
    },
    requests: {
        label: () => i18nText("请求"),
        value: (b) => b.requests,
        tick: (v) => compact.format(v),
        exact: plain,
    },
    cost: {
        label: () => i18nText("费用"),
        value: (b) => b.cost_micro_usd,
        tick: (v) => "$" + (v / 1e6).toLocaleString("en-US", { maximumFractionDigits: 6 }),
        exact: (v) => fmtUSD(v),
    },
};
const METRIC_KEY = "agentbox_usage_trend_metric";
const OPEN_KEY = "agentbox_usage_trend";
const box = $("usage-trend");
const chart = $("ut-chart");
const plot = $("ut-plot");
const slotsEl = $("ut-slots");
const yEl = $("ut-y");
const xEl = $("ut-x");
const note = $("ut-note");
const tip = $("ut-tip");
const tipHead = $("ut-tip-head");
const tipName = $("ut-tip-name");
const tipValue = $("ut-tip-value");
const metricBox = $("ut-metric");
let metric = "tokens";
let data = null;
let active = -1;
let rangeLabel = () => "";
/* ---------------- 纵轴 ---------------- */
/* 纵轴固定三个刻度（0、一半、顶），所以「一半」也要是好读的数；指标都是整数，一半也得是
 * 整数，否则单次请求的图上会出现 0.5 次。 */
export function axisTop(max) {
    if (max <= 0)
        return 0;
    const half = max / 2;
    const scale = 10 ** Math.floor(Math.log10(half));
    for (const m of [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) {
        const step = m * scale;
        if (step >= half && Number.isInteger(step))
            return step * 2;
    }
    return Math.ceil(half) * 2;
}
function renderYAxis(top) {
    const ticks = top ? [top, top / 2, 0] : [0];
    yEl.replaceChildren(...ticks.map((v) => {
        const label = document.createElement("span");
        label.textContent = METRICS[metric].tick(v);
        return label;
    }));
    // 只有一个刻度时它属于基线：space-between 会把唯一的子元素放到顶上。
    yEl.classList.toggle("only-zero", !top);
}
/* ---------------- 横轴 ---------------- */
const clock = (start) => start.slice(11, 16);
const monthDay = (start) => `${start.slice(5, 7)}/${start.slice(8, 10)}`;
const yearMonth = (start) => `${start.slice(0, 4)}/${start.slice(5, 7)}`;
function tooltipLabel(start, granularity) {
    if (granularity === "month")
        return yearMonth(start);
    const day = `${start.slice(0, 4)}/${monthDay(start)}`;
    return granularity === "hour" ? `${day} ${clock(start)}` : day;
}
/* 同一天的小时图刻度只写钟点，日期在标题里已经有；跨天时 0 点那格写日期，标出换日。 */
function tickLabel(start, trend) {
    const buckets = trend.buckets;
    const first = buckets[0].start, last = buckets[buckets.length - 1].start;
    if (trend.granularity === "month")
        return yearMonth(start);
    if (trend.granularity === "day") {
        return first.slice(0, 4) === last.slice(0, 4) ? monthDay(start) : `${start.slice(2, 4)}/${monthDay(start)}`;
    }
    return first.slice(0, 10) !== last.slice(0, 10) && clock(start) === "00:00" ? monthDay(start) : clock(start);
}
/* 小时图只挑能整除的钟点当刻度，疏密再变 0 点也总在；其余从最新一格往回数，最新的总有标签。 */
function tickIndexes(trend, width) {
    const n = trend.buckets.length;
    const sample = trend.buckets[0] ? tickLabel(trend.buckets[0].start, trend) : "";
    const room = Math.max(2, Math.floor(width / (sample.length * 7 + 18)));
    const all = Array.from({ length: n }, (_, i) => i);
    if (trend.granularity === "hour") {
        const step = [1, 2, 3, 4, 6, 12, 24].find((s) => n / s <= room) ?? 24;
        return all.filter((i) => Number(trend.buckets[i].start.slice(11, 13)) % step === 0);
    }
    const step = Math.ceil(n / room);
    return all.filter((i) => (n - 1 - i) % step === 0);
}
function renderXAxis() {
    xEl.replaceChildren();
    const width = plot.clientWidth;
    if (!data || !data.buckets.length || !width)
        return;
    const n = data.buckets.length;
    for (const i of tickIndexes(data, width)) {
        const label = document.createElement("span");
        label.textContent = tickLabel(data.buckets[i].start, data);
        xEl.appendChild(label);
        // 两头的标签以柱子中心对齐会伸出图外，推回来贴边。
        const center = (i + 0.5) * width / n;
        label.style.left = Math.max(0, Math.min(width - label.offsetWidth, center - label.offsetWidth / 2)) + "px";
    }
}
/* ---------------- 柱子与悬停 ---------------- */
function render() {
    const def = METRICS[metric];
    const buckets = data?.buckets || [];
    const top = axisTop(Math.max(0, ...buckets.map(def.value)));
    renderYAxis(top);
    slotsEl.replaceChildren(...buckets.map((b, i) => {
        const slot = document.createElement("div");
        slot.className = "ut-slot";
        slot.dataset.i = String(i);
        const value = def.value(b);
        if (value > 0) {
            const bar = document.createElement("div");
            bar.className = "ut-bar";
            bar.style.height = (value / top) * 100 + "%";
            slot.appendChild(bar);
        }
        return slot;
    }));
    renderXAxis();
    if (active >= buckets.length)
        active = -1;
    setActive(active);
}
function setActive(i) {
    slotsEl.children[active]?.classList.remove("on");
    active = i;
    const slot = slotsEl.children[i];
    const bucket = data?.buckets[i];
    if (!slot || !bucket || !data) {
        tip.hidden = true;
        return;
    }
    slot.classList.add("on");
    tipHead.textContent = tooltipLabel(bucket.start, data.granularity);
    tipValue.textContent = METRICS[metric].exact(METRICS[metric].value(bucket));
    tip.hidden = false;
    // 默认摆在这一格右边，放不下就翻到左边，不挡住正在看的柱子。
    const gap = 8, width = plot.clientWidth, tipWidth = tip.offsetWidth;
    const right = slot.offsetLeft + slot.offsetWidth + gap;
    const left = right + tipWidth <= width ? right : slot.offsetLeft - gap - tipWidth;
    tip.style.left = Math.max(0, Math.min(width - tipWidth, left)) + "px";
}
/* ---------------- 对外接口 ---------------- */
export function usageTrendOpen() { return !box.classList.contains("collapsed"); }
// 标题写着当前时间范围，范围随筛选变，每次取数都要重新绑定。
const syncTitle = () => setTextRender($("ut-title"), () => i18nText("使用趋势") + " · " + rangeLabel());
export function renderUsageTrend(next) {
    // 响应不一定是这个形状（比如前面还有一层代理）：坏数据不能进状态，否则之后每次重排都崩。
    if (!Array.isArray(next?.buckets)) {
        showUsageTrendError();
        return;
    }
    data = next;
    chart.classList.remove("busy");
    syncTitle();
    const empty = !next.buckets.length;
    note.hidden = !empty;
    if (empty)
        setText(note, "这个时间范围内没有记录");
    render();
}
/** 换筛选时旧的图先留着调暗，等新数据到了再换，不闪、不跳布局。 */
export function markUsageTrendBusy() {
    chart.classList.add("busy");
    syncTitle();
}
/** 读取失败：手里有图就接着显示（轮询失败不打断），没有才说明。 */
export function showUsageTrendError() {
    chart.classList.remove("busy");
    if (data?.buckets.length)
        return;
    note.hidden = false;
    setText(note, "读取使用趋势失败");
}
export function clearUsageTrend() {
    data = null;
    active = -1;
    chart.classList.remove("busy");
    note.hidden = true;
    render();
}
/* ---------------- 指标切换与折叠 ---------------- */
function syncMetric() {
    for (const button of metricBox.querySelectorAll("[data-metric]")) {
        const on = button.dataset.metric === metric;
        button.classList.toggle("active", on);
        button.setAttribute("aria-pressed", String(on));
    }
    for (const el of [$("ut-legend-name"), tipName])
        setTextRender(el, () => METRICS[metric].label());
}
function setOpen(open, remember = true) {
    box.classList.toggle("collapsed", !open);
    $("ut-toggle").setAttribute("aria-expanded", String(open));
    // 隐私模式下 localStorage 可能不可写：本次会话仍生效，只是记不住
    if (remember)
        try {
            localStorage.setItem(OPEN_KEY, open ? "open" : "closed");
        }
        catch (_) { }
}
/* 窄屏和矮窗口先收起，把高度留给明细（和筛选条同一个规矩）；用户动过一次就记住。 */
export function initUsageTrend(opts) {
    let savedMetric = null, savedOpen = null;
    try {
        savedMetric = localStorage.getItem(METRIC_KEY);
        savedOpen = localStorage.getItem(OPEN_KEY);
    }
    catch (_) { }
    if (savedMetric && Object.hasOwn(METRICS, savedMetric))
        metric = savedMetric;
    rangeLabel = opts.rangeLabel;
    syncMetric();
    syncTitle();
    setOpen(savedOpen ? savedOpen === "open" : !window.matchMedia("(max-width: 760px), (max-height: 760px)").matches, false);
    metricBox.addEventListener("click", (e) => {
        const next = e.target.closest("[data-metric]")?.dataset.metric;
        if (!next || next === metric)
            return;
        metric = next;
        try {
            localStorage.setItem(METRIC_KEY, metric);
        }
        catch (_) { }
        syncMetric();
        render();
    });
    $("ut-toggle").addEventListener("click", () => {
        setOpen(!usageTrendOpen());
        if (usageTrendOpen())
            opts.load();
    });
    // 整列都是热区：瞄准柱子很难，尤其是几乎为零的那些，甚至这一格根本没有柱子。
    const slotAt = (e) => e.target.closest(".ut-slot");
    const hover = (e) => {
        const slot = slotAt(e);
        if (slot)
            setActive(Number(slot.dataset.i));
    };
    plot.addEventListener("pointermove", hover);
    plot.addEventListener("pointerdown", hover);
    // 触屏抬手也会触发 leave：那时提示要留着，点图外面才收。
    plot.addEventListener("pointerleave", (e) => { if (e.pointerType === "mouse")
        setActive(-1); });
    document.addEventListener("pointerdown", (e) => { if (active >= 0 && !plot.contains(e.target))
        setActive(-1); });
    // 键盘和悬停看到同样的内容：Tab 进图后方向键逐格移动。鼠标点进来不算，提示跟指针走。
    plot.addEventListener("focus", () => {
        if (!plot.matches(":focus-visible") || !data?.buckets.length)
            return;
        setActive(active >= 0 ? active : data.buckets.length - 1);
    });
    plot.addEventListener("blur", () => setActive(-1));
    plot.addEventListener("keydown", (e) => {
        const n = data?.buckets.length || 0;
        if (!n)
            return;
        const moves = {
            ArrowLeft: Math.max(0, active - 1), ArrowRight: Math.min(n - 1, active + 1), Home: 0, End: n - 1, Escape: -1,
        };
        const move = moves[e.key];
        if (move === undefined)
            return;
        e.preventDefault();
        setActive(move);
    });
    // 视图隐藏时宽度是 0、画不了刻度；重新显示和改窗口大小都走这里。
    let lastWidth = 0;
    new ResizeObserver(() => {
        if (plot.clientWidth === lastWidth)
            return;
        lastWidth = plot.clientWidth;
        renderXAxis();
        setActive(active);
    }).observe(plot);
}
