/* skeleton：读取中的骨架占位。
 *
 * 读数据时先铺几条和真实内容同形状的灰条，数据到了整块换掉。要点是「占住高度」：
 * 弹窗、卡片在读取期间不先缩成一行「读取中…」再撑开，刷新时也不来回跳。
 * 外壳 .skeleton-list 晚 150ms 才淡入（base.css），读得快时根本看不到；
 * 读屏只听到外壳上的 aria-label，里面的条一律 aria-hidden。 */
"use strict";

/** 一根灰条。width 是百分比数字，或任意 CSS 长度。 */
export function skelBar(width: number | string, className = "") {
  const bar = document.createElement("span");
  bar.className = "skeleton" + (className ? " " + className : "");
  bar.style.width = typeof width === "number" ? width + "%" : width;
  bar.setAttribute("aria-hidden", "true");
  return bar;
}

/** 骨架外壳。tag 跟着宿主走：列表里放 li，表格里由调用方自己铺 tr。 */
export function skelShell(label: string, className = "", tag = "div") {
  const wrap = document.createElement(tag);
  wrap.className = "skeleton-list" + (className ? " " + className : "");
  wrap.setAttribute("role", "status");
  wrap.setAttribute("aria-label", label);
  return wrap;
}

/* 长短错开、但每次一样：同一处刷新时骨架不会每次换个样子 */
const width = (i: number, j: number, min: number, span: number) => min + ((i * 23 + j * 37) % span);

interface SkelRowsOptions {
  /** 每条几根说明条（标题条之外），默认 1 */
  lines?: number;
  /** 给每条加上真实行的类名，内边距与分隔线就和读完后一致 */
  rowClass?: string;
  /** 外壳的类名；不给 rowClass 时默认 skel-pad，自带行距与分隔线 */
  className?: string;
  /** 条目标签，默认 div；挂在 ul/ol 里时传 li */
  tag?: string;
  /** 标题条的宽度区间 [最短, 浮动范围]，默认 [28, 30] */
  title?: [number, number];
  /** 末尾再放一排按钮大小的占位（真实行带操作按钮时） */
  actions?: boolean;
}

/** 通用的「标题 + 说明」骨架行，适合卡片列表、记录列表。 */
export function skelRows(count: number, label: string, opts: SkelRowsOptions = {}) {
  const { lines = 1, rowClass = "", tag = "div", title = [28, 30] } = opts;
  const wrap = skelShell(label, opts.className ?? (rowClass ? "" : "skel-pad"));
  for (let i = 0; i < count; i++) {
    const row = document.createElement(tag);
    row.className = "skel-item" + (rowClass ? " " + rowClass : "");
    row.setAttribute("aria-hidden", "true");
    row.append(skelBar(width(i, 0, title[0], title[1])));
    for (let j = 1; j <= lines; j++) row.append(skelBar(width(i, j, 45, 45)));
    if (opts.actions) row.append(skelBar("min(16em, 70%)", "btn-like"));
    wrap.append(row);
  }
  return wrap;
}

/** 正文/代码的骨架：一行一根条，长短错开。 */
export function skelLines(count: number, label: string, className = "") {
  const wrap = skelShell(label, "skel-lines" + (className ? " " + className : ""));
  for (let i = 0; i < count; i++) wrap.append(skelBar(width(i, 1, 35, 60)));
  return wrap;
}

/** 表格骨架：铺 count 行，列数取表头里显示着的列。返回的是 tr 数组，调用方塞进 tbody。 */
export function skelTableRows(table: HTMLTableElement, count: number) {
  const cols = [...table.querySelectorAll<HTMLElement>("thead th")].filter(th => !th.classList.contains("hidden")).length || 1;
  return Array.from({ length: count }, (_, i) => {
    const tr = document.createElement("tr");
    tr.className = "skeleton-row";
    tr.setAttribute("aria-hidden", "true");
    for (let c = 0; c < cols; c++) {
      const td = document.createElement("td");
      td.append(skelBar(width(i, c, 45, 40)));
      tr.append(td);
    }
    return tr;
  });
}

/** 刷新前数一数已有几条，骨架就铺几条：高度原样保持。没有旧内容时用 fallback。 */
export function keepCount(host: Element, selector: string, fallback: number, max = 8) {
  const n = host.querySelectorAll(selector).length;
  return Math.min(max, n || fallback);
}
