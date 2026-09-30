package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"time"

	"agentbox/internal/buildinfo"
)

// Upgrade orchestration lives outside agentbox.service so it survives our
// graceful shutdown. Only the installed, trusted helper can launch a worker;
// browsers supply a version, never a URL, command, or filesystem path.
type upgradeJob struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	FromVersion string `json:"from_version"`
	Phase       string `json:"phase"`
	Message     string `json:"message"`
	Error       string `json:"error"`
	StartedAt   int64  `json:"started_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type upgradeInfo struct {
	Supported      bool        `json:"supported"`
	Reason         string      `json:"reason"`
	CurrentVersion string      `json:"current_version"`
	Job            *upgradeJob `json:"job"`
	Error          string      `json:"error,omitempty"`
}

var upgradeVersionRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func upgradeHelper(executable, version string) (app, script string, err error) {
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", "", err
	}
	packageDir := filepath.Dir(executable)
	if filepath.Base(executable) != "agentbox" || filepath.Base(packageDir) != version || filepath.Base(filepath.Dir(packageDir)) != "releases" {
		return "", "", fmt.Errorf("not a versioned installation")
	}
	app = filepath.Dir(filepath.Dir(packageDir))
	current, err := filepath.EvalSymlinks(filepath.Join(app, "current", "agentbox"))
	if err != nil || current != executable {
		return "", "", fmt.Errorf("running executable is not current")
	}
	script = filepath.Join(packageDir, "deploy", "update.py")
	for _, name := range []string{script, filepath.Join(packageDir, "deploy", "release.py")} {
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			return "", "", fmt.Errorf("upgrade helper missing")
		}
	}
	return app, script, nil
}

func (s *Server) upgradeCommand(ctx context.Context, action, version string) (upgradeInfo, error) {
	info := upgradeInfo{CurrentVersion: buildinfo.Version}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		info.Reason = "一键升级需要以 root 运行的 Linux/systemd 版本目录安装；当前环境请手工升级。"
		return info, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return info, err
	}
	app, script, err := upgradeHelper(executable, buildinfo.Version)
	if err != nil {
		info.Reason = "当前部署不是支持在线升级的版本目录安装，请使用手工升级。"
		return info, nil
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		info.Reason = "服务器缺少 Python 3，请使用手工升级。"
		return info, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := []string{"-I", script, "--app", app, "--config", s.cfg.Path(), "--pid", strconv.Itoa(os.Getpid()), "--current", buildinfo.Version, "--revision", buildinfo.Commit(), action}
	if action == "start" {
		args = append(args, "--version", version)
	}
	cmd := exec.CommandContext(ctx, python, args...)
	cmd.WaitDelay = time.Second
	cmd.Stderr = os.Stderr // Details go to journald, never into a browser response.
	output, err := cmd.Output()
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(output, &info); err != nil {
		return info, err
	}
	info.CurrentVersion = buildinfo.Version
	if info.Error != "" {
		return info, fmt.Errorf("%s", info.Error)
	}
	return info, nil
}

func (s *Server) handleUpgradeStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	info, err := s.upgradeCommand(r.Context(), "status", "")
	if err != nil {
		log.Printf("upgrade status: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法读取升级任务，请稍后刷新。"})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleUpgradeStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request struct {
		Version string `json:"version"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	d.DisallowUnknownFields()
	if err := d.Decode(&request); err != nil || !upgradeVersionRE.MatchString(request.Version) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择有效的正式版本。"})
		return
	}
	if d.Decode(new(any)) != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的升级请求。"})
		return
	}
	// Pin the version the administrator actually reviewed. The worker verifies
	// that exact tag and its assets again; a newer release cannot silently win.
	s.updates.mu.Lock()
	latest := s.updates.snapshot()
	s.updates.mu.Unlock()
	if !latest.Available || latest.LatestVersion != request.Version || latest.Error != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "版本检查结果已变化，请重新检查更新后升级。"})
		return
	}
	info, err := s.upgradeCommand(r.Context(), "start", request.Version)
	if err != nil {
		log.Printf("upgrade start: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "升级提交结果尚未确认，请刷新任务状态并检查服务器日志。"})
		return
	}
	if !info.Supported {
		writeJSON(w, http.StatusConflict, map[string]string{"error": info.Reason})
		return
	}
	writeJSON(w, http.StatusAccepted, info)
}
