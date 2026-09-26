/* 原生 textarea 负责输入、选择与保存；只读着色层和可见行号跟随它滚动。
 * 不把源码放进 contenteditable，避免浏览器改写空格或 HTML。 */
import { $ } from "./util.js";
const LANGUAGES = {
    js: "javascript", mjs: "javascript", cjs: "javascript", ts: "typescript",
    jsx: "jsx", tsx: "tsx", json: "json", html: "markup", htm: "markup", xml: "markup",
    vue: "markup", svelte: "markup", css: "css", sh: "bash", bash: "bash", zsh: "bash",
    py: "python", go: "go", rs: "rust", java: "java", c: "c", h: "c",
    cc: "cpp", cpp: "cpp", hpp: "cpp", yml: "yaml", yaml: "yaml", toml: "toml",
    sql: "sql", md: "markdown", markdown: "markdown", mdx: "markdown",
    ini: "ini", conf: "ini", env: "bash", diff: "diff", patch: "diff",
};
const LABELS = {
    javascript: "JavaScript", typescript: "TypeScript", jsx: "JSX", tsx: "TSX",
    json: "JSON", markup: "HTML / XML", css: "CSS", bash: "Shell", python: "Python",
    go: "Go", rust: "Rust", java: "Java", c: "C", cpp: "C++", yaml: "YAML",
    toml: "TOML", sql: "SQL", markdown: "Markdown", docker: "Dockerfile", ini: "INI", diff: "Diff",
};
// 正则语法分析只处理小文件；大文件仍保留行号、定位与编辑。
const HIGHLIGHT_MAX = 100_000;
export class SourceEditor {
    editor = $("fv-editor");
    highlight = $("fv-highlight");
    gutter = $("fv-gutter");
    line = $("fv-current-line");
    starts = [0];
    language = "";
    timer = 0;
    active = 0;
    constructor() {
        this.editor.addEventListener("input", () => this.update());
        this.editor.addEventListener("scroll", () => this.refresh());
        for (const event of ["click", "keyup", "select", "focus", "selectionchange"]) {
            this.editor.addEventListener(event, () => this.refresh());
        }
        new ResizeObserver(() => this.refresh()).observe(this.editor);
    }
    load(name) {
        const lower = name.toLowerCase();
        this.language = /^(dockerfile|containerfile)(\.|$)/.test(lower) ? "docker"
            : /^\.env(\.|$)/.test(lower) ? "bash" : LANGUAGES[lower.split(".").pop() || ""] || "";
        this.editor.scrollTop = this.editor.scrollLeft = 0;
        this.editor.setSelectionRange(0, 0);
        this.update(true);
    }
    update(immediate = false) {
        clearTimeout(this.timer);
        const text = this.editor.value;
        this.starts = [0];
        for (let i = 0; i < text.length; i++)
            if (text[i] === "\n")
                this.starts.push(i + 1);
        $("fv-source").style.setProperty("--fv-gutter-digits", String(Math.max(3, String(this.starts.length).length)));
        // 输入后立即同步明文，避免着色层短暂显示旧字符（也兼容中文输入法）。
        this.highlight.textContent = text + "\n";
        const canHighlight = text.length <= HIGHLIGHT_MAX && this.language && typeof Prism !== "undefined";
        $("fv-language").textContent = (LABELS[this.language] || "纯文本") +
            (text.length > HIGHLIGHT_MAX && this.language ? " · 大文件，已简化着色" : "");
        const paint = () => {
            const grammar = Prism.languages[this.language];
            if (!grammar)
                return;
            try {
                // Prism 对源码转义后输出 token span；不直接把原始文件作为 HTML。
                this.highlight.innerHTML = Prism.highlight(text, grammar, this.language) + "\n";
            }
            catch {
                this.highlight.textContent = text + "\n";
            }
            this.refresh();
        };
        if (canHighlight) {
            if (immediate)
                paint();
            else
                this.timer = window.setTimeout(paint, 100);
        }
        this.refresh();
    }
    refresh() {
        const t = this.editor;
        if (!t.clientHeight)
            return;
        const caret = t.selectionDirection === "backward" ? t.selectionStart : t.selectionEnd;
        let low = 0, high = this.starts.length;
        while (low + 1 < high) {
            const mid = (low + high) >>> 1;
            if (this.starts[mid] <= caret)
                low = mid;
            else
                high = mid;
        }
        this.active = low;
        const style = getComputedStyle(t);
        const height = parseFloat(style.lineHeight), padding = parseFloat(style.paddingTop);
        this.highlight.style.width = `${t.clientWidth}px`;
        this.highlight.style.height = `${t.clientHeight}px`;
        this.highlight.scrollTop = t.scrollTop;
        this.highlight.scrollLeft = t.scrollLeft;
        const top = padding + low * height - t.scrollTop;
        this.line.style.transform = `translateY(${top}px)`;
        this.line.style.height = `${height}px`;
        this.line.hidden = top + height < 0 || top > t.clientHeight;
        $("fv-position").textContent = `第 ${low + 1} 行，第 ${caret - this.starts[low] + 1} 列 · 共 ${this.starts.length} 行`;
        // 只画可见的行号，2MB 的短行文件也不会创建几十万个 DOM 节点。
        const first = Math.max(0, Math.floor((t.scrollTop - padding) / height));
        const end = Math.min(this.starts.length, first + Math.ceil(t.clientHeight / height) + 2);
        const rows = document.createDocumentFragment();
        for (let i = first; i < end; i++) {
            const row = document.createElement("span");
            row.textContent = String(i + 1);
            row.classList.toggle("active", i === this.active);
            row.style.top = `${padding + i * height - t.scrollTop}px`;
            rows.append(row);
        }
        this.gutter.replaceChildren(rows);
    }
}
