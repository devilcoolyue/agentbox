package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"agentbox/pkg/extspi"
)

const (
	webhookQueue    = 256
	webhookWorkers  = 2
	webhookTimeout  = 10 * time.Second
	webhookRetryGap = 2 * time.Second
)

// Webhook is the agentbox.webhook extension: it posts workspace lifecycle and
// chat turn events to an administrator's address. It is official code with
// network access; third-party extensions never get egress. Event only queues;
// workers deliver with one retry and report counts as metrics. The URL often
// carries a bot secret, so it never appears in logs.
type Webhook struct {
	extspi.BaseHandler
	host                          extspi.Host
	target                        string
	format, turns, workspaces, lc string
	client                        *http.Client

	queue  chan extspi.EventParams
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu                    sync.Mutex
	sent, failed, dropped float64
}

type webhookConfig struct {
	URL        string `json:"url"`
	Format     string `json:"format"`
	Turns      string `json:"turns"`
	Workspaces string `json:"workspaces"`
	Language   string `json:"language"`
}

func (w *Webhook) Initialize(_ context.Context, p extspi.InitializeParams, host extspi.Host) (extspi.InitializeResult, error) {
	cfg := webhookConfig{Format: "json", Turns: "failed", Workspaces: "all", Language: "zh-CN"}
	if len(p.Config) != 0 {
		if err := json.Unmarshal(p.Config, &cfg); err != nil {
			return extspi.InitializeResult{}, &extspi.Error{Code: extspi.CodeInvalidParams, Message: "invalid config"}
		}
	}
	w.host, w.format, w.turns, w.workspaces, w.lc = host, cfg.Format, cfg.Turns, cfg.Workspaces, cfg.Language
	if cfg.URL != "" {
		u, err := url.Parse(cfg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return extspi.InitializeResult{}, &extspi.Error{Code: extspi.CodeInvalidParams, Message: "the webhook URL must be an http or https address"}
		}
		w.target = cfg.URL
	}
	w.client = &http.Client{Timeout: webhookTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	w.queue = make(chan extspi.EventParams, webhookQueue)
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	for range webhookWorkers {
		w.wg.Add(1)
		go w.work(ctx)
	}
	var hooks []string
	for _, perm := range p.Permissions {
		switch perm {
		case extspi.PermWorkspaceEvents:
			hooks = append(hooks, extspi.HookWorkspaceLifecycle)
		case extspi.PermChatTurns:
			hooks = append(hooks, extspi.HookChatTurnEnded)
		}
	}
	if w.target == "" {
		host.Log(extspi.LogParams{Level: "warn", Message: "no webhook URL is set; events are not sent"})
		host.ReportStatus(extspi.StatusReport{State: "degraded"})
	} else {
		w.report()
	}
	return extspi.InitializeResult{SPIVersion: extspi.Version, Hooks: hooks}, nil
}

// wants applies the administrator's event filters.
func (w *Webhook) wants(p extspi.EventParams) bool {
	switch {
	case w.target == "":
		return false
	case p.Workspace != nil:
		return w.workspaces == "all"
	case p.Turn != nil:
		return w.turns == "all" || (w.turns == "failed" && p.Turn.Status != extspi.TurnCompleted)
	}
	return false
}

func (w *Webhook) Event(p extspi.EventParams) {
	if !w.wants(p) {
		return
	}
	select {
	case w.queue <- p:
	default:
		w.count(&w.dropped)
	}
}

func (w *Webhook) work(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-w.queue:
			body, err := w.body(p)
			if err == nil {
				err = w.post(ctx, body)
				if retryable(err) {
					select {
					case <-ctx.Done():
						return
					case <-time.After(webhookRetryGap):
					}
					err = w.post(ctx, body)
				}
			}
			if err != nil {
				w.count(&w.failed)
				w.host.Log(extspi.LogParams{Level: "warn", Message: "delivering a " + p.Hook + " event failed: " + err.Error()})
			} else {
				w.count(&w.sent)
			}
		}
	}
}

// deliveryError is a failed delivery; its text never contains the URL.
type deliveryError struct {
	msg   string
	retry bool
}

func (e *deliveryError) Error() string { return e.msg }

func retryable(err error) bool {
	var d *deliveryError
	return errors.As(err, &d) && d.retry
}

func (w *Webhook) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.target, bytes.NewReader(body))
	if err != nil {
		return &deliveryError{msg: "invalid request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agentbox-webhook/1")
	resp, err := w.client.Do(req)
	if err != nil {
		// *url.Error repeats the URL; keep only the cause.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return &deliveryError{msg: "connection failed: " + err.Error(), retry: ctx.Err() == nil}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &deliveryError{msg: fmt.Sprintf("HTTP %d", resp.StatusCode), retry: resp.StatusCode == 429 || resp.StatusCode >= 500}
	}
	// Bot APIs answer 200 with their own error code.
	var reply struct {
		Code    *int `json:"code"`
		ErrCode *int `json:"errcode"`
	}
	if json.Unmarshal(raw, &reply) == nil {
		if reply.Code != nil && *reply.Code != 0 {
			return &deliveryError{msg: fmt.Sprintf("rejected with code %d", *reply.Code)}
		}
		if reply.ErrCode != nil && *reply.ErrCode != 0 {
			return &deliveryError{msg: fmt.Sprintf("rejected with errcode %d", *reply.ErrCode)}
		}
	}
	return nil
}

func (w *Webhook) body(p extspi.EventParams) ([]byte, error) {
	text := webhookText(w.lc, p)
	switch w.format {
	case "feishu":
		return json.Marshal(map[string]any{"msg_type": "text", "content": map[string]string{"text": text}})
	case "dingtalk", "wecom":
		return json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": text}})
	}
	return json.Marshal(struct {
		extspi.EventParams
		Text string `json:"text"`
	}{p, text})
}

func (w *Webhook) count(n *float64) {
	w.mu.Lock()
	*n++
	w.mu.Unlock()
	w.report()
}

func (w *Webhook) report() {
	w.mu.Lock()
	metrics := map[string]float64{"sent": w.sent, "failed": w.failed, "dropped": w.dropped}
	w.mu.Unlock()
	w.host.ReportStatus(extspi.StatusReport{State: "ok", Metrics: metrics})
}

// Shutdown stops the workers; events still queued are not sent.
func (w *Webhook) Shutdown(ctx context.Context) error {
	if w.cancel == nil {
		return nil
	}
	w.cancel()
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var webhookWords = map[string]map[string]string{
	"zh-CN": {
		"workspace": "%s 的空间「%s」%s", extspi.WorkspaceCreated: "已创建", extspi.WorkspaceStarted: "已启动",
		extspi.WorkspaceStopped: "已停止", "idle": "已停止（空闲休眠）", extspi.WorkspaceDeleted: "已删除",
		"turn": "%s 的空间「%s」对话%s", extspi.TurnCompleted: "已完成", extspi.TurnFailed: "失败", extspi.TurnInterrupted: "已中断",
		"model": "模型", "duration": "耗时", "cost": "费用",
	},
	"zh-TW": {
		"workspace": "%s 的空間「%s」%s", extspi.WorkspaceCreated: "已建立", extspi.WorkspaceStarted: "已啟動",
		extspi.WorkspaceStopped: "已停止", "idle": "已停止（閒置休眠）", extspi.WorkspaceDeleted: "已刪除",
		"turn": "%s 的空間「%s」對話%s", extspi.TurnCompleted: "已完成", extspi.TurnFailed: "失敗", extspi.TurnInterrupted: "已中斷",
		"model": "模型", "duration": "耗時", "cost": "費用",
	},
	"en": {
		"workspace": "%s's workspace \"%s\" %s", extspi.WorkspaceCreated: "was created", extspi.WorkspaceStarted: "started",
		extspi.WorkspaceStopped: "stopped", "idle": "stopped (idle)", extspi.WorkspaceDeleted: "was deleted",
		"turn": "%s's workspace \"%s\": chat turn %s", extspi.TurnCompleted: "completed", extspi.TurnFailed: "failed", extspi.TurnInterrupted: "interrupted",
		"model": "Model", "duration": "Duration", "cost": "Cost",
	},
}

// webhookText is the one-line summary bots show and JSON receivers get as
// text.
func webhookText(lc string, p extspi.EventParams) string {
	words, ok := webhookWords[lc]
	if !ok {
		words = webhookWords["zh-CN"]
	}
	name := func(ref extspi.WorkspaceRef) string {
		if ref.Name != "" {
			return ref.Name
		}
		return ref.ID
	}
	switch {
	case p.Workspace != nil:
		e := p.Workspace
		action := words[e.Action]
		if e.Action == extspi.WorkspaceStopped && e.Reason == "idle" {
			action = words["idle"]
		}
		return "agentbox · " + fmt.Sprintf(words["workspace"], e.Workspace.User, name(e.Workspace), action)
	case p.Turn != nil:
		e := p.Turn
		status := words[e.Status]
		if e.Status == extspi.TurnFailed && e.Code != "" {
			status += " (" + e.Code + ")"
		}
		line := "agentbox · " + fmt.Sprintf(words["turn"], e.Workspace.User, name(e.Workspace), status)
		var details []string
		if e.Model != "" {
			details = append(details, words["model"]+" "+e.Model)
		}
		details = append(details, words["duration"]+" "+shortDuration(time.Duration(e.DurationMS)*time.Millisecond))
		if e.CostMicroUSD > 0 {
			details = append(details, words["cost"]+" "+usd(e.CostMicroUSD))
		}
		out := line + "\n"
		for i, d := range details {
			if i > 0 {
				out += " · "
			}
			out += d
		}
		return out
	}
	return "agentbox · " + p.Hook
}

func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func usd(micro int64) string {
	if micro >= 10_000 {
		return fmt.Sprintf("$%.2f", float64(micro)/1e6)
	}
	return fmt.Sprintf("$%.4f", float64(micro)/1e6)
}
