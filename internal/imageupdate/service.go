// Package imageupdate updates CLI packages on top of the configured image.
// It does not restart containers or depend on a checkout, shell, or systemd.
package imageupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentbox/internal/config"
)

type Image struct {
	ID      string `json:"id"`
	Claude  string `json:"claude"`
	Codex   string `json:"codex"`
	Browser bool   `json:"browser"`
	User    string `json:"-"`
}
type Versions struct {
	Claude string `json:"claude"`
	Codex  string `json:"codex"`
}
type Backend interface {
	InspectCLIImage(context.Context, string) (Image, error)
	BuildCLIImage(context.Context, Image, Versions, string, func(string)) error
}
type Status struct {
	Running       bool     `json:"running"`
	Action        string   `json:"action"`
	Phase         string   `json:"phase"`
	StartedAt     int64    `json:"started_at"`
	FinishedAt    int64    `json:"finished_at"`
	CheckedAt     int64    `json:"checked_at"`
	BaseImage     string   `json:"base_image"`
	Current       Image    `json:"current"`
	Target        Versions `json:"target"`
	Available     bool     `json:"available"`
	ResultImage   string   `json:"result_image"`
	Error         string   `json:"error"`
	Log           string   `json:"log"`
	LastScheduled string   `json:"last_scheduled"`
}
type Service struct {
	mu       sync.Mutex
	cfg      *config.Config
	backend  Backend
	path     string
	status   Status
	client   *http.Client
	registry string
}

func New(cfg *config.Config, backend Backend) *Service {
	s := &Service{cfg: cfg, backend: backend, path: filepath.Join(cfg.DataDir, "image-update-state.json"), client: &http.Client{Timeout: 30 * time.Second}, registry: "https://registry.npmjs.org/"}
	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &s.status)
	}
	if s.status.Running {
		s.status.Running = false
		s.status.Phase = "failed"
		s.status.Error = "服务重启中断了上次任务，请重新检查"
	}
	return s
}
func (s *Service) Snapshot() Status { s.mu.Lock(); defer s.mu.Unlock(); return s.status }
func (s *Service) persistLocked() error {
	raw, err := json.Marshal(s.status)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".image-update-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}
func (s *Service) log(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Log += strings.ToValidUTF8(line, "")
	const limit = 16000
	if len(s.status.Log) > limit {
		s.status.Log = strings.ToValidUTF8(s.status.Log[len(s.status.Log)-limit:], "")
	}
}

// Due uses the site's timezone, runs at most once per local day, and catches up
// after downtime. A failed scheduled run is retried the next day or manually.
func Due(now time.Time, clock, last string) bool {
	return now.Format("15:04") >= clock && now.Format("2006-01-02") != last
}

// Prepare reserves the single job slot before the caller starts its lifecycle-
// tracked goroutine. The returned function must be called even on shutdown.
func (s *Service) Prepare(action string, scheduled bool) (func(context.Context), error) {
	if action != "check" && action != "update" && action != "rollback" {
		return nil, fmt.Errorf("未知更新操作")
	}
	policy, base, previous := s.cfg.ImageUpdateState()
	now := time.Now().In(s.cfg.GetLocation())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Running {
		return nil, fmt.Errorf("已有客户端更新任务正在执行")
	}
	if scheduled && (!policy.Enabled || !Due(now, policy.Time, s.status.LastScheduled)) {
		return nil, nil
	}
	last := s.status.LastScheduled
	if scheduled {
		last = now.Format("2006-01-02")
	}
	s.status = Status{Running: true, Action: action, Phase: "checking", StartedAt: now.UnixMilli(), BaseImage: base, LastScheduled: last}
	if err := s.persistLocked(); err != nil {
		s.status.Running = false
		s.status.Phase = "failed"
		s.status.Error = "保存更新任务失败: " + err.Error()
		return nil, fmt.Errorf("保存更新任务失败: %w", err)
	}
	return func(parent context.Context) {
		ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
		defer cancel()
		err := s.run(ctx, action, policy, base, previous)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.status.Running = false
		s.status.FinishedAt = time.Now().UnixMilli()
		if err != nil {
			s.status.Phase = "failed"
			s.status.Error = err.Error()
		} else {
			s.status.Phase = "done"
		}
		if err := s.persistLocked(); err != nil {
			s.status.Error += " 保存状态失败: " + err.Error()
		}
	}, nil
}

var versionRE = regexp.MustCompile(`^[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}$`)

func ValidVersion(v string) bool { return versionRE.MatchString(v) }
func newer(a, b string) bool {
	if !ValidVersion(a) || !ValidVersion(b) {
		return false
	}
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range aa {
		x, e1 := strconv.ParseUint(aa[i], 10, 64)
		y, e2 := strconv.ParseUint(bb[i], 10, 64)
		if e1 != nil || e2 != nil {
			return false
		}
		if x != y {
			return x > y
		}
	}
	return false
}
func (s *Service) version(ctx context.Context, pkg, channel string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.registry+pkg+"/"+channel, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("查询 %s 失败: %w", pkg, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("查询 %s/%s: HTTP %d", pkg, channel, resp.StatusCode)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&v); err != nil {
		return "", err
	}
	if !ValidVersion(v.Version) {
		return "", fmt.Errorf("npm 返回了不支持的版本号")
	}
	return v.Version, nil
}
func (s *Service) run(ctx context.Context, action string, policy config.ImageUpdateConfig, base, previous string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	img, err := s.backend.InspectCLIImage(ctx, base)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.status.Current = img
	s.mu.Unlock()
	s.log(fmt.Sprintf("当前镜像 %s\nClaude %s / Codex %s\n", base, img.Claude, img.Codex))
	if action == "rollback" {
		if previous == "" {
			return errors.New("没有可回退的镜像")
		}
		old, err := s.backend.InspectCLIImage(ctx, previous)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.cfg.SwitchAgentImage(base, policy, old.ID, img.ID, true); err != nil {
			return err
		}
		s.mu.Lock()
		s.status.ResultImage = old.ID
		s.mu.Unlock()
		s.log("已回退，自动更新已暂停；停止再启动空间后生效。\n")
		return nil
	}
	if !ValidVersion(img.Claude) || !ValidVersion(img.Codex) {
		return errors.New("当前镜像缺少有效的 Claude/Codex 版本标签，请先使用带版本标签的 Agent 镜像")
	}
	claude, err := s.version(ctx, "@anthropic-ai/claude-code", policy.Channel)
	if err != nil {
		return err
	}
	target := Versions{Claude: img.Claude, Codex: img.Codex}
	if newer(claude, img.Claude) {
		target.Claude = claude
	}
	s.log(fmt.Sprintf("Claude %s 渠道：%s（自动更新不降级）\n", policy.Channel, claude))
	if policy.UpdateCodex {
		codex, err := s.version(ctx, "@openai/codex", "latest")
		if err != nil {
			return err
		}
		if newer(codex, img.Codex) {
			target.Codex = codex
		}
		s.log("Codex latest 渠道：" + codex + "\n")
	}
	available := target.Claude != img.Claude || target.Codex != img.Codex
	s.mu.Lock()
	s.status.Target = target
	s.status.Available = available
	s.status.CheckedAt = time.Now().UnixMilli()
	s.mu.Unlock()
	if !available {
		s.log("当前版本已满足所选渠道。\n")
		return nil
	}
	if action == "check" {
		s.log("发现更新，点击“立即更新”构建并应用。\n")
		return nil
	}
	// Unique tags keep both old and unsuccessful candidate images available to
	// operators. FROM uses the immutable image ID, retaining browser/custom fixes.
	tag := fmt.Sprintf("agentbox-agent:cli-%d", time.Now().UnixNano())
	s.mu.Lock()
	s.status.Phase = "building"
	s.mu.Unlock()
	if err := s.backend.BuildCLIImage(ctx, img, target, tag, s.log); err != nil {
		return err
	}
	built, err := s.backend.InspectCLIImage(ctx, tag)
	if err != nil {
		return err
	}
	if built.Claude != target.Claude || built.Codex != target.Codex || built.Browser != img.Browser {
		return errors.New("新镜像验证失败，未切换")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.cfg.SwitchAgentImage(base, policy, tag, img.ID, false); err != nil {
		return err
	}
	s.mu.Lock()
	s.status.ResultImage = tag
	s.status.Available = false
	s.mu.Unlock()
	s.log("新镜像验证通过并已切换。运行中的空间保持不变，停止再启动后生效。\n")
	return nil
}
