package linkapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"agentbox/internal/buildinfo"
)

// DefaultAddr is where the panel listens. Loopback only — the console can
// start and stop a tunnel into the user's intranet, so it must never be
// reachable from the network.
const DefaultAddr = "127.0.0.1:7801"

// RunOptions configures a panel run.
type RunOptions struct {
	Addr string
	// Daemon marks a run started by the OS at boot/login: no browser is opened
	// and the saved config connects on its own.
	Daemon bool
}

// RunPanel starts the control panel and blocks until the process is signalled.
//
// If the same version already holds the address, this one hands over to it —
// double-clicking the app while the autostart service is running should surface
// the existing console, not fail with a port conflict. A different version must
// not hand over: its console would look like the new one failed to take effect.
func RunPanel(opts RunOptions) error {
	if opts.Addr == "" {
		opts.Addr = DefaultAddr
	}

	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		if version, running := runningPanel(opts.Addr); running {
			url := "http://" + opts.Addr
			if opts.Daemon {
				fmt.Printf("abox-link 已经在运行，控制台：%s\n", url)
				return nil
			}
			if version != buildinfo.Version {
				// No browser: it would cover this window with the other version's console.
				fmt.Print(replaceNotice(url, version, buildinfo.Version, AutostartStatus().Installed, runtime.GOOS))
				waitForEnter()
				return errors.New("未启动：另一个版本正在运行 / not started: another version is running")
			}
			fmt.Printf("abox-link 已经在运行，控制台：%s\n", url)
			OpenBrowser(url)
			return nil
		}
		return fmt.Errorf("无法监听 %s：%w", opts.Addr, err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	sup := NewSupervisor()
	panel := NewPanel(cfg, sup)

	srv := &http.Server{Handler: panel.Handler()}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "abox-link: 控制台异常退出:", err)
		}
	}()

	url := "http://" + opts.Addr
	fmt.Printf("abox-link 控制台：%s\n", url)
	if !cfg.Paired() {
		sup.Logf("欢迎使用 abox-link。请在 agentbox 里生成配对码，粘贴到上面完成接入。")
	}

	// The saved config decides whether we come up connected; the service install
	// relies on this to restore the tunnel after a reboot without any clicking.
	if cfg.Paired() && cfg.AutoConnect {
		if err := sup.Start(cfg); err != nil {
			sup.Logf("自动连接未能启动：%v", err)
		}
	} else if cfg.Paired() {
		sup.Logf("已接入 %s。点「启动」连接隧道。", cfg.Server)
	}

	if !opts.Daemon {
		OpenBrowser(url)
	}

	// Wait for Ctrl-C / systemd stop, then take the tunnel down cleanly so the
	// server drops the link instead of waiting for a keepalive timeout.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\nabox-link: 正在退出…")

	sup.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// runningPanel reports whether the address is held by another abox-link panel
// (as opposed to some unrelated program that happens to own the port), and its
// version. Panels older than the version field report "".
func runningPanel(addr string) (version string, running bool) {
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/state", nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("X-Abox-Panel", "1")
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	// A long allow list makes this larger than it looks; a cut-off body would
	// read as "not ours" and turn a running panel into a port conflict.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var probe struct {
		Paired  *bool  `json:"paired"` // a field only our panel serves
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.Paired == nil {
		return "", false
	}
	return probe.Version, true
}

// replaceNotice tells someone who just opened a different version how to switch
// to it. Terminal output has no language setting, so it carries both languages.
func replaceNotice(url, running, self string, autostart bool, goos string) string {
	runningZH, runningEN := running, running
	if running == "" {
		runningZH, runningEN = "旧版", "an older version"
	}
	stopZH, stopEN := "pkill -f abox-link", "pkill -f abox-link"
	if goos == "windows" {
		stopZH, stopEN = "在任务管理器里结束 abox-link", "end abox-link in Task Manager"
	}
	var zh, en []string
	if autostart {
		zh = append(zh, "打开控制台 "+url+"，取消勾选「开机自启」，否则下次开机还会启动旧版")
		en = append(en, "Open the console at "+url+" and untick autostart, or the running version comes back at the next login")
	}
	zh = append(zh, "结束正在运行的版本："+stopZH, "重新运行这个文件")
	en = append(en, "Stop the running version: "+stopEN, "Run this file again")
	if autostart {
		zh = append(zh, "在新控制台里重新勾选「开机自启」")
		en = append(en, "Tick autostart again in the new console")
	}
	numbered := func(steps []string) string {
		var b strings.Builder
		for i, step := range steps {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, step)
		}
		return b.String()
	}
	return fmt.Sprintf("\n这个文件（%s）没有启动：另一个 abox-link（%s）正在运行，占用了 %s。"+
		"\n放行规则和配对信息都会保留。换成这个文件：\n%s"+
		"\nThis file (%s) did not start: another abox-link (%s) is running on %s."+
		"\nYour allow rules and pairing are kept. To switch to this file:\n%s",
		self, runningZH, url, numbered(zh), self, runningEN, url, numbered(en))
}

// waitForEnter keeps a double-clicked console window open long enough to read.
func waitForEnter() {
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return
	}
	fmt.Print("\n按回车键关闭 / Press Enter to close…")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
