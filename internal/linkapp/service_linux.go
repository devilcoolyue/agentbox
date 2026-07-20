package linkapp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Linux autostart is a systemd *user* unit: no root needed, and it inherits
// the user's home so the config file is the same one the panel edits.

func unitPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "systemd", "user", ServiceName+".service")
}

func haveSystemd() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	// A user manager only exists on a real login session; in containers and on
	// SysV boxes systemctl is present but --user fails.
	return exec.Command("systemctl", "--user", "is-system-running").Run() == nil ||
		os.Getenv("XDG_RUNTIME_DIR") != ""
}

func AutostartStatus() ServiceStatus {
	if !haveSystemd() {
		return ServiceStatus{Detail: "此系统没有可用的 systemd 用户服务，无法设置开机自启"}
	}
	st := ServiceStatus{Supported: true}
	if _, err := os.Stat(unitPath()); err == nil {
		st.Installed = true
		st.Detail = "已注册为 systemd 用户服务，登录后自动启动"
		// Without lingering the unit only runs while the user is logged in —
		// surprising on a headless box, so say so instead of silently failing.
		if out, err := exec.Command("loginctl", "show-user", os.Getenv("USER"), "-p", "Linger").Output(); err == nil &&
			strings.Contains(string(out), "Linger=no") {
			st.Detail += "（注意：未开启 linger，注销后会停止。执行 sudo loginctl enable-linger $USER 可常驻）"
		}
	}
	return st
}

func InstallAutostart() error {
	if !haveSystemd() {
		return fmt.Errorf("此系统没有可用的 systemd 用户服务")
	}
	exe, err := exePath()
	if err != nil {
		return err
	}
	var env string
	if home := configHomeOverride(); home != "" {
		env = fmt.Sprintf("Environment=ABOX_LINK_HOME=%s\n", home)
	}
	unit := fmt.Sprintf(`[Unit]
Description=abox-link 内网隧道
After=network-online.target

[Service]
ExecStart=%s --daemon
%sRestart=always
RestartSec=5

[Install]
WantedBy=default.target
`, exe, env)
	if err := writeServiceFile(unitPath(), unit, 0o644); err != nil {
		return err
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return run("systemctl", "--user", "enable", ServiceName+".service")
}

func UninstallAutostart() error {
	// Best-effort disable: the unit file is the source of truth for "installed",
	// so removing it must succeed even if systemctl complains.
	_ = run("systemctl", "--user", "disable", ServiceName+".service")
	if err := os.Remove(unitPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return run("systemctl", "--user", "daemon-reload")
}
