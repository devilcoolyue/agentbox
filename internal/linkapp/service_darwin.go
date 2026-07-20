package linkapp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// macOS autostart is a LaunchAgent in the user's own library: it starts at
// login, runs as the user, and needs no admin rights.

const launchLabel = "com.agentbox.abox-link"

func plistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
}

func AutostartStatus() ServiceStatus {
	st := ServiceStatus{Supported: true}
	if _, err := os.Stat(plistPath()); err == nil {
		st.Installed = true
		st.Detail = "已注册为登录项（LaunchAgent），开机登录后自动启动"
	}
	return st
}

func InstallAutostart() error {
	exe, err := exePath()
	if err != nil {
		return err
	}
	logPath := filepath.Join(ConfigDir(), "abox-link.log")
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	var env string
	if home := configHomeOverride(); home != "" {
		env = fmt.Sprintf(
			"  <key>EnvironmentVariables</key>\n  <dict><key>ABOX_LINK_HOME</key><string>%s</string></dict>\n", home)
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>--daemon</string>
  </array>
%s  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchLabel, exe, env, logPath, logPath)
	if err := writeServiceFile(plistPath(), plist, 0o644); err != nil {
		return err
	}
	// bootstrap is the modern verb; fall back to load -w on older systems.
	target := "gui/" + strconv.Itoa(os.Getuid())
	if err := run("launchctl", "bootstrap", target, plistPath()); err != nil {
		if lerr := run("launchctl", "load", "-w", plistPath()); lerr != nil {
			return fmt.Errorf("注册登录项失败：%w", err)
		}
	}
	return nil
}

func UninstallAutostart() error {
	target := "gui/" + strconv.Itoa(os.Getuid())
	if err := exec.Command("launchctl", "bootout", target+"/"+launchLabel).Run(); err != nil {
		_ = exec.Command("launchctl", "unload", "-w", plistPath()).Run()
	}
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
