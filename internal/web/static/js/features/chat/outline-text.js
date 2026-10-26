/* 刻度导航的纯计算部分（不碰 DOM，scripts/test-outline.mjs 直接测）：
 * 摘要文字取原文——问题是用户消息本身（附件标记换成短标签），回答是这一轮
 * 最终回复的第一句；服务端已各截到 200 字（threads.go 的 outline）。 */
// 附件协议标记，与 chat-render.ts 的 USER_ATTACH_RE 同形；服务端截断处的半截标记也一并收掉
const ATTACH_RE = /\[(图片|附件)#\d+ [^\]\s]*\]?/g;
/** 问题：附件标记换成 label 给出的短标签，空白折叠成一个空格 */
export function outlineQuestion(text, label) {
    return text.replace(ATTACH_RE, (_, kind) => label(kind)).replace(/\s+/g, " ").trim();
}
/** 去掉一行 Markdown 的块标记与行内标记，只留读得出来的文字 */
function plainLine(line) {
    return line
        .replace(/^\s{0,3}(?:#{1,6}\s+|>\s?|[-*+]\s+(?:\[[ xX]\]\s+)?|\d+[.)]\s+)/, "")
        .replace(/!\[([^\]]*)\]\([^)]*\)/g, "$1")
        .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
        .replace(/(\*\*|__|~~)(.+?)\1/g, "$2")
        .replace(/`+([^`]+)`+/g, "$1")
        .replace(/(^|[\s(])[*_]([^*_\s][^*_]*)[*_](?=[\s).,!?:;]|$)/g, "$1$2")
        .replace(/\s+/g, " ")
        .trim();
}
/** 第一句：中文句末标点，或后面跟空白/结尾的西文句末标点（版本号、小数里的点不算） */
function firstSentence(s) {
    const m = /[。！？]|[.!?…](?=\s|$)/.exec(s);
    return m ? s.slice(0, m.index + m[0].length) : s;
}
/** 回答：跳过代码块、分隔线、表格和标题，取第一行正文的第一句，最长 max 个字；
 * 通篇只有标题时用第一个标题 */
export function outlineAnswer(text, max = 120) {
    const prose = text.replace(/^\s*(```|~~~)[\s\S]*?(?:^\s*\1\s*$|(?![\s\S]))/gm, "\n");
    let heading = "";
    for (const raw of prose.split("\n")) {
        if (/^\s*(?:[-*_]\s*){3,}$/.test(raw) || /^\s*\|/.test(raw))
            continue;
        const line = plainLine(raw);
        if (!line)
            continue;
        if (/^\s{0,3}#{1,6}\s/.test(raw)) {
            heading ||= line;
            continue;
        }
        return clip(firstSentence(line), max);
    }
    return clip(heading, max);
}
const clip = (s, max) => s.length > max ? s.slice(0, max) + "…" : s;
/** Dock 鼓起：离指针 d 像素的刻度放大多少（0–1），cos² 衰减，半径 r 外为 0 */
export function dockLift(d, r) {
    if (!(d < r))
        return 0;
    const c = Math.cos((Math.max(0, d) / r) * Math.PI / 2);
    return c * c;
}
/** 刻度排布：能放下时用 gap 间距；放不下先压到 minGap，仍放不下就开窗（只显示一段，可滚动） */
export function railLayout(count, avail, gap = 10, minGap = 6) {
    const g = Math.min(gap, Math.max(minGap, avail / Math.max(1, count)));
    const windowed = count * g > avail + 0.5;
    const height = windowed ? Math.max(g, Math.floor(avail / g) * g) : count * g;
    return { gap: g, windowed, height };
}
/** 有序数列里最后一个 ≤ y 的下标；没有返回 -1。at(i) 为 null 的项（未加载）不参与，
 * 调用方保证已加载的项是连续的一段 [from, to)。 */
export function lastAtOrBefore(from, to, at, y) {
    let lo = from, hi = to - 1, ans = -1;
    while (lo <= hi) {
        const mid = (lo + hi) >> 1;
        if (at(mid) <= y) {
            ans = mid;
            lo = mid + 1;
        }
        else
            hi = mid - 1;
    }
    return ans;
}
