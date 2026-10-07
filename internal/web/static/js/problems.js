export const problemMessages = {
    "chat_request_conflict": ["消息编号与已接收内容或状态不一致", "请查询原消息状态，核对后使用新的消息编号。"],
    "chat_request_gone": ["该消息已放弃或删除", "请核对历史和工作区结果；不会重新执行这个消息编号。"],
    "chat_request_not_found": ["尚未查到该消息的接收记录", "请保留待确认内容并重试查询，不要换编号重复发送。"],
    "chat_pending": ["有消息尚未结束或结果待核对", "请先查询消息状态、查看历史和文件，再明确核对结果。"],
    "chat_scope_changed": ["消息所属的实例或登录身份已变化", "请重新登录并核对原任务；不会自动重新发送。"],
    "chat_thread_changed": ["当前对话与消息所属对话不一致", "请返回原对话并核对输入后再发送。"],
    "chat_attachments_invalid": ["消息附件已失效或无法验证", "请移除失效附件或重新上传，核对后再发送。"],
    "project_upload_limits": [
        "项目上传超过限制",
        "请减少文件数量或体积；大小限制可由管理员调整。"
    ],
    "creation_conflict": [
        "创建信息与已接收记录不一致",
        "请继续原创建流程，或明确开始新的创建。"
    ],
    "creation_gone": [
        "该创建已放弃或空间已删除",
        "请刷新空间列表；不会重新创建已删除空间。"
    ],
    "import_pending": [
        "导入仍在执行或结果待核对",
        "请先查询结果或查看已有目录，不要重复提交。"
    ],
    "import_failed": [
        "项目导入失败",
        "请检查上传内容、仓库地址或 Git 连接，在同一空间重试。"
    ],
    "import_target_exists": [
        "目标目录已存在",
        "请查看现有文件，或选择新的目录；不会覆盖原目录。"
    ],
    "invalid_request": [
        "请求格式错误",
        "请检查输入后重新提交。"
    ],
    "invalid_credentials": [
        "账号或密码错误",
        "请核对用户名和密码后重新登录。"
    ],
    "login_rate_limited": [
        "登录尝试过于频繁",
        "请稍后再登录；忘记密码请联系管理员。"
    ],
    "authentication_required": [
        "登录已过期",
        "请重新登录后继续。"
    ],
    "admin_required": [
        "需要管理员权限",
        "请联系实例管理员处理。"
    ],
    "session_not_found": [
        "工作空间不存在或不可访问",
        "请刷新空间列表并重新选择。"
    ],
    "account_access_denied": [
        "当前用户无权使用该账号",
        "请联系管理员调整账号使用范围。"
    ],
    "account_unavailable": [
        "空间绑定的账号不可用",
        "请联系管理员检查账号配置。"
    ],
    "account_credentials_unavailable": [
        "账号凭证无法准备",
        "请联系管理员检查凭证并在需要时重新授权账号。"
    ],
    "quota_exhausted": [
        "额度已用完",
        "请联系管理员充值后再继续。"
    ],
    "capacity_exhausted": [
        "运行空间数量已达上限",
        "请停止不用的空间，或联系管理员调整容量。"
    ],
    "storage_full": [
        "数据盘可用空间不足",
        "请联系管理员释放磁盘空间后重试。"
    ],
    "storage_permission_denied": [
        "数据目录权限不足",
        "请联系管理员检查数据目录及容器用户的读写权限。"
    ],
    "docker_unavailable": [
        "无法连接 Docker 服务",
        "请联系管理员检查 Docker 服务和访问权限。"
    ],
    "agent_image_missing": [
        "工作空间镜像不存在",
        "请联系管理员构建或安装配置中指定的 Agent 镜像。"
    ],
    "workspace_start_failed": [
        "工作空间启动失败",
        "请将操作编号交给管理员检查环境，修复后再启动。"
    ],
    "chat_busy": [
        "上一条消息仍在处理中",
        "请等待当前回合完成，或先中断当前回合。"
    ],
    "chat_options_invalid": [
        "对话模型或推理设置不可用",
        "请调整模型和推理设置，必要时联系管理员检查 CLI 兼容性。"
    ],
    "chat_failed": [
        "对话执行失败",
        "请先查看对话历史和工作区结果，再决定是否重新发送。"
    ],
    "mcp_configuration_failed": [
        "MCP 配置尚未应用",
        "请在 MCP 设置中处理冲突或检测配置后重试。"
    ],
    "operation_timed_out": [
        "操作超时",
        "请先检查当前空间和任务状态，再决定是否重试。"
    ],
    "operation_cancelled": [
        "操作已取消",
        "请检查当前空间和任务状态后再继续。"
    ],
    "server_stopping": [
        "服务正在重启或停止",
        "请稍后重新连接并核对任务状态。"
    ],
    "websocket_failed": [
        "对话连接不可用",
        "请检查网络及反向代理的 WebSocket 配置后重连。"
    ],
    "internal_error": [
        "服务暂时无法完成操作",
        "请将操作编号交给管理员排查。"
    ]
};
const validID = (value) => typeof value === "string" && /^[a-f0-9]{32}$/.test(value);
export function formatProblem(problem, translate, fallback = "") {
    const known = problem.code && Object.hasOwn(problemMessages, problem.code) ? problemMessages[problem.code] : undefined;
    const message = known ? translate(known[0]) : (typeof problem.error === "string" ? problem.error : fallback);
    const hint = known ? translate(known[1]) : (typeof problem.hint === "string" ? problem.hint : "");
    const reference = validID(problem.operation_id) ? translate("操作编号：{id}", { id: problem.operation_id }) : "";
    return [message, hint, reference].filter(Boolean).join(" ");
}
export class APIError extends Error {
    problem;
    status;
    constructor(problem, status, translate, fallback) {
        super(formatProblem(problem, translate, fallback));
        this.problem = problem;
        this.status = status;
        this.name = "APIError";
    }
}
export async function responseError(response, translate, fallback = response.statusText) {
    let problem = {};
    try {
        const body = await response.json();
        if (body && typeof body === "object" && !Array.isArray(body))
            problem = body;
    }
    catch { /* Old proxy errors may be non-JSON. */ }
    if (!validID(problem.operation_id)) {
        const id = response.headers.get("X-Agentbox-Operation-ID");
        if (validID(id))
            problem.operation_id = id;
    }
    return new APIError(problem, response.status, translate, fallback || translate("服务暂时无法完成操作"));
}
