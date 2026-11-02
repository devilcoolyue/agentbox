package linkapp

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/buildinfo"
)

// serve answers /api/state on a loopback port and returns its host:port.
func serve(t *testing.T, state map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(state)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestRunningPanel(t *testing.T) {
	if v, ok := runningPanel(serve(t, map[string]any{"paired": true, "version": "v9"})); !ok || v != "v9" {
		t.Errorf("current panel = %q, %v", v, ok)
	}
	if v, ok := runningPanel(serve(t, map[string]any{"paired": false})); !ok || v != "" {
		t.Errorf("panel from before the version field = %q, %v", v, ok)
	}
	if _, ok := runningPanel(serve(t, map[string]any{"name": "something else"})); ok {
		t.Error("an unrelated service must not count as a panel")
	}
	// A long allow list must not make a running panel look like a port conflict.
	long := map[string]any{"paired": true, "version": "v9", "allow": strings.Split(strings.Repeat("10.0.0.0/8 ", 2000), " ")}
	if _, ok := runningPanel(serve(t, long)); !ok {
		t.Error("a large state must still be recognised")
	}
}

// hold keeps a port busy the way a running panel does, so RunPanel cannot bind it.
func hold(t *testing.T, state map[string]any) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(state)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// A different version must stop and explain instead of handing over: its console
// would make the new file look like it had no effect.
func TestRunPanelRefusesToHandOverToAnotherVersion(t *testing.T) {
	addr := hold(t, map[string]any{"paired": true})
	err := RunPanel(RunOptions{Addr: addr})
	if err == nil || !strings.Contains(err.Error(), "another version") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunPanelDaemonStaysQuietAboutVersions(t *testing.T) {
	addr := hold(t, map[string]any{"paired": true})
	if err := RunPanel(RunOptions{Addr: addr, Daemon: true}); err != nil {
		t.Fatalf("the OS service retries on its own; err = %v", err)
	}
	addr = hold(t, map[string]any{"paired": true, "version": buildinfo.Version})
	if err := RunPanel(RunOptions{Addr: addr, Daemon: true}); err != nil {
		t.Fatalf("same version: err = %v", err)
	}
}

func TestReplaceNotice(t *testing.T) {
	const url = "http://127.0.0.1:7801"
	withAutostart := replaceNotice(url, "", "v2", true, "darwin")
	for _, want := range []string{"旧版", "an older version", "v2", url, "取消勾选「开机自启」", "pkill -f abox-link", "重新勾选", "untick autostart", "放行规则和配对信息都会保留"} {
		if !strings.Contains(withAutostart, want) {
			t.Errorf("autostart notice lacks %q:\n%s", want, withAutostart)
		}
	}
	// Unticking must come before stopping: a service that is still registered restarts.
	if strings.Index(withAutostart, "取消勾选") > strings.Index(withAutostart, "pkill") {
		t.Errorf("steps out of order:\n%s", withAutostart)
	}

	plain := replaceNotice(url, "v1", "v2", false, "linux")
	if !strings.Contains(plain, "v1") || strings.Contains(plain, "旧版") {
		t.Errorf("a known version should be named:\n%s", plain)
	}
	if strings.Contains(plain, "开机自启") || strings.Contains(plain, "autostart") {
		t.Errorf("no autostart steps when it is not registered:\n%s", plain)
	}

	windows := replaceNotice(url, "v1", "v2", false, "windows")
	if strings.Contains(windows, "pkill") || !strings.Contains(windows, "任务管理器") || !strings.Contains(windows, "Task Manager") {
		t.Errorf("windows has no pkill:\n%s", windows)
	}
}

func TestStateReportsVersion(t *testing.T) {
	p := NewPanel(Config{}, NewSupervisor())
	if got := p.state()["version"]; got != buildinfo.Version {
		t.Errorf("version = %v, want %q", got, buildinfo.Version)
	}
}
