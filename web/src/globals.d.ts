/* globals：用 <script> 标签引入、挂在 window 上的第三方库声明。
 *
 * xterm 与 KaTeX 都是 vendor 目录里的预打包 UMD 产物（见 index.html 末尾），
 * 不走 npm 依赖，所以这里手写声明。刻意只声明代码真正调用到的那部分 API：
 * 装 @xterm/xterm 的类型包只为几个方法不划算，而全写成 any 又等于放弃检查。
 *
 * 三个类都写成「构造器 | 命名空间」两种形态兼容：不同版本的 UMD 产物有的把类
 * 直接挂在 window.Terminal，有的挂在 window.Terminal.Terminal，term.ts 顶部的
 * `window.X && (window.X.X || window.X)` 就是在抹平这个差异。 */

/* ---------------- xterm ---------------- */

interface XtermTheme {
  background?: string;
  foreground?: string;
  cursor?: string;
  selectionBackground?: string;
}

interface XtermOptions {
  fontFamily?: string;
  fontSize?: number;
  cursorBlink?: boolean;
  theme?: XtermTheme;
  /** 切换它的底层实现是给 textarea 改 readOnly，会打断 IME，见 term.ts 的 refocusTerm */
  disableStdin?: boolean;
}

interface XtermBufferLine {
  /** trimRight=true 去掉行尾空白 */
  translateToString(trimRight?: boolean): string;
}

interface XtermBuffer {
  active: {
    getLine(y: number): XtermBufferLine | undefined;
  };
}

/** registerLinkProvider 回调里描述一处可点链接。 */
interface XtermLink {
  range: {
    start: { x: number; y: number };
    end: { x: number; y: number };
  };
  text: string;
  activate(event: MouseEvent, text: string): void;
}

interface XtermLinkProvider {
  provideLinks(y: number, callback: (links: XtermLink[] | undefined) => void): void;
}

/** xterm 插件（FitAddon / WebglAddon 都满足）。 */
interface XtermAddon {
  dispose?(): void;
}

interface XtermTerminal {
  readonly cols: number;
  readonly rows: number;
  readonly buffer: XtermBuffer;
  readonly modes: { applicationCursorKeysMode: boolean };
  /** 承载键盘输入的隐藏 textarea；IME 相关处理需要直接操作它 */
  readonly textarea: HTMLTextAreaElement | undefined;
  /** open 之后的根元素，随实例销毁；挂监听用它而不是 #term-mount，避免重建实例后叠加 */
  readonly element: HTMLElement | undefined;
  options: XtermOptions;
  open(parent: HTMLElement): void;
  write(data: string | Uint8Array): void;
  focus(): void;
  dispose(): void;
  loadAddon(addon: XtermAddon): void;
  onData(handler: (data: string) => void): void;
  onResize(handler: (size: { cols: number; rows: number }) => void): void;
  /** 老版本 xterm 没有这个方法，term.ts 里先判空再用 */
  registerLinkProvider?(provider: XtermLinkProvider): void;
}

interface XtermTerminalConstructor {
  new (options?: XtermOptions): XtermTerminal;
  /** UMD 命名空间形态：window.Terminal.Terminal */
  Terminal?: XtermTerminalConstructor;
}

interface XtermFitAddon extends XtermAddon {
  fit(): void;
}

interface XtermFitAddonConstructor {
  new (): XtermFitAddon;
  FitAddon?: XtermFitAddonConstructor;
}

interface XtermWebglAddon extends XtermAddon {
  onContextLoss(handler: () => void): void;
  dispose(): void;
}

interface XtermWebglAddonConstructor {
  new (): XtermWebglAddon;
  WebglAddon?: XtermWebglAddonConstructor;
}

/* ---------------- KaTeX ---------------- */

interface KatexRenderOptions {
  displayMode?: boolean;
  /** false = 把错误画成红字留在原位，不抛异常 */
  throwOnError?: boolean;
}

interface Katex {
  render(tex: string, element: HTMLElement, options?: KatexRenderOptions): void;
}

/* ---------------- window 扩展 ---------------- */

interface Window {
  /** vendor/xterm.js；未加载时为 undefined，term.ts 会降级提示 */
  Terminal?: XtermTerminalConstructor;
  /** vendor/addon-fit.js */
  FitAddon?: XtermFitAddonConstructor;
  /** vendor/addon-webgl.js；不可用时回退默认 DOM 渲染器 */
  WebglAddon?: XtermWebglAddonConstructor;
  /** vendor/katex/katex.min.js；未加载时公式回退成原样文本 */
  katex?: Katex;
  /** Safari 及旧 Chrome 的 Web Speech API 前缀实现 */
  webkitSpeechRecognition?: SpeechRecognitionConstructor;
}

/* ---------------- Web Speech API ----------------
 * lib.dom.d.ts 收录了事件与结果那几个类型（SpeechRecognitionEvent /
 * SpeechRecognitionErrorEvent / SpeechRecognitionResultList / …）以及
 * SpeechRecognitionErrorCode，但没有识别器本体，也没有 Safari/旧 Chrome 的
 * webkit 前缀版。缺的这两处在此补齐，已有的不重复声明。 */

interface SpeechRecognition extends EventTarget {
  lang: string;
  continuous: boolean;
  interimResults: boolean;
  onresult: ((event: SpeechRecognitionEvent) => void) | null;
  onerror: ((event: SpeechRecognitionErrorEvent) => void) | null;
  onend: (() => void) | null;
  start(): void;
  stop(): void;
  abort(): void;
}

interface SpeechRecognitionConstructor {
  new (): SpeechRecognition;
}

declare var SpeechRecognition: SpeechRecognitionConstructor | undefined;
