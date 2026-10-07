// Package diagnostics provides bounded, allowlisted environment observations.
// Passing configuration checks never means an upstream model was called.
package diagnostics

import (
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"runtime"
	"syscall"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/credentials"
	"agentbox/internal/safefs"
)

type State string

const (
	Passed     State = "passed"
	Failed     State = "failed"
	NotChecked State = "not_checked"
)

type Check struct {
	ID      string `json:"id"`
	State   State  `json:"state"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

type Report struct {
	Version     int     `json:"version"`
	Scope       string  `json:"scope"`
	CheckedAt   int64   `json:"checked_at"`
	OperationID string  `json:"operation_id,omitempty"`
	Checks      []Check `json:"checks"`
}

// Messages and hints are a closed vocabulary; errors and paths never enter it.
var Messages = map[string][2]string{
	"config_ok":                {"配置校验通过", "仅验证当前配置，不代表外部服务可用。"},
	"config_invalid":           {"配置校验失败", "请管理员检查配置文件的格式和必填设置。"},
	"not_requested":            {"尚未运行此项检查", "请主动运行环境检查。"},
	"dependency_unavailable":   {"前置检查未通过", "请先处理前置检查，再重新运行。"},
	"check_cancelled":          {"检查已取消或超时", "请稍后重新运行检查。"},
	"docker_ok":                {"Docker 服务可连接", "仅验证 daemon 接口，不启动容器。"},
	"docker_unavailable":       {"Docker 服务无法连接", "请管理员检查 Docker 服务及访问权限。"},
	"image_ok":                 {"指定镜像存在", "仅确认镜像可读取，未验证 CLI 或模型调用。"},
	"image_missing":            {"指定镜像不存在", "请管理员构建或安装配置中指定的镜像。"},
	"image_unavailable":        {"无法检查指定镜像", "请管理员检查 Docker 镜像及访问权限。"},
	"disk_ok":                  {"数据盘空间符合保留设置", "这是当前可用空间，不是工作区磁盘配额。"},
	"storage_full":             {"数据盘可用空间不足", "请管理员释放空间或核对磁盘保留设置。"},
	"storage_unavailable":      {"无法访问数据目录", "请管理员检查目录、挂载和读写权限。"},
	"write_ok":                 {"服务端写入探测通过", "已创建、同步并删除独立临时文件，未修改用户文件。"},
	"ownership_ok":             {"容器属主设置探测通过", "已在独立临时文件上验证 1000:1000，不代表全部历史文件权限正确。"},
	"ownership_not_checked":    {"未验证 Linux 容器属主设置", "请在 Linux 服务端检查 1000:1000 的属主设置能力。"},
	"permission_denied":        {"目录或属主设置权限不足", "请管理员检查服务运行用户及目录权限。"},
	"probe_cleanup_failed":     {"临时探测文件清理失败", "请管理员核对数据目录中的诊断临时目录后重新检查。"},
	"accounts_ok":              {"账号配置可读取", "仅检查凭证配置存在，未验证登录有效性或模型权限。"},
	"accounts_missing":         {"没有可检查的账号配置", "请管理员接入账号，再运行检查。"},
	"credentials_missing":      {"账号凭证缺失或无法读取", "请管理员检查账号凭证或重新接入账号。"},
	"account_access_ok":        {"空间账号授权通过", "按空间属主的当前权限检查。"},
	"account_access_denied":    {"空间账号授权未通过", "请联系管理员检查账号是否存在及使用范围。"},
	"quota_ok":                 {"当前额度允许开始回合", "回合收尾仍按实际用量结算。"},
	"quota_exhausted":          {"当前额度不足", "请联系管理员充值后再继续。"},
	"workspace_permissions_ok": {"空间目录权限检查通过", "只检查工作区和 home 根目录，不扫描项目文件或启动容器。"},
	"websocket_not_checked":    {"未检查浏览器 WebSocket", "请在网页诊断窗口运行连接检查；CLI 不代表浏览器网络。"},
	"websocket_ok":             {"浏览器 WebSocket 探测通过", "已完成诊断通道握手并收到响应，不代表模型可用。"},
	"websocket_failed":         {"浏览器 WebSocket 探测失败", "请检查网络、登录状态与反向代理的 WebSocket 配置。"},
	"model_not_checked":        {"未调用模型", "本检查不消耗模型额度，不验证上游模型是否可用。"},
}

func Result(id string, state State, code string) Check {
	text, ok := Messages[code]
	if !ok {
		code = "not_requested"
		state = NotChecked
		text = Messages[code]
	}
	return Check{ID: id, State: state, Code: code, Message: text[0], Hint: text[1]}
}

func NewReport(scope string) Report {
	return Report{Version: 1, Scope: scope, CheckedAt: time.Now().UnixMilli(), Checks: []Check{}}
}
func (r Report) HasFailures() bool {
	for _, c := range r.Checks {
		if c.State == Failed {
			return true
		}
	}
	return false
}

type Runtime interface {
	DiagnosticPing(context.Context) error
	DiagnosticImage(context.Context, string) error
}

// Offline describes checks that were intentionally not performed by check-config.
func Offline(valid bool) Report {
	r := NewReport("configuration")
	state, code := Passed, "config_ok"
	if !valid {
		state, code = Failed, "config_invalid"
	}
	r.Checks = append(r.Checks, Result("configuration", state, code))
	for _, id := range []string{"docker", "agent_image", "data_disk", "data_permissions", "container_ownership", "account_configuration"} {
		r.Checks = append(r.Checks, Result(id, NotChecked, "not_requested"))
	}
	return Finish(r)
}

func Finish(r Report) Report {
	r.Checks = append(r.Checks, Result("websocket", NotChecked, "websocket_not_checked"), Result("model", NotChecked, "model_not_checked"))
	return r
}

func RuntimeChecks(ctx context.Context, rt Runtime, image string) []Check {
	if rt == nil {
		return []Check{Result("docker", NotChecked, "not_requested"), Result("agent_image", NotChecked, "dependency_unavailable")}
	}
	if ctx.Err() != nil {
		return []Check{Result("docker", NotChecked, "check_cancelled"), Result("agent_image", NotChecked, "check_cancelled")}
	}
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := rt.DiagnosticPing(probe)
	cancel()
	if ctx.Err() != nil {
		return []Check{Result("docker", NotChecked, "check_cancelled"), Result("agent_image", NotChecked, "check_cancelled")}
	}
	if err != nil {
		return []Check{Result("docker", Failed, "docker_unavailable"), Result("agent_image", NotChecked, "dependency_unavailable")}
	}
	probe, cancel = context.WithTimeout(ctx, 3*time.Second)
	err = rt.DiagnosticImage(probe, image)
	cancel()
	if ctx.Err() != nil {
		return []Check{Result("docker", Passed, "docker_ok"), Result("agent_image", NotChecked, "check_cancelled")}
	}
	imageCheck := Result("agent_image", Passed, "image_ok")
	if err != nil {
		code := "image_unavailable"
		if errors.Is(err, fs.ErrNotExist) {
			code = "image_missing"
		}
		imageCheck = Result("agent_image", Failed, code)
	}
	return []Check{Result("docker", Passed, "docker_ok"), imageCheck}
}

func Disk(dir string, reserve int64) Check {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return Result("data_disk", Failed, "storage_unavailable")
	}
	free := stat.Bavail * uint64(stat.Bsize)
	if free == 0 || reserve > 0 && free < uint64(reserve) {
		return Result("data_disk", Failed, "storage_full")
	}
	return Result("data_disk", Passed, "disk_ok")
}

func storageCode(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return "permission_denied"
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return "storage_full"
	}
	return "storage_unavailable"
}

// ProbeWrite only touches its freshly created private directory under a pinned
// root. Never follow a container-created link or replace an existing file.
func ProbeWrite(ctx context.Context, root *safefs.Root) (checks []Check) {
	if ctx.Err() != nil {
		return []Check{Result("data_permissions", NotChecked, "check_cancelled"), Result("container_ownership", NotChecked, "check_cancelled")}
	}
	checks = []Check{Result("data_permissions", Passed, "write_ok"), Result("container_ownership", NotChecked, "ownership_not_checked")}
	name := ".agentbox-diagnostic-" + rand.Text()
	if err := root.Mkdir(name, 0700); err != nil {
		checks[0] = Result("data_permissions", Failed, storageCode(err))
		return
	}
	defer func() {
		if err := root.RemoveAll(name); err != nil {
			checks[0] = Result("data_permissions", Failed, "probe_cleanup_failed")
		}
	}()
	probe, err := root.Sub(name)
	if err != nil {
		checks[0] = Result("data_permissions", Failed, storageCode(err))
		return
	}
	defer probe.Close()
	if _, err = probe.WriteFile("probe", []byte("agentbox diagnostic\n"), safefs.WriteOptions{Mode: 0600}); err != nil {
		checks[0] = Result("data_permissions", Failed, storageCode(err))
		return
	}
	if runtime.GOOS == "linux" {
		checks[1] = Result("container_ownership", Passed, "ownership_ok")
		if err := probe.Chown("probe", 1000, 1000); err != nil {
			checks[1] = Result("container_ownership", Failed, storageCode(err))
		}
	}
	return
}

func Accounts(accounts []config.Account) Check {
	if len(accounts) == 0 {
		return Result("account_configuration", Failed, "accounts_missing")
	}
	for _, a := range accounts {
		if !credentials.ConfigurationPresent(a) {
			return Result("account_configuration", Failed, "credentials_missing")
		}
	}
	return Result("account_configuration", Passed, "accounts_ok")
}

// Instance performs explicit environment checks, without containers or models.
func Instance(ctx context.Context, cfg *config.Config, rt Runtime, writeProbe bool) Report {
	r := NewReport("instance")
	c := Result("configuration", Passed, "config_ok")
	if cfg.Check() != nil {
		c = Result("configuration", Failed, "config_invalid")
	}
	r.Checks = append(r.Checks, c)
	r.Checks = append(r.Checks, RuntimeChecks(ctx, rt, cfg.GetAgentImage())...)
	r.Checks = append(r.Checks, Disk(cfg.DataDir, cfg.GetResources().MinFreeBytes))
	if !writeProbe {
		r.Checks = append(r.Checks, Result("data_permissions", NotChecked, "not_requested"), Result("container_ownership", NotChecked, "not_requested"))
	} else {
		root, err := safefs.Open(cfg.DataDir)
		if err != nil {
			r.Checks = append(r.Checks, Result("data_permissions", Failed, storageCode(err)), Result("container_ownership", NotChecked, "dependency_unavailable"))
		} else {
			r.Checks = append(r.Checks, ProbeWrite(ctx, root)...)
			root.Close()
		}
	}
	r.Checks = append(r.Checks, Accounts(cfg.AccountList()))
	return Finish(r)
}

// WorkspacePermissions only inspects pinned root metadata. No project content,
// chmod, chown, directory creation or file writes occur in a session.
func WorkspacePermissions(root *safefs.Root, paths ...string) Check {
	for _, path := range paths {
		dir, err := root.Sub(path)
		if err != nil {
			return Result("workspace_permissions", Failed, storageCode(err))
		}
		info, err := dir.Lstat(".")
		dir.Close()
		if err != nil {
			return Result("workspace_permissions", Failed, storageCode(err))
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 1000 || info.Mode().Perm()&0700 != 0700 {
			return Result("workspace_permissions", Failed, "permission_denied")
		}
	}
	return Result("workspace_permissions", Passed, "workspace_permissions_ok")
}
