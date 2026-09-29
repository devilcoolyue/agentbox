// Package pricecatalog manages reviewed price data independently of releases.
// A catalog is a candidate; fetching it never changes the billing configuration.
package pricecatalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/safefs"
)

//go:embed catalog.json
var bundled []byte

const MaxBytes = 1 << 20

type Entry struct {
	Price      config.ModelPrice `json:"price"`
	SourceURL  string            `json:"source_url"`
	VerifiedAt string            `json:"verified_at,omitempty"`
	Notes      string            `json:"notes,omitempty"`
}

type Catalog struct {
	Schema      int              `json:"schema"`
	Version     string           `json:"version"`
	PublishedAt string           `json:"published_at"`
	Entries     map[string]Entry `json:"entries"`
	Source      string           `json:"source,omitempty"`
	Issues      []Issue          `json:"issues,omitempty"`
}

type Issue struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

func Parse(raw []byte, requireVerified bool) (Catalog, error) {
	var c Catalog
	if len(raw) > MaxBytes {
		return c, fmt.Errorf("价格目录超过 1 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("价格目录格式错误: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return c, fmt.Errorf("价格目录含多余数据")
	}
	if c.Schema != 1 || c.Version == "" || len(c.Version) > 100 || len(c.Entries) == 0 {
		return c, fmt.Errorf("价格目录版本或条目无效")
	}
	if _, err := time.Parse(time.RFC3339, c.PublishedAt); err != nil {
		return c, fmt.Errorf("published_at 必须是 RFC3339 时间")
	}
	prices := map[string]config.ModelPrice{}
	// Missing rates are unknown, not free. Every bucket must be explicitly
	// present, including zero for a free/not-applicable bucket.
	var fields struct {
		Entries map[string]struct {
			Price map[string]json.RawMessage `json:"price"`
		} `json:"entries"`
	}
	_ = json.Unmarshal(raw, &fields)
	checkBuckets := func(values map[string]json.RawMessage) bool {
		for _, k := range []string{"input", "output", "cache_read", "cache_write"} {
			if len(values[k]) == 0 || string(values[k]) == "null" {
				return false
			}
		}
		return true
	}
	for key, e := range c.Entries {
		values := fields.Entries[key].Price
		if !checkBuckets(values) {
			return c, fmt.Errorf("%s 必须显式提供四项单价，未知价格不能填成 0", key)
		}
		if e.Price.Long != nil {
			var long map[string]json.RawMessage
			_ = json.Unmarshal(values["long"], &long)
			if !checkBuckets(long) {
				return c, fmt.Errorf("%s 的长上下文档缺少单价", key)
			}
		}
		if key == "claude" || key == "codex" {
			return c, fmt.Errorf("目录只能包含具体模型，不能修改 Agent 兜底价")
		}
		if e.SourceURL == "" || config.ValidateCatalogURL(e.SourceURL) != nil {
			return c, fmt.Errorf("%s 缺少合法的来源链接", key)
		}
		if requireVerified || e.VerifiedAt != "" {
			if _, err := time.Parse(time.RFC3339, e.VerifiedAt); err != nil {
				return c, fmt.Errorf("%s 缺少有效的 verified_at 核验时间", key)
			}
		}
		if len(e.Notes) > 2000 {
			return c, fmt.Errorf("%s 的备注过长", key)
		}
		prices[key] = e.Price
	}
	if err := config.ValidatePrices(prices); err != nil {
		return c, err
	}
	return c, nil
}

func (c Catalog) Revision() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type Status struct {
	Catalog     Catalog `json:"catalog"`
	Revision    string  `json:"revision"`
	URL         string  `json:"url"`
	Bundled     bool    `json:"bundled"`
	CheckedAt   int64   `json:"checked_at"`
	AttemptedAt int64   `json:"attempted_at"`
	Error       string  `json:"error"`
}

type Service struct {
	mu       sync.Mutex
	status   Status
	cacheDir string
	client   *http.Client
}

func New(cacheDir string) *Service {
	c, err := Parse(bundled, false)
	if err != nil {
		panic(err)
	}
	s := &Service{cacheDir: cacheDir, client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return config.ValidateCatalogURL(req.URL.String())
	}}, status: Status{Catalog: c, Revision: c.Revision(), Bundled: true}}
	// The cache contains only public price data, and is safe to discard.
	if root, err := safefs.Open(cacheDir); err == nil {
		defer root.Close()
		if raw, err := root.ReadAll("pricing-catalog.json", MaxBytes*2); err == nil {
			var saved Status
			if json.Unmarshal(raw, &saved) == nil && saved.URL != "" && config.ValidateCatalogURL(saved.URL) == nil {
				data, _ := json.Marshal(saved.Catalog)
				thirdParty := saved.URL == ModelsDevURL && saved.Catalog.Source == "models.dev"
				if catalog, err := Parse(data, !thirdParty); err == nil {
					saved.Catalog = catalog
					saved.Revision = catalog.Revision()
					saved.Bundled = false
					s.status = saved
				}
			}
		}
	}
	return s
}

// Copy maps as well as tier pointers: callers may build a diff or test fixture.
func cloneStatus(s Status) Status {
	raw, _ := json.Marshal(s)
	var out Status
	_ = json.Unmarshal(raw, &out)
	return out
}

func (s *Service) selectSource(url string) {
	if s.status.URL == url {
		return
	}
	c, _ := Parse(bundled, false)
	s.status = Status{Catalog: c, Revision: c.Revision(), URL: url, Bundled: true}
}

func (s *Service) Snapshot(url string) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selectSource(url)
	return cloneStatus(s.status)
}

func (s *Service) Check(ctx context.Context, url string, manual bool) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selectSource(url)
	if url == "" {
		s.status.Error = "尚未配置远程价格目录，当前展示内置旧快照"
		return cloneStatus(s.status)
	}
	interval := 24 * time.Hour
	if manual {
		interval = time.Minute
	}
	if s.status.AttemptedAt != 0 && time.Since(time.UnixMilli(s.status.AttemptedAt)) < interval {
		return cloneStatus(s.status)
	}
	if ctx.Err() != nil {
		return cloneStatus(s.status)
	}
	s.status.AttemptedAt = time.Now().UnixMilli()
	c, err := s.fetch(ctx, url)
	if err != nil {
		s.status.Error = "获取或校验价格目录失败，已保留上次候选与生效价格；请检查地址和目录格式（手动检查间隔 1 分钟）"
		return cloneStatus(s.status)
	}
	next := Status{Catalog: c, Revision: c.Revision(), URL: url, CheckedAt: time.Now().UnixMilli(), AttemptedAt: s.status.AttemptedAt}
	if err := os.MkdirAll(s.cacheDir, 0o700); err != nil {
		s.status.Error = "无法保存价格目录缓存"
		return cloneStatus(s.status)
	}
	root, err := safefs.Open(s.cacheDir)
	if err != nil {
		s.status.Error = "无法打开价格目录缓存"
		return cloneStatus(s.status)
	}
	defer root.Close()
	raw, _ := json.Marshal(next)
	if _, err := root.WriteFile("pricing-catalog.json", raw, safefs.WriteOptions{Mode: 0o600}); err != nil {
		s.status.Error = "无法保存价格目录缓存"
		return cloneStatus(s.status)
	}
	s.status = next
	return cloneStatus(next)
}

func (s *Service) fetch(ctx context.Context, url string) (Catalog, error) {
	if err := config.ValidateCatalogURL(url); err != nil {
		return Catalog{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Catalog{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agentbox-price-catalog")
	res, err := s.client.Do(req)
	if err != nil {
		return Catalog{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Catalog{}, fmt.Errorf("catalog status %d", res.StatusCode)
	}
	limit := int64(MaxBytes)
	if url == ModelsDevURL {
		limit = ModelsDevMaxBytes
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return Catalog{}, err
	}
	if int64(len(raw)) > limit {
		return Catalog{}, fmt.Errorf("价格源超过大小限制")
	}
	if url == ModelsDevURL {
		return ParseModelsDev(raw, time.Now())
	}
	return Parse(raw, true)
}
