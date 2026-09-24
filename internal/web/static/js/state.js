/* state：全局可变状态 S + 跨模块事件总线 bus。
 * bus 只用于打破环形依赖的少数场景（api→登录、外壳→功能模块、数据刷新广播），
 * 其余模块间调用一律显式 import。事件清单：
 *   unauthorized             — api 收到 401
 *   data-updated             — refreshAll 拉到新的 sessions/accounts
 *   open-session  {detail}   — 侧栏点击会话卡片
 *   open-settings            — 侧栏点击系统设置
 *   open-usage               — 侧栏点击使用记录
 *   open-tunnel              — 侧栏点击内网隧道
 *   app-ready                — 登录验证及初始数据加载完成，恢复 URL 页面
 *   navigation-changed       — 页面/分区/标签变化，将最终状态写入 URL
 *   thread-changed           — 对话线程切换/新建/删除，chat.ts 重载对话流
 *   tips-updated             — 终端提示语配置变化（登录下发 / 管理员保存），term.ts 重排轮播
 *   timezone-updated         — 系统界面时区变化，使用记录重绘时间 */
"use strict";
export const bus = new EventTarget();
export const emit = (type, detail) => bus.dispatchEvent(new CustomEvent(type, { detail }));
export const S = {
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
