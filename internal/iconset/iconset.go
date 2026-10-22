// Package iconset 保存管理员的全站图标配置：界面位置 ID → 图标库引用。
//
// 位置注册表和默认图标在前端（web/src/icon-slots.ts），这里只认引用格式并核对图标
// 确实存在于随二进制分发的图标库（static/vendor/icons/*.json）。浏览器只收到图标
// 名字和从这些文件里取出的形状，管理员无法提交任意 SVG 标记。
package iconset

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"

	"agentbox/internal/safefs"
)

const (
	file         = "icons.json"
	MaxOverrides = 500
	maxFileBytes = 256 << 10
)

// Sets 是可选的图标库前缀；顺序即图标选择器里的顺序。
var Sets = []string{"tabler", "tabler-filled", "lucide"}

var (
	slotPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9-]+)*$`)
	refPattern  = regexp.MustCompile(`^(tabler|tabler-filled|lucide):[a-z0-9-]+$`)
)

// Config 是存盘与下发的配置。
type Config struct {
	Version   int               `json:"version"`
	Overrides map[string]string `json:"overrides"`
	UpdatedAt int64             `json:"updated_at,omitempty"`
	UpdatedBy string            `json:"updated_by,omitempty"`
}

// Library 按需读取图标库文件，只留下形状（json.RawMessage，原样转发给浏览器）。
type Library struct {
	fsys  fs.FS
	once  sync.Once
	icons map[string]json.RawMessage
	err   error
}

func NewLibrary(fsys fs.FS) *Library { return &Library{fsys: fsys} }

func (l *Library) load() {
	l.icons = map[string]json.RawMessage{}
	for _, prefix := range Sets {
		raw, err := fs.ReadFile(l.fsys, "vendor/icons/"+prefix+".json")
		if err != nil {
			l.err = fmt.Errorf("read icon set %s: %w", prefix, err)
			return
		}
		var doc struct {
			Icons map[string]json.RawMessage `json:"icons"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			l.err = fmt.Errorf("parse icon set %s: %w", prefix, err)
			return
		}
		for name, nodes := range doc.Icons {
			l.icons[prefix+":"+name] = nodes
		}
	}
}

// Shape 返回图标形状；图标库读不出来时返回错误，图标不存在时 ok=false。
func (l *Library) Shape(ref string) (json.RawMessage, bool, error) {
	l.once.Do(l.load)
	if l.err != nil {
		return nil, false, l.err
	}
	nodes, ok := l.icons[ref]
	return nodes, ok, nil
}

// ValidationError 是给管理员看的配置错误。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Validate 核对位置 ID 与图标引用格式，以及图标在库里存在。
func (l *Library) Validate(overrides map[string]string) error {
	if len(overrides) > MaxOverrides {
		return &ValidationError{fmt.Sprintf("最多只能覆盖 %d 个位置", MaxOverrides)}
	}
	slots := make([]string, 0, len(overrides))
	for slot := range overrides {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	for _, slot := range slots {
		ref := overrides[slot]
		if len(slot) > 64 || !slotPattern.MatchString(slot) {
			return &ValidationError{fmt.Sprintf("位置 ID 无效：%q", slot)}
		}
		if !refPattern.MatchString(ref) {
			return &ValidationError{fmt.Sprintf("图标引用无效：%q", ref)}
		}
		_, ok, err := l.Shape(ref)
		if err != nil {
			return err
		}
		if !ok {
			return &ValidationError{fmt.Sprintf("图标库里没有 %s", ref)}
		}
	}
	return nil
}

// Shapes 取出配置里用到的图标形状，供浏览器直接绘制；库里找不到的跳过。
func (l *Library) Shapes(cfg Config) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for _, ref := range cfg.Overrides {
		nodes, ok, err := l.Shape(ref)
		if err != nil {
			return nil, err
		}
		if ok {
			out[ref] = nodes
		}
	}
	return out, nil
}

// Service 把配置存在 <data>/icons.json（容器挂载之外，进入系统备份）。
type Service struct {
	data string
	lib  *Library
	mu   sync.Mutex
}

func New(data string, lib *Library) *Service { return &Service{data: data, lib: lib} }

func (s *Service) Library() *Library { return s.lib }

// Get 读取配置；文件不存在时返回空配置。磁盘上被手工改坏的条目丢弃，不下发。
func (s *Service) Get() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

func (s *Service) read() (Config, error) {
	cfg := Config{Version: 1, Overrides: map[string]string{}}
	root, err := safefs.Open(s.data)
	if err != nil {
		return cfg, err
	}
	defer root.Close()
	raw, err := root.ReadAll(file, maxFileBytes)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, errors.New("无法读取图标配置")
	}
	if json.Unmarshal(raw, &cfg) != nil || cfg.Version != 1 {
		return Config{Version: 1, Overrides: map[string]string{}}, errors.New("图标配置文件版本或内容无效")
	}
	if cfg.Overrides == nil {
		cfg.Overrides = map[string]string{}
	}
	for slot, ref := range cfg.Overrides {
		if s.lib.Validate(map[string]string{slot: ref}) != nil {
			delete(cfg.Overrides, slot)
		}
	}
	return cfg, nil
}

// Put 整体替换覆盖表。
func (s *Service) Put(overrides map[string]string, by string) (Config, error) {
	if overrides == nil {
		overrides = map[string]string{}
	}
	if err := s.lib.Validate(overrides); err != nil {
		return Config{}, err
	}
	cfg := Config{Version: 1, Overrides: overrides, UpdatedAt: time.Now().UnixMilli(), UpdatedBy: by}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Config{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := safefs.Open(s.data)
	if err != nil {
		return Config{}, err
	}
	defer root.Close()
	if _, err := root.WriteFile(file, raw, safefs.WriteOptions{Mode: 0600}); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
