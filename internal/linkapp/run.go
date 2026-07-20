package linkapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
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
// If another instance already holds the address, this one hands over to it —
// double-clicking the app while the autostart service is running should surface
// the existing console, not fail with a port conflict.
func RunPanel(opts RunOptions) error {
	if opts.Addr == "" {
		opts.Addr = DefaultAddr
	}

	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		if alreadyRunning(opts.Addr) {
			url := "http://" + opts.Addr
			fmt.Printf("abox-link 已经在运行，控制台：%s\n", url)
			if !opts.Daemon {
				OpenBrowser(url)
			}
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

// alreadyRunning reports whether the address is held by another abox-link
// panel (as opposed to some unrelated program that happens to own the port).
func alreadyRunning(addr string) bool {
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/state", nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Abox-Panel", "1")
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var probe map[string]any
	if json.Unmarshal(raw, &probe) != nil {
		return false
	}
	_, ok := probe["paired"] // a field only our panel serves
	return ok
}
