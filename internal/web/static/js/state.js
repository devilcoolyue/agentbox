/* state：全局可变状态 S + 跨模块事件总线 bus。
 * bus 只用于打破环形依赖的少数场景（api→登录、外壳→功能模块、数据刷新广播），
 * 其余模块间调用一律显式 import。事件清单：
 *   unauthorized             — api 收到 401
 *   data-updated             — refreshAll 拉到新的 sessions/accounts
 *   open-session  {detail}   — 侧栏点击会话卡片
 *   open-settings            — 侧栏点击系统设置
 *   thread-changed           — 对话线程切换/新建/删除，chat.js 重载对话流 */
"use strict";

export const bus = new EventTarget();
export const emit = (type, detail) => bus.dispatchEvent(new CustomEvent(type, { detail }));

export const S = {
  token: localStorage.getItem("agentbox_token") || "",
  user: "",            // 当前登录用户名
  role: "",            // "admin" | "user"：admin 才能进系统设置
  sessions: [],
  accounts: [],
  current: null,      // 当前会话对象
  view: "work",       // "work" | "settings"：主区当前视图
  sec: "accounts",    // 设置页当前分区
  settings: null,     // GET /api/settings 的缓存
  tab: "chat",
  chatWS: null,
  chatWSGen: 0,       // 防止旧连接的重连定时器复活
  termWS: null,
  term: null,
  fit: null,
  filePath: "",
  fileScope: "workspace", // "workspace" | "shared"（共享目录：同用户所有会话可见）
  refreshTimer: null,
  actionBusy: false,  // 启动/停止执行中，期间锁住生命周期按钮
  chatState: "idle",  // "idle" | "running"，决定发送按钮是发送还是中断
  thread: null,       // 当前对话线程元数据（/history 返回；空的新对话为 null）
  pick: { model: "", effort: "" }, // 当前会话的模型/思考强度选择（"" = 默认）
  models: null,       // 服务端下发的可选模型表 { claude: [{id,label}], codex: [...] }
  histLoading: false, // 历史对话加载中：中央转圈，不闪新会话引导页
};
