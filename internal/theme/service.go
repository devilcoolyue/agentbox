package theme

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agentbox/internal/safefs"
)

const (
	// MaxSite / MaxUser 限制条目数：主题清单整份下发给每个登录用户，也会全部
	// 出现在风格菜单里，不能由着无限增长。
	MaxSite = 50
	MaxUser = 20
)

var (
	ErrLimit    = errors.New("theme limit reached")
	ErrNotFound = errors.New("theme not found")
)

// Entry 是保存下来的一条主题：清单原样可导出，updated_at 由服务端记录。
type Entry struct {
	Manifest  Manifest  `json:"manifest"`
	UpdatedAt time.Time `json:"updated_at"`
}

type document struct {
	Version int              `json:"version"`
	Themes  map[string]Entry `json:"themes"`
}

// Service 把全站主题存在 <data>/themes.json，个人主题存在
// <data>/users/<user>/themes.json——都在容器挂载之外，并进入系统备份。
type Service struct {
	data string
	mu   sync.Mutex
}

func New(data string) *Service { return &Service{data: data} }

// location 返回范围对应的文件；user 为空表示全站。
func location(user string) (string, error) {
	if user == "" {
		return "themes.json", nil
	}
	if user == "." || user == ".." || strings.ContainsAny(user, `/\`+"\x00") {
		return "", errors.New("invalid theme owner")
	}
	return filepath.Join("users", user, "themes.json"), nil
}

func limitFor(user string) int {
	if user == "" {
		return MaxSite
	}
	return MaxUser
}

func (s *Service) read(user string) (document, error) {
	d := document{Version: 1, Themes: map[string]Entry{}}
	p, err := location(user)
	if err != nil {
		return d, err
	}
	root, err := safefs.Open(s.data)
	if err != nil {
		return d, err
	}
	defer root.Close()
	raw, err := root.ReadAll(p, int64(limitFor(user)+1)*MaxBytes)
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return d, errors.New("无法读取主题列表")
	}
	if json.Unmarshal(raw, &d) != nil || d.Version != 1 {
		return d, errors.New("主题列表文件版本或内容无效")
	}
	if d.Themes == nil {
		d.Themes = map[string]Entry{}
	}
	// 磁盘上的内容同样按清单规则复核：手工改坏的条目跳过并记日志，不下发给浏览器。
	for id, e := range d.Themes {
		if err := e.Manifest.Normalize(); err != nil || e.Manifest.ID != id {
			log.Printf("theme: skip invalid entry %q in %s: %v", id, p, err)
			delete(d.Themes, id)
			continue
		}
		d.Themes[id] = e
	}
	return d, nil
}

func (s *Service) write(user string, d document) error {
	p, err := location(user)
	if err != nil {
		return err
	}
	root, err := safefs.Open(s.data)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	_, err = root.WriteFile(p, raw, safefs.WriteOptions{Mode: 0600})
	return err
}

// List 返回某个范围的主题，按名称排序（同名按 ID）。
func (s *Service) List(user string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.read(user)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(d.Themes))
	for _, e := range d.Themes {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Manifest.Name), strings.ToLower(out[j].Manifest.Name)
		if a != b {
			return a < b
		}
		return out[i].Manifest.ID < out[j].Manifest.ID
	})
	return out, nil
}

// Put 新增或整份替换同 ID 的主题；清单须已经过 Parse 校验。
func (s *Service) Put(user string, m Manifest) (Entry, error) {
	if err := m.Normalize(); err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.read(user)
	if err != nil {
		return Entry{}, err
	}
	if _, exists := d.Themes[m.ID]; !exists && len(d.Themes) >= limitFor(user) {
		return Entry{}, fmt.Errorf("%w (%d)", ErrLimit, limitFor(user))
	}
	e := Entry{Manifest: m, UpdatedAt: time.Now().UTC().Truncate(time.Second)}
	d.Themes[m.ID] = e
	return e, s.write(user, d)
}

// Delete 删除一条主题；不存在时返回 ErrNotFound。
func (s *Service) Delete(user, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.read(user)
	if err != nil {
		return err
	}
	if _, ok := d.Themes[id]; !ok {
		return ErrNotFound
	}
	delete(d.Themes, id)
	return s.write(user, d)
}

// RemoveUser 删掉某个用户的个人主题文件。删除用户时调用，同名重建的用户不继承旧主题。
func (s *Service) RemoveUser(user string) error {
	if user == "" {
		return errors.New("invalid theme owner")
	}
	p, err := location(user)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := safefs.Open(s.data)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
