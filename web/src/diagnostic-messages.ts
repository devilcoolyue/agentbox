/** Safe diagnostic vocabulary, checked against the versioned Go fixture. */
export const diagnosticMessages: Record<string, readonly [string,string]> = {
  "config_ok": [
    "配置校验通过",
    "仅验证当前配置，不代表外部服务可用。"
  ],
  "config_invalid": [
    "配置校验失败",
    "请管理员检查配置文件的格式和必填设置。"
  ],
  "not_requested": [
    "尚未运行此项检查",
    "请主动运行环境检查。"
  ],
  "dependency_unavailable": [
    "前置检查未通过",
    "请先处理前置检查，再重新运行。"
  ],
  "check_cancelled": [
    "检查已取消或超时",
    "请稍后重新运行检查。"
  ],
  "docker_ok": [
    "Docker 服务可连接",
    "仅验证 daemon 接口，不启动容器。"
  ],
  "docker_unavailable": [
    "Docker 服务无法连接",
    "请管理员检查 Docker 服务及访问权限。"
  ],
  "image_ok": [
    "指定镜像存在",
    "仅确认镜像可读取，未验证 CLI 或模型调用。"
  ],
  "image_missing": [
    "指定镜像不存在",
    "请管理员构建或安装配置中指定的镜像。"
  ],
  "image_unavailable": [
    "无法检查指定镜像",
    "请管理员检查 Docker 镜像及访问权限。"
  ],
  "disk_ok": [
    "数据盘空间符合保留设置",
    "这是当前可用空间，不是工作区磁盘配额。"
  ],
  "storage_full": [
    "数据盘可用空间不足",
    "请管理员释放空间或核对磁盘保留设置。"
  ],
  "storage_unavailable": [
    "无法访问数据目录",
    "请管理员检查目录、挂载和读写权限。"
  ],
  "write_ok": [
    "服务端写入探测通过",
    "已创建、同步并删除独立临时文件，未修改用户文件。"
  ],
  "ownership_ok": [
    "容器属主设置探测通过",
    "已在独立临时文件上验证 1000:1000，不代表全部历史文件权限正确。"
  ],
  "ownership_not_checked": [
    "未验证 Linux 容器属主设置",
    "请在 Linux 服务端检查 1000:1000 的属主设置能力。"
  ],
  "permission_denied": [
    "目录或属主设置权限不足",
    "请管理员检查服务运行用户及目录权限。"
  ],
  "probe_cleanup_failed": [
    "临时探测文件清理失败",
    "请管理员核对数据目录中的诊断临时目录后重新检查。"
  ],
  "accounts_ok": [
    "账号配置可读取",
    "仅检查凭证配置存在，未验证登录有效性或模型权限。"
  ],
  "accounts_missing": [
    "没有可检查的账号配置",
    "请管理员接入账号，再运行检查。"
  ],
  "credentials_missing": [
    "账号凭证缺失或无法读取",
    "请管理员检查账号凭证或重新接入账号。"
  ],
  "account_access_ok": [
    "空间账号授权通过",
    "按空间属主的当前权限检查。"
  ],
  "account_access_denied": [
    "空间账号授权未通过",
    "请联系管理员检查账号是否存在及使用范围。"
  ],
  "quota_ok": [
    "当前额度允许开始回合",
    "回合收尾仍按实际用量结算。"
  ],
  "quota_exhausted": [
    "当前额度不足",
    "请联系管理员充值后再继续。"
  ],
  "workspace_permissions_ok": [
    "空间目录权限检查通过",
    "只检查工作区和 home 根目录，不扫描项目文件或启动容器。"
  ],
  "websocket_not_checked": [
    "未检查浏览器 WebSocket",
    "请在网页诊断窗口运行连接检查；CLI 不代表浏览器网络。"
  ],
  "websocket_ok": [
    "浏览器 WebSocket 探测通过",
    "已完成诊断通道握手并收到响应，不代表模型可用。"
  ],
  "websocket_failed": [
    "浏览器 WebSocket 探测失败",
    "请检查网络、登录状态与反向代理的 WebSocket 配置。"
  ],
  "model_not_checked": [
    "未调用模型",
    "本检查不消耗模型额度，不验证上游模型是否可用。"
  ]
};
