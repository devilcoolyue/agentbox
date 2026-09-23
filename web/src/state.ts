/* state：全局可变状态 S + 跨模块事件总线 bus。
 * bus 只用于打破环形依赖的少数场景（api→登录、外壳→功能模块、数据刷新广播），
 * 其余模块间调用一律显式 import。事件清单：
 *   unauthorized             — api 收到 401
 *   data-updated             — refreshAll 拉到新的 sessions/accounts
 *   open-session  {detail}   — 侧栏点击会话卡片
 *   open-settings            — 侧栏点击系统设置
 *   open-usage               — 侧栏点击使用记录
 *   open-tunnel              — 侧栏点击内网隧道
 *   thread-changed           — 对话线程切换/新建/删除，chat.ts 重载对话流
 *   tips-updated             — 终端提示语配置变化（登录下发 / 管理员保存），term.ts 重排轮播
 *   timezone-updated         — 系统界面时区变化，使用记录重绘时间 */
"use strict";

import type {
  Account, ModelOption, Quota, Session, TerminalTips, Thread,
} from "./types.js";

export const bus = new EventTarget();
export const emit = (type: string, detail?: unknown) =>
  bus.dispatchEvent(new CustomEvent(type, { detail }));

/** 主区当前视图 */
export type View = "work" | "settings" | "usage" | "tunnel";
/** 工作台当前标签页 */
export type Tab = "chat" | "term" | "files" | "changes" | "skills";
/** 文件浏览范围（shared = 共享目录，同用户所有会话可见） */
export type FileScope = "workspace" | "shared";
/** 对话回合状态，决定发送按钮是「发送」还是「中断」 */
export type ChatState = "idle" | "running";

/** 当前会话的模型 / 思考强度选择（"" = 跟随账号默认） */
export interface Pick {
  model: string;
  effort: string;
}

/* S 各字段的类型集中声明在此。原来纯 JS 时靠初值推断，null 初值会被推成 null
 * 类型，于是所有 S.current.xxx 都成了「属性不存在于 never」——这也是移植时最
 * 密集的一类报错。 */
export interface AppState {
  token: string;
  /** 当前登录用户名 */
  user: string;
  /** "admin" | "user"：admin 才能进系统设置 */
  role: string;
  sessions: Session[];
  accounts: Account[];
  /** 当前会话对象 */
  current: Session | null;
  /** 主区当前视图 */
  view: View;
  /** 设置页当前分区 */
  sec: string;
  tab: Tab;
  termWS: WebSocket | null;
  /** 终端连接代际：切换/收尾/手动重连时自增，作废旧连接的重连定时器 */
  termWSGen: number;
  /** 终端提示语配置 { tips, interval_sec, animation }（/me 下发，全用户可见） */
  termTips: TerminalTips | null;
  /** 系统界面时区；老服务端兜底到中国标准时间 */
  timeZone: string;
  term: XtermTerminal | null;
  fit: XtermFitAddon | null;
  filePath: string;
  fileScope: FileScope;
  /** 启动/停止执行中，期间锁住生命周期按钮 */
  actionBusy: boolean;
  chatState: ChatState;
  /** 当前对话线程元数据（/history 返回；空的新对话为 null） */
  thread: Thread | null;
  pick: Pick;
  /** 服务端下发的可选模型表 { claude: [{id,label}], codex: [...] } */
  models: Record<string, ModelOption[]> | null;
  /** 历史对话加载中：中央转圈，不闪新会话引导页 */
  histLoading: boolean;
  /** 历史加载失败：保留错误态，禁止误在未知线程上发送消息 */
  histError: string;
  /** 自己的额度 { metered, enforced, blocked, balance_micro_usd, ... }（/me 下发） */
  quota: Quota | null;
}

export const S: AppState = {
  token: localStorage.getItem("agentbox_token") || "",
  user: "",
  role: "",
  sessions: [],
  accounts: [],
  current: null,
  view: "work",
  sec: "accounts",
  tab: "chat",
  termWS: null,
  termWSGen: 0,
  termTips: null,
  timeZone: "Asia/Shanghai",
  term: null,
  fit: null,
  filePath: "",
  fileScope: "workspace",
  actionBusy: false,
  chatState: "idle",
  thread: null,
  pick: { model: "", effort: "" },
  models: null,
  histLoading: false,
  histError: "",
  quota: null,
};
