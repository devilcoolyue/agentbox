/* types：服务端 API 与 WebSocket 报文的类型定义。
 *
 * 这里的每个接口都对应 Go 侧一个结构体（字段名即 json tag），改服务端报文时
 * 两边要一起改。故意只声明前端真正读到的字段：多写不会有编译期收益，少写会
 * 在用到时立刻报错，比写成 any 安全。
 *
 * 纯类型文件用 .d.ts，编译后不产生多余的 js —— 输出目录严格保持 22 个模块。 */

/* ---------------- 会话 / 账号 ---------------- */

/** GET /api/sessions、POST /api/sessions 等的会话视图（server.sessionView）。 */
export interface Session {
  id: string;
  user: string;
  name: string;
  /** "claude" | "codex"，服务端不保证只有这两个值，故留 string */
  agent: string;
  account_id: string;
  /** 创建空间时保存的默认模型。 */
  default_model: string;
  container_id?: string;
  status: string;
  chat_session?: string;
  /** "idle" = 空闲回收器停的（前端展示为「休眠」），"" = 用户手动停或从未运行 */
  stop_reason?: string;
  created_at: string;
  updated_at: string;
  /** sessionView 在 store.Session 之上补的账号展示名 */
  account_label: string;
}

export interface AccountAccess {
  mode: "all" | "users" | "admin";
  users?: string[];
}

/** GET /api/accounts 的账号池条目（server.acctView）。 */
export interface Account {
  /** 仅管理员可见；缺省表示全体用户共享。 */
  access?: AccountAccess;
  id: string;
  /** "claude" | "codex" */
  type: string;
  label: string;
  /** 正在使用该账号的会话数 */
  sessions: number;
  /** "ok" | "norefresh" | "missing" */
  cred_status: string;
  /** claude access token 到期时间(ms) */
  expires_at?: number;
  /** claude："oauth" | "apikey"（中转站） */
  auth_mode?: string;
  base_url?: string;
  /** codex："responses" | "chat" */
  wire_api?: string;
  env?: Record<string, string>;
  /** 绑定的出口 IP 代理 id；空 = 直连（仅管理员可见） */
  proxy_id?: string;
  /** 绑定代理的展示名，如 "香港节点 · socks5://1.2.3.4:1080" */
  proxy_label?: string;
}

/** GET /api/proxies 的一行：IP 代理池里的一个出口。密码永不下发。 */
export interface Proxy {
  id: string;
  name: string;
  /** "socks5" | "http" | "https" */
  scheme: string;
  host: string;
  port: number;
  username?: string;
  /** 是否设过密码（用于弹窗里显示掩码占位） */
  has_pass: boolean;
  disabled: boolean;
  /** scheme://host:port，不含凭证 */
  url: string;
  /** 绑定该代理的账号数 */
  accounts: number;
}

/** GET /api/proxies */
export interface ProxyList {
  proxies: Proxy[];
  /** 桥接监听地址（host:port） */
  bridge_bind: string;
  /** 注入容器的地址（host:port） */
  bridge_host: string;
  /** 桥接是否真的在监听 */
  bridge_up: boolean;
  bridge_error?: string;
}

/** POST /api/proxies/test */
export interface ProxyTest {
  ok: boolean;
  latency_ms: number;
  /** 探测到的出口 IP */
  exit_ip?: string;
  /** 实际探通的回显地址 */
  endpoint?: string;
  error?: string;
}

/** POST /api/proxies/import */
export interface ProxyImport {
  added: number;
  /** 与已有条目 host:port 重复、被跳过的行数 */
  duplicates: number;
  /** 解析失败的行（最多 10 条） */
  errors: string[];
}

/** POST /api/accounts/{id}/oauth/start */
export interface OAuthStart {
  /** 让用户去浏览器打开的授权地址 */
  url: string;
}

/** POST /api/accounts/{id}/oauth/finish */
export interface OAuthFinish {
  ok: boolean;
  /** pro / max / …，查不到订阅身份时为空 */
  subscription_type: string;
  /** access token 到期时间(ms) */
  expires_at: number;
}

/** POST /api/accounts/{id}/apikey/test：拉一次 /v1/models 探活。 */
export interface ApiKeyTest {
  ok: boolean;
  /** 实际探测到的接口地址 */
  endpoint: string;
  latency_ms: number;
  /** 已排序的模型 id 列表 */
  models: string[];
}

/* ---------------- 额度 ---------------- */

/** server.quotaView：/me 与 /users 下发的额度视图，金额一律微美元整数。 */
export interface Quota {
  user: string;
  /** false = 不限额，前端不显示余额 */
  metered: boolean;
  /** 余额见底是否真的拦截新回合 */
  enforced: boolean;
  /** 当前是否已被拦截 */
  blocked: boolean;
  balance_micro_usd: number;
  granted_micro_usd: number;
  spent_micro_usd: number;
  updated_at?: number;
}

/** server.ledgerView：额度流水的一条不可变记录。 */
export interface LedgerEntry {
  ts: number;
  ref: string;
  /** "spend" | "grant" | "adjust" */
  reason: string;
  delta_micro_usd: number;
  balance_after: number;
  note?: string;
  /** 充值/冲正的操作者 */
  actor?: string;
}

/** GET /api/users/{name}/quota */
export interface QuotaDetail {
  quota: Quota;
  ledger: LedgerEntry[];
}

/** POST /api/users/{name}/credits：applied=false 表示 ref 幂等键命中，未重复入账。 */
export interface CreditResult {
  quota: Quota;
  applied: boolean;
}

/** GET /api/usage 的按维度聚合行。 */
export interface UsageRow {
  key: string;
  turns: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cost_micro_usd: number;
}

/** GET /api/usage/events 的一行明细。
 *
 * 一行 = 一个回合 × 一个模型，**不是一次 API 调用**：容器里的 CLI 直接打
 * provider，我们不在链路上，只拿得到 CLI 在回合收尾汇总报的那份账。 */
export interface UsageEventRow {
  id: number;
  /** Unix 毫秒 */
  ts: number;
  user: string;
  session_name?: string;
  session_id: string;
  thread_id?: string;
  turn_id: string;
  agent: string;
  account_id?: string;
  account_label?: string;
  model?: string;
  /** "chat" 用户的对话 | "title" 服务端自动起标题 */
  kind: string;
  /** provider 自报的服务方（claude 的 "firstParty"）；codex 不报 */
  provider?: string;
  /** "provider" provider 报价 | "table" 价目表折算 | "none" 未定价 */
  billing: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  cost_micro_usd: number;
  /** provider 自报的模型侧耗时，不含 CLI 启动；回合级指标（同回合各行重复，跨行求和无意义） */
  duration_ms: number;
  /** 我们自己量的回合墙钟（容器就绪→进程退出，含 CLI 启动），与 ttft 同一块表；0 = 老数据没量过 */
  wall_ms: number;
  /** 首字延迟，同上；0 = 没量到 */
  ttft_ms: number;
  /** 这一行的入账单价快照；旧数据回退当前参考单价，用来把费用逐项摊开；查不到价时没有这个字段 */
  rate?: UsageRate;
}

/** 一行消耗对应的单价（已按档位选好），美元 / 百万 token。 */
export interface UsageRate extends TokenRates {
  /** true 表示入账时保存的价目表快照；缺省为旧数据的当前参考价。 */
  snapshot?: boolean;
  /** 命中的价目表键：模型 ID，或作为兜底的 agent 名 */
  key: string;
  /** "table" 这行的钱就是它算出来的 | "reference" provider 自报了总额，这份只是照价目表推的参考拆分 */
  basis: string;
  /** 真时表示这个回合走的是长上下文档单价 */
  long?: boolean;
  long_context_over?: number;
}

/** 整个筛选范围的合计，不随翻页变化。turns 按 turn_id 去重。 */
export interface UsageTotals {
  rows: number;
  turns: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cost_micro_usd: number;
}

/** GET /api/usage/events。facets 是筛选下拉的可选值（服务端按可见范围裁过）。 */
export interface UsageEvents {
 sync?: { last_scan_at: number; last_success_at: number; scanning: boolean; errors: number };
  rows: UsageEventRow[];
  total: UsageTotals;
  facets: { users: string[]; agents: string[]; models: string[] };
  limit: number;
  offset: number;
  /** 这一页实际用的时间排序方向："desc" 最新在前（默认）| "asc" 最早在前 */
  order: string;
  /** "self" 只看得到自己（隐藏用户列）| "all" 管理员看全部 */
  scope: string;
  /** 服务端实际用于解析本次时间筛选的 IANA 时区 */
  timezone: string;
}

/* ---------------- Claude 订阅额度 ---------------- */

/** 一条限额窗口（5 小时 / 本周 / 本周 Opus…）。percent 是 0-100 的使用率。 */
export interface UsageWindow {
  key: string;
  label: string;
  percent: number;
  /** RFC3339；部分窗口上游不下发 */
  resets_at?: string;
}

/** 订阅额度用尽后继续走的「额外用量」信用金，金额已换算成货币单位。 */
export interface UsageExtra {
  enabled: boolean;
  percent: number;
  used: number;
  limit: number;
  currency?: string;
}

/** GET /api/sessions/{id}/account/usage */
export interface AccountUsage {
  account_id: string;
  account_label: string;
  /** 订阅档位："pro" | "max" | ... */
  plan?: string;
  windows: UsageWindow[];
  extra?: UsageExtra;
  fetched_at: string;
}

/* ---------------- 用户 ---------------- */

/** server.userView：管理端用户列表条目。 */
export interface User {
  name: string;
  /** "admin" | "user" */
  role: string;
  sessions: number;
  /** 毫秒时间戳 */
  created_at: number;
  quota: Quota;
}

/** GET /api/me */
export interface Me {
  user: string;
  /** "admin" | "user"：admin 才能进系统设置 */
  role: string;
  models: Record<string, ModelOption[]> | null;
  terminal_tips: TerminalTips | null;
  /** 控制台显示与使用记录筛选使用的 IANA 时区 */
  timezone: string;
  quota: Quota | null;
}

/* ---------------- 系统设置 ---------------- */

/** config.ModelOption：对话选择器里的一个可选模型。 */
export interface ModelOption {
  id: string;
  label: string;
}

/** config.TerminalTips：终端页顶栏轮播提示语。interval_sec <= 0 关闭轮播。 */
export interface TerminalTips {
  tips: string[];
  interval_sec: number;
  /** 目前只有 "scroll"（纵向滚动） */
  animation: string;
}

/** config.ContainerLimits：会话容器资源限额。 */
export interface ContainerLimits {
  memory_mb: number;
  cpus: number;
  pids_limit: number;
  network: string;
}

/** config.TunnelConfig：反向隧道（容器经用户机器出网）配置。 */
export interface TunnelConfig {
  enabled: boolean;
  /** SOCKS5 代理监听的 host:port，需容器可达 */
  proxy_bind?: string;
  /** 注入给容器的代理地址主机名，默认取 proxy_bind 的 host */
  proxy_host?: string;
}

/** GET/PUT /api/settings（server.settingsView）。 */
export interface Settings {
 resources: ResourceLimits;
  listen: string;
  agent_image: string;
  permission_mode: string;
  max_upload_mb: number;
  /** 会话空闲自动停机的分钟数；0 = 关闭 */
  idle_timeout_min: number;
  /** 控制台显示与使用记录筛选使用的 IANA 时区 */
  timezone: string;
  container: ContainerLimits;
  models: Record<string, ModelOption[]>;
  default_models: Record<string, string>;
  terminal_tips: TerminalTips;
  tunnel: TunnelConfig;
  /** SOCKS 代理是否真的在监听 */
  tunnel_active: boolean;
  /** 最近一次隧道启停失败的原因 */
  tunnel_error?: string;
  /** 账号出口代理的本地 HTTP 桥接监听配置 */
  proxy_bridge: ProxyBridgeConfig;
  /** 按 token 折算费用的价目表，键是模型 ID 或 agent 名 */
  pricing: Record<string, ModelPrice>;
  /** listen 改过但未重启 */
  restart_required: boolean;
}

/** 一档单价，美元 / 百万 token。0 表示这一桶免费。 */
export interface TokenRates {
  input: number;
  output: number;
  cache_read: number;
  cache_write: number;
}

/** 一个模型（或一个 agent 兜底）的价。long 是长上下文档：
 * 提示词超过 long_context_over 个 token 时，**整个回合**按 long 计价——是台阶不是加价。 */
export interface ModelPrice extends TokenRates {
  long_context_over?: number;
  long?: TokenRates;
}

export interface ProxyBridgeConfig {
  /** 桥接监听地址 host:port，默认 172.17.0.1:1081 */
  bind: string;
  /** 注入容器时用的主机名，留空取 bind 的 host */
  host?: string;
}

/** GET /api/system（关于页）。 */
export interface SystemInfo {
 version?: string; revision?: string; built_at?: string; schema_version?: number;
  go_version: string;
  data_dir: string;
  config_path: string;
  /** 连不上 docker 时为空串 */
  docker_version: string;
  sessions_total: number;
  sessions_running: number;
  accounts: number;
  /** 用户数（含管理员） */
  users: number;
  /** 进程启动时间(ms) */
  started_at: number;
  /** 启动时实际绑定的监听地址 */
  listen: string;
}

/* ---------------- 监控 ---------------- */

export interface ProcessStat {
  /** 单核百分比，多核可超过 100 */
  cpu_percent: number;
  rss: number;
  heap_alloc: number;
  heap_sys: number;
  goroutines: number;
  uptime_ms: number;
}

export interface HostStat {
  cpu_count: number;
  /** 全核合计占用 0-100 */
  cpu_percent: number;
  load1: number;
  mem_total: number;
  mem_used: number;
  /** data_dir 所在文件系统容量；读不到为 0 */
  disk_total: number;
  disk_used: number;
}

export interface MonitorSummary {
  total: number;
  running: number;
  cpu_percent: number;
  mem_usage: number;
}

export interface ContainerStat {
  session_id: string;
  name: string;
  user: string;
  agent: string;
  account_id: string;
  running: boolean;
  created_at: number;
  /** 容器本次启动时间(ms)，0 = 未运行/未知 */
  started_at: number;
  cpu_percent: number;
  mem_usage: number;
  mem_limit: number;
  pids: number;
}

/** GET /api/monitor */
export interface Monitor {
  /** 服务端当前时间(ms)，前端据此算运行时长 */
  now: number;
  /** 采样窗口(ms)，0 = 首次采样，速率类指标还没有上一帧可做差 */
  window_ms: number;
  process: ProcessStat;
  host: HostStat;
  summary: MonitorSummary;
  containers: ContainerStat[];
}

/* ---------------- 文件 / 变更 ---------------- */

/** GET /api/sessions/{id}/files 的目录项。 */
export interface FileEntry {
  name: string;
  is_dir: boolean;
  size: number;
  /** 形如 -rw-r--r-- / drwxr-xr-x */
  mode: string;
  mtime: string;
}

/** git status --porcelain 的一条变更项。 */
export interface ChangeEntry {
  path: string;
  /** porcelain 两字符 XY */
  status: string;
  untracked: boolean;
}

/** GET /api/sessions/{id}/git/status：工作区里没有仓库时只返回 is_repo:false。 */
export interface GitStatus {
  is_repo: boolean;
  /** 当前查看的仓库，相对 workspace 的斜杠路径；"" 表示 workspace 本身 */
  repo?: string;
  /** 工作区里发现的全部仓库（含 repo 自己） */
  repos?: string[];
  branch?: string;
  files?: ChangeEntry[];
  /** 变更太多被截断（未跟踪的大目录会撑爆列表），files 只是前一部分 */
  truncated?: boolean;
}

/** POST /api/sessions/{id}/git/commit */
export interface GitCommitResult {
  output: string;
}

/** 技能页的两个范围：这个会话的 ~/.claude/skills，或用户模板。 */
export type SkillScope = "session" | "template";

/** GET /api/sessions/{id}/skills 里的一项。 */
export interface SkillInfo {
  name: string;
  /** SKILL.md front matter 里的 description，取不到为空串 */
  description: string;
  files: number;
  bytes: number;
  updated_at: string;
  /** session=会话自装，template=用户模板，global=服务器模板；只在 session 范围有意义 */
  source: string;
}

/** GET /api/marketplace 里的一条：官方市场的单位是插件，不一定含技能。 */
export interface MarketPlugin {
  name: string;
  display_name?: string;
  description: string;
  author?: string;
  category?: string;
  homepage?: string;
  keywords?: string[];
  /** 目录里显式声明的技能数；0/缺省表示没声明，装了才知道有没有 */
  skills?: number;
}

/** GET /api/marketplace */
export interface MarketCatalog {
  plugins: MarketPlugin[];
  categories: string[];
  updated_at: string;
  source: string;
}

/** GET /api/sessions/{id}/skills/{name} */
export interface SkillDetail extends SkillInfo {
  /** SKILL.md 正文 */
  content: string;
  /** 正文超长被截断 */
  truncated: boolean;
  /** 技能目录的扁平清单（含子目录与 SKILL.md 自己），父目录排在子项之前 */
  entries: SkillEntry[];
  /** 条目太多，清单被截断 */
  more: boolean;
}

/** SkillDetail.entries 里的一项，path 相对技能目录。 */
export interface SkillEntry {
  path: string;
  dir?: boolean;
  /** 符号链接：装技能时不会产生，会话里可能自己造；不支持预览 */
  link?: boolean;
  /** 有可执行位（scripts/ 下的脚本靠它才跑得起来） */
  exec?: boolean;
  size: number;
  mtime: string;
}

/** GET /api/sessions/{id}/skills/{name}/file?path=… */
export interface SkillFile {
  path: string;
  size: number;
  /** 形如 -rwxr-xr-x */
  mode: string;
  exec: boolean;
  mtime: string;
  /** 文本内容；binary 时为空 */
  content: string;
  truncated: boolean;
  binary: boolean;
}

/** POST /api/sessions/{id}/upload：压缩包会被解开，mode 区分两种处理。 */
export interface UploadSummary {
  /** "file" | "archive" */
  mode: string;
  /** archive 时是解出的文件数，file 时为 1 */
  files: number;
}

/** POST /api/sessions/{id}/images 的上传结果。 */
export interface UploadResult {
  /** 容器内可直接读取的绝对路径 */
  path: string;
  name: string;
  /** 原始文件名 */
  orig?: string;
}

/* ---------------- 内网反向隧道 ---------------- */

/** 一条端口映射（容器侧监听地址 → 用户机器上的目标）。 */
export interface TunnelMap {
  listen: string;
  target: string;
}

/** GET /api/tunnel/status：调用者自己的隧道状态。 */
export interface TunnelStatus {
  enabled: boolean;
  /** SOCKS5 代理是否在监听 */
  proxy_up: boolean;
  connected: boolean;
  /** enabled 时才有：容器该用的代理地址 */
  proxy?: string;
  /** 接入时间(ms) */
  since?: number;
  /** 客户端来源地址 */
  remote?: string;
  maps: TunnelMap[];
  /** 仅管理员可见 */
  online_users?: string[];
}

/** GET /api/tunnel/clients：可下载的 abox-link 客户端。 */
export interface TunnelClient {
  name: string;
  size: number;
}

/** POST /api/tunnel/pair：一次性配对码。 */
export interface TunnelPair {
  code: string;
  /** 有效期秒数 */
  expires_in: number;
}

/* ---------------- 对话线程 ---------------- */

/** server.threadMeta：一条对话线程的元数据。 */
export interface Thread {
  id: string;
  /** 模型总结的标题，没有则回退到首条用户消息（截断） */
  title: string;
  ts: string;
  updated: string;
  turns: number;
  /** 是否记录了可续聊的 provider 会话 id */
  resumable: boolean;
}

/** GET /api/sessions/{id}/history 的一条落盘记录。 */
export interface HistoryEntry {
  ts: string;
  /** "user" | "event" | "status" | "chat_session" | "title" | "divider"(旧) */
  kind: string;
  text?: string;
  event?: AgentEvent;
  state?: string;
  error?: string;
}

/** GET /api/sessions/{id}/history */
export interface History {
  entries: HistoryEntry[];
  thread: Thread | null;
}

/** GET /api/sessions/{id}/chat/threads */
export interface ThreadList {
  threads: Thread[];
  /** 当前激活线程的 id */
  active: string;
}

/** POST /api/sessions/{id}/chat/threads：created=false 表示当前已是空的新对话 */
export interface ThreadCreated {
  id: string;
  created: boolean;
}

/** POST …/threads/{tid}/activate：resumable=false 表示挖不出可续聊的 provider 会话 id */
export interface ThreadActivated {
  id: string;
  resumable: boolean;
}

/** PATCH …/threads/{tid} */
export interface ThreadRenamed {
  id: string;
  title: string;
}

/* ---------------- Agent 事件 ----------------
 * agent_event 的 event 体是 provider 原样透传的 JSON：Claude 的 stream-json
 * 事件、Codex 的 app-server item 事件，两家结构不同且各自还有新旧版本。这里
 * 只声明渲染管线真正读到的字段，全部可选 —— 拿不到就走兜底分支，与原来的
 * `ev && ev.type === ...` 防御式写法一致。 */

export interface ContentBlock {
  type?: string;
  text?: string;
  thinking?: string;
  name?: string;
  input?: unknown;
  content?: unknown;
  tool_use_id?: string;
  is_error?: boolean;
  [key: string]: unknown;
}

/** Claude 的 rate_limit_event 载荷：额度用量与重置时间。 */
export interface RateLimitInfo {
  /** "allowed" | "allowed_warning" | "rejected"，其余状态原样展示 */
  status?: string;
  /** five_hour / seven_day / seven_day_opus / … */
  rateLimitType?: string;
  /** 0–1 的已用比例 */
  utilization?: number;
  /** 重置时刻，Unix 秒 */
  resetsAt?: number;
  isUsingOverage?: boolean;
}

/** Codex item.completed 里 file_change 的一处改动 */
export interface FileChange {
  path: string;
}

export interface AgentEvent {
  type?: string;
  subtype?: string;
  /* Claude：assistant / user 事件的消息体。
   * 注意 Codex 旧版的 error 事件把一句错误文案直接塞在 message 上（字符串），
   * 渲染那处只做字符串拼接，故这里按主要用法声明成对象，不为那一处拆联合。 */
  message?: {
    role?: string;
    content?: ContentBlock[] | string;
    model?: string;
    [key: string]: unknown;
  };
  /** Claude：--include-partial-messages 的增量事件体 */
  event?: StreamEvent;
  /** Claude：rate_limit_event */
  rate_limit_info?: RateLimitInfo;
  /** Claude：result 事件的耗时与计费 */
  duration_ms?: number;
  total_cost_usd?: number;
  result?: string;
  is_error?: boolean;
  /** Codex：新版 app-server 的条目 */
  item?: {
    type?: string;
    text?: string;
    command?: string;
    query?: string;
    changes?: FileChange[];
    [key: string]: unknown;
  };
  /** Codex：旧版 exec --json 的消息体（command 可能是数组或字符串） */
  msg?: {
    type?: string;
    message?: string;
    text?: string;
    command?: string[] | string;
    [key: string]: unknown;
  };
  /** Codex：turn.completed 的 token 统计 */
  usage?: {
    input_tokens?: number;
    output_tokens?: number;
    [key: string]: unknown;
  };
  /** Codex：turn.failed 的错误体 */
  error?: { message?: string; [key: string]: unknown };
  [key: string]: unknown;
}

/** liveNode 产出的流式临时节点：body 由打字机填充，el 是插入对话流的外层。 */
export interface LiveNode {
  el: HTMLElement;
  body: HTMLElement;
}

/** Claude 的流式增量事件（content_block_start/delta/stop、message_start…）。 */
export interface StreamEvent {
  type?: string;
  content_block?: { type?: string; [key: string]: unknown };
  delta?: {
    type?: string;
    text?: string;
    thinking?: string;
    [key: string]: unknown;
  };
  [key: string]: unknown;
}

/* ---------------- WebSocket 报文 ---------------- */

/** 对话通道服务端 → 前端。type 决定其余字段，用可选字段而非联合类型，
 *  与 handleChatMsg 的 switch 写法直接对应。 */
export interface ChatMessage {
  type: string;
  /** user_message / agent_raw 的文本 */
  text?: string;
  /** agent_event 的事件体 */
  event?: AgentEvent;
  /** thread / thread_title 的线程 id */
  id?: string;
  /** thread_title 的标题 */
  title?: string;
  /** status 的状态："running" | "idle" | "error" */
  state?: string;
  /** status / error 的错误文案 */
  error?: string;
}

export interface ResourceLimits { max_running: number; max_running_per_user: number; min_free_bytes: number; }
