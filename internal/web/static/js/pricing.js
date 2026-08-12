/* pricing：系统设置「价目表」分区 —— 按 token 折算费用的单价表。
 *
 * 这张表只在 **provider 不自报价格** 时用得上：Codex 的全部回合，以及从 CLI 记录里
 * 补记的终端消耗（transcript 只给 token 不给美元）。网页对话里 Claude 自报
 * total_cost_usd 的行不查这张表——所以给 claude 配了价，也不会改变已有对话行的金额。
 *
 * 单位统一是「美元 / 百万 token」，与两家官方价目表的写法一致，管理员照抄即可，
 * 不必自己换算。服务端拿它乘 token 数得到微美元整数（见 quota.go 的 priceEvent）。
 *
 * 编辑是「整表提交」而不是逐行保存：删行没法用增量表达，而且价目表是要整体核对的
 * 东西，一次看完一次存更不容易漏。 */
"use strict";
import { S } from "./state.js";
import { $, toast } from "./util.js";
// settings.ts 也会 import 本模块（进分区时调 openPricingSection），构成一个环。
// 这里只在点击回调里用 putSettings，而它是函数声明（提升），环里取到的绑定
// 一定已经就位；两个模块的顶层代码都不碰对方，所以这个环是安全的。
import { putSettings } from "./settings.js";
/* Claude 官方价目快照，抄自 platform.claude.com/docs/en/about-claude/pricing
 * （2026-08-11）。四个数依次是 输入 / 输出 / 缓存读取 / 缓存写入，美元每百万 token。
 *
 * 缓存写入取 **1 小时** 档（= 2× 输入价）而不是 5 分钟档（1.25×）：Claude Code 用的
 * 就是 1h 缓存，实测 transcript 里 cache_creation.ephemeral_1h_input_tokens 有值、
 * 5m 那项是 0。我们的用量只有一个「缓存写入」桶，两档合不了，取实际用的那档。
 *
 * Claude 4.6 及之后的模型 1M 上下文按标准价计费，所以都不带长上下文档位。 */
const CLAUDE_OFFICIAL = {
    "claude-fable-5": [10, 50, 1, 20],
    "claude-mythos-5": [10, 50, 1, 20],
    "claude-opus-5": [5, 25, 0.5, 10],
    "claude-opus-4-8": [5, 25, 0.5, 10],
    "claude-opus-4-7": [5, 25, 0.5, 10],
    "claude-opus-4-6": [5, 25, 0.5, 10],
    "claude-opus-4-5": [5, 25, 0.5, 10],
    "claude-opus-4-1": [15, 75, 1.5, 30],
    "claude-sonnet-5": [2, 10, 0.2, 4],
    "claude-sonnet-4-6": [3, 15, 0.3, 6],
    "claude-sonnet-4-5": [3, 15, 0.3, 6],
    "claude-haiku-4-5": [1, 5, 0.1, 2],
    "claude-haiku-3-5": [0.8, 4, 0.08, 1.6],
};
/* OpenAI 官方价目快照（2026-08-11）。短上下文四个数 + 长上下文四个数，
 * 顺序同上。官方表里的「-」一律记 0——我们的约定就是「0 = 这一桶免费/不适用」。
 * null 表示该模型没有长上下文档位。
 *
 * 阈值 OAI_LONG_OVER 不在官方那张表上（它只写「Short / Long context」两列），
 * 沿用项目里一直在用的 272000 input token。换模型时记得核对这个数。 */
const OAI_LONG_OVER = 272000;
const OPENAI_OFFICIAL = {
    "gpt-5.6-sol": [[5, 30, 0.5, 6.25], [10, 45, 1, 12.5]],
    "gpt-5.6-terra": [[2, 12, 0.2, 2.5], [4, 18, 0.4, 5]],
    "gpt-5.6-luna": [[0.2, 1.2, 0.02, 0.25], [0.4, 1.8, 0.04, 0.5]],
    "gpt-5.5": [[5, 30, 0.5, 0], [10, 45, 1, 0]],
    "gpt-5.5-pro": [[30, 180, 0, 0], [60, 270, 0, 0]],
    "gpt-5.4": [[2.5, 15, 0.25, 0], [5, 22.5, 0.5, 0]],
    "gpt-5.4-mini": [[0.75, 4.5, 0.075, 0], null],
    "gpt-5.4-nano": [[0.2, 1.25, 0.02, 0], null],
    "gpt-5.4-pro": [[30, 180, 0, 0], [60, 270, 0, 0]],
};
/* agent 名当键时是「这个 agent 的兜底价」，比具体模型行管得宽，标出来免得看混。 */
const FALLBACK_KEYS = new Set(["claude", "codex"]);
/** 编辑中的表。进分区时从 S.settings 灌一次，保存前从 DOM 读回来。 */
let draft = {};
const num = (v) => (v ? String(v) : "");
function rateInput(cls, value, title) {
    const el = document.createElement("input");
    el.type = "text"; // 用 text 而不是 number：number 在中文输入法下会吃掉小数点
    el.className = cls;
    el.value = num(value);
    el.placeholder = "0";
    el.title = title;
    el.inputMode = "decimal";
    return el;
}
function renderRows() {
    const body = $("price-rows");
    body.replaceChildren();
    const keys = Object.keys(draft).sort();
    for (const key of keys) {
        const p = draft[key];
        const tr = document.createElement("tr");
        tr.dataset.key = key;
        const k = document.createElement("td");
        k.className = "pt-key" + (FALLBACK_KEYS.has(key) ? " fallback" : "");
        k.textContent = key;
        k.title = FALLBACK_KEYS.has(key)
            ? `${key} 的兜底价：这个 agent 下没有单独配价的模型都按它算`
            : key;
        tr.appendChild(k);
        for (const [field, label] of [
            ["input", "输入"], ["output", "输出"],
            ["cache_read", "缓存读取"], ["cache_write", "缓存写入"],
        ]) {
            const td = document.createElement("td");
            td.className = "num";
            td.appendChild(rateInput("rate " + field, p[field], `${label}：美元 / 百万 token`));
            tr.appendChild(td);
        }
        const over = document.createElement("td");
        over.className = "num";
        over.appendChild(rateInput("rate wide over", p.long_context_over, "超过这么多 token 的回合整体按右边那档计价（留空 = 没有长上下文档位）"));
        tr.appendChild(over);
        const long = document.createElement("td");
        long.className = "num";
        const wrap = document.createElement("div");
        wrap.className = "pt-long";
        const l = p.long || {};
        for (const [field, label] of [
            ["input", "输入"], ["output", "输出"],
            ["cache_read", "缓存读取"], ["cache_write", "缓存写入"],
        ]) {
            wrap.appendChild(rateInput("rate long-" + field, l[field], `长上下文档的${label}`));
        }
        long.appendChild(wrap);
        tr.appendChild(long);
        const del = document.createElement("td");
        const btn = document.createElement("button");
        btn.className = "pt-del";
        btn.type = "button";
        btn.textContent = "✕";
        btn.title = "删掉这一行";
        btn.addEventListener("click", () => {
            readDraft(); // 先把别的行的改动收进来，别让删除顺手回滚它们
            delete draft[key];
            renderRows();
        });
        del.appendChild(btn);
        tr.appendChild(del);
        body.appendChild(tr);
    }
    $("price-empty").classList.toggle("hidden", keys.length > 0);
    $("price-count").textContent = String(keys.length);
}
/** 空串按 0 算；填了非数字则返回 NaN，由 readDraft 的调用方拦下。 */
function parseRate(el) {
    const t = el.value.trim();
    if (!t)
        return 0;
    return Number(t);
}
/** 从 DOM 读回整张表，覆盖 draft。 */
function readDraft() {
    const next = {};
    for (const tr of document.querySelectorAll("#price-rows tr")) {
        const key = tr.dataset.key;
        const get = (cls) => parseRate(tr.querySelector("input." + cls));
        const p = {
            input: get("input"), output: get("output"),
            cache_read: get("cache_read"), cache_write: get("cache_write"),
        };
        const over = get("over");
        const long = {
            input: get("long-input"), output: get("long-output"),
            cache_read: get("long-cache_read"), cache_write: get("long-cache_write"),
        };
        // 长上下文档只有「阈值和单价都填了」才成立：只填阈值等于没配价，
        // 只填价则永远命中不到，两种都是安静失效，不如不写进去。
        if (over > 0 && (long.input || long.output || long.cache_read || long.cache_write)) {
            p.long_context_over = over;
            p.long = long;
        }
        next[key] = p;
    }
    draft = next;
}
/** 找出所有不是合法数字的格子，返回给保存流程提示。 */
function badCells() {
    const bad = [];
    for (const tr of document.querySelectorAll("#price-rows tr")) {
        for (const el of tr.querySelectorAll("input.rate")) {
            const t = el.value.trim();
            if (t && !(Number(t) >= 0))
                bad.push(tr.dataset.key); // NaN / 负数都进这里
        }
    }
    return [...new Set(bad)];
}
/** 进入价目表分区：用服务端的当前配置重灌编辑区，丢弃上次没存的改动。 */
export function openPricingSection() {
    draft = {};
    const src = S.settings?.pricing || {};
    for (const [k, v] of Object.entries(src))
        draft[k] = { ...v, long: v.long ? { ...v.long } : undefined };
    renderRows();
}
/** 左侧导航的条数：停在别的分区时也该是真的。 */
export function refreshPriceCount() {
    $("price-count").textContent = String(Object.keys(S.settings?.pricing || {}).length);
}
$("price-add").addEventListener("click", () => {
    const el = $("price-new-key");
    const key = el.value.trim();
    if (!key)
        return;
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(key)) {
        toast("模型 ID 只能用字母、数字和 . _ -", true);
        return;
    }
    readDraft();
    if (draft[key]) {
        toast(`${key} 已经在表里了`, true);
        return;
    }
    draft[key] = { input: 0, output: 0, cache_read: 0, cache_write: 0 };
    el.value = "";
    renderRows();
    // 新行排序后可能在任何位置，滚过去并聚焦第一个单价框
    const row = document.querySelector(`#price-rows tr[data-key="${CSS.escape(key)}"]`);
    row?.scrollIntoView({ block: "nearest" });
    row?.querySelector("input.input")?.focus();
});
const quad = (q) => ({ input: q[0], output: q[1], cache_read: q[2], cache_write: q[3] });
/* 两个「填入官方价」按钮共用：只补表里没有的键，**已有的行一律不覆盖**——
 * 管理员可能是按自己的折扣改过的，一键把它冲掉是最难查的那种 bug。 */
function fillOfficial(rows, who) {
    readDraft();
    const added = [];
    for (const [k, p] of Object.entries(rows)) {
        if (draft[k])
            continue;
        draft[k] = p;
        added.push(k);
    }
    renderRows();
    toast(added.length
        ? `填入 ${added.length} 个${who}模型的官方价，核对后点「保存价目表」`
        : `${who}官方价目里的模型都已经在表里了，没有新增`);
}
$("price-fill").addEventListener("click", () => {
    const rows = {};
    for (const [k, q] of Object.entries(CLAUDE_OFFICIAL))
        rows[k] = quad(q);
    fillOfficial(rows, "Claude ");
});
$("price-fill-oai").addEventListener("click", () => {
    const rows = {};
    for (const [k, [short, long]] of Object.entries(OPENAI_OFFICIAL)) {
        const p = quad(short);
        if (long) {
            p.long_context_over = OAI_LONG_OVER;
            p.long = quad(long);
        }
        rows[k] = p;
    }
    fillOfficial(rows, "OpenAI ");
});
$("price-save").addEventListener("click", async () => {
    const bad = badCells();
    if (bad.length) {
        toast(`这些行的单价不是合法数字：${bad.join("、")}`, true);
        return;
    }
    readDraft();
    const btn = $("price-save");
    if (await putSettings({ pricing: draft }, btn, "价目表已保存"))
        openPricingSection();
});
