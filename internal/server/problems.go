package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"strings"
	"syscall"

	"agentbox/internal/dockerx"
	"agentbox/internal/protocol"
	"agentbox/internal/workspace"
	"github.com/docker/docker/client"
)

// Additive error contract: old clients continue to read Error. Everything in
// this payload is from this allowlist; never include err.Error(), URLs or paths.
type ProblemDetails = protocol.ProblemDetails
type apiProblem = protocol.APIProblem

type problemDefinition struct {
	status                int
	message, hint, action string
	retryable             bool
}

var problemCatalog = map[string]problemDefinition{
	"chat_request_conflict":           {409, "消息编号与已接收内容或状态不一致", "请查询原消息状态，核对后使用新的消息编号。", "check_result", false},
	"chat_request_gone":               {410, "该消息已放弃或删除", "请核对历史和工作区结果；不会重新执行这个消息编号。", "check_result", false},
	"chat_request_not_found":          {404, "尚未查到该消息的接收记录", "请保留待确认内容并重试查询，不要换编号重复发送。", "check_result", false},
	"chat_pending":                    {409, "有消息尚未结束或结果待核对", "请先查询消息状态、查看历史和文件，再明确核对结果。", "check_result", false},
	"chat_scope_changed":              {409, "消息所属的实例或登录身份已变化", "请重新登录并核对原任务；不会自动重新发送。", "sign_in", false},
	"chat_thread_changed":             {409, "当前对话与消息所属对话不一致", "请返回原对话并核对输入后再发送。", "check_result", false},
	"chat_attachments_invalid":        {409, "消息附件已失效或无法验证", "请移除失效附件或重新上传，核对后再发送。", "edit_request", false},
	"project_upload_limits":           {413, "项目上传超过限制", "请减少文件数量或体积；大小限制可由管理员调整。", "edit_request", false},
	"creation_conflict":               {409, "创建信息与已接收记录不一致", "请继续原创建流程，或明确开始新的创建。", "review_creation", false},
	"creation_gone":                   {410, "该创建已放弃或空间已删除", "请刷新空间列表；不会重新创建已删除空间。", "refresh_sessions", false},
	"import_pending":                  {409, "导入仍在执行或结果待核对", "请先查询结果或查看已有目录，不要重复提交。", "check_result", false},
	"import_failed":                   {422, "项目导入失败", "请检查上传内容、仓库地址或 Git 连接，在同一空间重试。", "review_import", false},
	"import_target_exists":            {409, "目标目录已存在", "请查看现有文件，或选择新的目录；不会覆盖原目录。", "check_result", false},
	"invalid_request":                 {400, "请求格式错误", "请检查输入后重新提交。", "edit_request", false},
	"invalid_credentials":             {401, "账号或密码错误", "请核对用户名和密码后重新登录。", "sign_in", false},
	"login_rate_limited":              {429, "登录尝试过于频繁", "请稍后再登录；忘记密码请联系管理员。", "wait", true},
	"authentication_required":         {401, "登录已过期", "请重新登录后继续。", "sign_in", false},
	"admin_required":                  {403, "需要管理员权限", "请联系实例管理员处理。", "contact_admin", false},
	"session_not_found":               {404, "工作空间不存在或不可访问", "请刷新空间列表并重新选择。", "refresh_sessions", false},
	"account_access_denied":           {403, "当前用户无权使用该账号", "请联系管理员调整账号使用范围。", "contact_admin", false},
	"account_unavailable":             {500, "空间绑定的账号不可用", "请联系管理员检查账号配置。", "contact_admin", false},
	"account_credentials_unavailable": {500, "账号凭证无法准备", "请联系管理员检查凭证并在需要时重新授权账号。", "reauthorize_account", false},
	"quota_exhausted":                 {403, "额度已用完", "请联系管理员充值后再继续。", "contact_admin", false},
	"capacity_exhausted":              {429, "运行空间数量已达上限", "请停止不用的空间，或联系管理员调整容量。", "free_capacity", false},
	"storage_full":                    {429, "数据盘可用空间不足", "请联系管理员释放磁盘空间后重试。", "contact_admin", false},
	"storage_permission_denied":       {500, "数据目录权限不足", "请联系管理员检查数据目录及容器用户的读写权限。", "contact_admin", false},
	"docker_unavailable":              {500, "无法连接 Docker 服务", "请联系管理员检查 Docker 服务和访问权限。", "contact_admin", false},
	"agent_image_missing":             {500, "工作空间镜像不存在", "请联系管理员构建或安装配置中指定的 Agent 镜像。", "contact_admin", false},
	"workspace_start_failed":          {500, "工作空间启动失败", "请将操作编号交给管理员检查环境，修复后再启动。", "contact_admin", false},
	"chat_busy":                       {409, "上一条消息仍在处理中", "请等待当前回合完成，或先中断当前回合。", "wait", true},
	"chat_options_invalid":            {400, "对话模型或推理设置不可用", "请调整模型和推理设置，必要时联系管理员检查 CLI 兼容性。", "edit_request", false},
	"chat_failed":                     {500, "对话执行失败", "请先查看对话历史和工作区结果，再决定是否重新发送。", "check_result", false},
	"mcp_configuration_failed":        {409, "MCP 配置尚未应用", "请在 MCP 设置中处理冲突或检测配置后重试。", "configure_mcp", false},
	"operation_timed_out":             {504, "操作超时", "请先检查当前空间和任务状态，再决定是否重试。", "check_result", false},
	"operation_cancelled":             {409, "操作已取消", "请检查当前空间和任务状态后再继续。", "check_result", false},
	"server_stopping":                 {503, "服务正在重启或停止", "请稍后重新连接并核对任务状态。", "wait", true},
	"websocket_failed":                {400, "对话连接不可用", "请检查网络及反向代理的 WebSocket 配置后重连。", "reconnect", true},
	"internal_error":                  {500, "服务暂时无法完成操作", "请将操作编号交给管理员排查。", "contact_admin", false},
}

type operationContextKey struct{}

var operationIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func newOperationID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}

func operationContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, operationContextKey{}, newOperationID())
}

// No ResponseWriter wrapper: WebSocket Hijacker and streaming stay intact.
func operationHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newOperationID()
		// Browsers cannot inspect a failed WS handshake response. Allow only a
		// bounded opaque connection reference so the UI and handshake log agree.
		// This is neither authentication nor an idempotency key.
		if r.URL.Path == "/api/diagnostics/ws" || strings.HasPrefix(r.URL.Path, "/api/sessions/") && (strings.HasSuffix(r.URL.Path, "/chat") || strings.HasSuffix(r.URL.Path, "/diagnostics/ws")) {
			if candidate := r.URL.Query().Get("connection_id"); operationIDPattern.MatchString(candidate) {
				id = candidate
			}
		}
		w.Header().Set("X-Agentbox-Operation-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), operationContextKey{}, id)))
	})
}

func operationProblem(ctx context.Context, operation, code string) apiProblem {
	definition, ok := problemCatalog[code]
	if !ok {
		code, definition = "internal_error", problemCatalog["internal_error"]
	}
	id, _ := ctx.Value(operationContextKey{}).(string)
	if id == "" {
		id = newOperationID()
	}
	// Deliberately no raw cause, actor, URL/query, prompt or credential data.
	log.Printf("operation_failed operation_id=%s operation=%s code=%s action=%s retryable=%t", id, operation, code, definition.action, definition.retryable)
	return apiProblem{Error: definition.message, ProblemDetails: ProblemDetails{Code: code, OperationID: id, Hint: definition.hint, Action: definition.action, Retryable: definition.retryable}}
}

func writeProblem(w http.ResponseWriter, r *http.Request, operation, code string) {
	p := operationProblem(r.Context(), operation, code)
	w.Header().Set("X-Agentbox-Operation-ID", p.OperationID)
	writeJSON(w, problemCatalog[p.Code].status, p)
}

func classifyProblem(err error, fallback string) string {
	switch {
	case errors.Is(err, errAccountAccess):
		return "account_access_denied"
	case errors.Is(err, errAccountGone):
		return "account_unavailable"
	case errors.Is(err, workspace.ErrSessionGone):
		return "session_not_found"
	case errors.Is(err, workspace.ErrDiskSpace), errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return "storage_full"
	case errors.Is(err, workspace.ErrCapacity):
		return "capacity_exhausted"
	case errors.Is(err, dockerx.ErrImageMissing):
		return "agent_image_missing"
	case client.IsErrConnectionFailed(err):
		return "docker_unavailable"
	case errors.Is(err, fs.ErrPermission):
		return "storage_permission_denied"
	case errors.Is(err, context.DeadlineExceeded):
		return "operation_timed_out"
	case errors.Is(err, context.Canceled):
		return "operation_cancelled"
	case errors.Is(err, workspace.ErrCredentials):
		return "account_credentials_unavailable"
	default:
		return fallback
	}
}
