// Package mcpconfig owns user MCP configuration and workspace reconciliation.
// Canonical files live outside container mounts; native configuration remains
// owned by Claude. All writes beneath data_dir use pinned safefs handles.
package mcpconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"agentbox/internal/safefs"
)

const KeepSecret = "__AGENTBOX_KEEP_SECRET__"

var ErrRevision = errors.New("配置已变化，请刷新后重试")
var ErrConflict = errors.New("MCP 配置冲突，请在 MCP 页接管或解除管理")
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var envRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Definition struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}
type Entry struct {
	Config   Definition `json:"config"`
	Disabled bool       `json:"disabled,omitempty"`
}
type Document struct {
	Version  int                    `json:"version"`
	Revision int64                  `json:"revision"`
	Entries  map[string]Entry       `json:"entries"`
	Applied  map[string]*Definition `json:"applied,omitempty"`
	// Intent is durable before invoking the CLI, so a crash after a native write
	// can be recovered without mistaking our own write for a user's edit.
	Pending map[string]*Definition `json:"pending,omitempty"`
}
type Item struct {
	Name string `json:"name"`
	Entry
	Source         string      `json:"source"`
	Status         string      `json:"status"`
	NativeRevision string      `json:"native_revision,omitempty"`
	Native         *Definition `json:"native,omitempty"`
}
type External struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}
type View struct {
	External     []External `json:"external"`
	Revision     int64      `json:"revision"`
	UserRevision int64      `json:"user_revision"`
	Items        []Item     `json:"items"`
	ProjectNames []string   `json:"project_names"`
}
type Service struct {
	data string
	mu   sync.Map
}

func New(data string) *Service   { return &Service{data: data} }
func ValidName(name string) bool { return nameRE.MatchString(name) }
func component(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`+"\x00")
}
func location(user, session string) (string, error) {
	if !component(user) || (session != "" && !component(session)) {
		return "", errors.New("invalid MCP owner")
	}
	p := filepath.Join("users", user)
	if session != "" {
		p = filepath.Join(p, "sessions", session)
	}
	return filepath.Join(p, "mcp.json"), nil
}
func (s *Service) read(user, session string) (Document, error) {
	d := Document{Version: 1, Entries: map[string]Entry{}, Applied: map[string]*Definition{}, Pending: map[string]*Definition{}}
	p, err := location(user, session)
	if err != nil {
		return d, err
	}
	root, err := safefs.Open(s.data)
	if err != nil {
		return d, err
	}
	defer root.Close()
	raw, err := root.ReadAll(p, 4<<20)
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return d, errors.New("无法读取 MCP 配置")
	}
	if json.Unmarshal(raw, &d) != nil || d.Version != 1 || d.Entries == nil {
		return d, errors.New("MCP 配置版本或内容无效")
	}
	if d.Applied == nil {
		d.Applied = map[string]*Definition{}
	}
	if d.Pending == nil {
		d.Pending = map[string]*Definition{}
	}
	return d, nil
}
func (s *Service) write(user, session string, d Document) error {
	p, err := location(user, session)
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
	if len(raw) > 4<<20 {
		return errors.New("MCP 配置总大小超过 4 MiB")
	}
	_, err = root.WriteFile(p, raw, safefs.WriteOptions{Mode: 0600})
	return err
}
func Normalize(d Definition) Definition {
	if d.Type == "" {
		d.Type = "stdio"
	}
	if len(d.Args) == 0 {
		d.Args = nil
	}
	if len(d.Env) == 0 {
		d.Env = nil
	}
	if len(d.Headers) == 0 {
		d.Headers = nil
	}
	return d
}
func Validate(name string, d Definition) error {
	if !ValidName(name) {
		return errors.New("MCP 名称只能包含字母、数字、点、下划线和短横线，最多 64 字符")
	}
	d = Normalize(d)
	switch d.Type {
	case "stdio":
		if strings.TrimSpace(d.Command) == "" || d.URL != "" || len(d.Headers) > 0 {
			return errors.New("stdio 需要 command/args/env")
		}
	case "http":
		u, e := url.Parse(d.URL)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
			return errors.New("HTTP URL 必须是 http(s) 地址；凭证请使用 Header，不支持 URL 用户信息或片段")
		}
		if d.Command != "" || len(d.Args) > 0 || len(d.Env) > 0 {
			return errors.New("HTTP 只支持 URL 和 headers")
		}
	default:
		return errors.New("首版仅支持 stdio 和 http")
	}
	raw, _ := json.Marshal(d)
	if len(raw) > 32768 || strings.ContainsRune(d.Command, 0) {
		return errors.New("MCP 配置过大或含非法字符")
	}
	for _, a := range d.Args {
		if strings.ContainsRune(a, 0) {
			return errors.New("参数含非法字符")
		}
	}
	for k, v := range d.Env {
		if !envRE.MatchString(k) || strings.ContainsRune(v, 0) {
			return errors.New("环境变量格式无效")
		}
	}
	for k, v := range d.Headers {
		if !regexp.MustCompile(`^[!#$%&'*+.^_`+"`"+`|~0-9A-Za-z-]+$`).MatchString(k) || strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("HTTP Header 格式无效")
		}
	}
	return nil
}
func Mask(d Definition) Definition {
	d = Normalize(d)
	raw, _ := json.Marshal(d)
	d = Definition{}
	_ = json.Unmarshal(raw, &d)
	for k := range d.Env {
		d.Env[k] = KeepSecret
	}
	for k := range d.Headers {
		d.Headers[k] = KeepSecret
	}
	return d
}
func preserve(next, old map[string]string) error {
	for k, v := range next {
		if v == KeepSecret {
			previous, ok := old[k]
			if !ok {
				return errors.New("无法保留不存在的凭证")
			}
			next[k] = previous
		}
	}
	return nil
}
func digest(d *Definition) string {
	if d == nil {
		return "absent"
	}
	n := Normalize(*d)
	raw, _ := json.Marshal(n)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func same(a, b *Definition) bool { return digest(a) == digest(b) }
func (s *Service) Put(user, session, name string, revision int64, e *Entry) error {
	unlock, _ := s.lock(context.Background(), user)
	defer unlock()
	d, err := s.read(user, session)
	if err != nil {
		return err
	}
	if d.Revision != revision {
		return ErrRevision
	}
	if !ValidName(name) {
		return errors.New("MCP 名称无效")
	}
	if e == nil {
		delete(d.Entries, name)
	} else {
		old, ok := d.Entries[name]
		if !ok && session != "" {
			u, err := s.read(user, "")
			if err != nil {
				return err
			}
			old = u.Entries[name]
		}
		e.Config = Normalize(e.Config)
		// A disabled inheritance marker need not carry a duplicate definition.
		if e.Disabled && e.Config.Command == "" && e.Config.URL == "" {
			e.Config = Definition{}
		} else {
			if err = preserve(e.Config.Env, old.Config.Env); err != nil {
				return err
			}
			if err = preserve(e.Config.Headers, old.Config.Headers); err != nil {
				return err
			}
			if err = Validate(name, e.Config); err != nil {
				return err
			}
		}
		if len(d.Entries) >= 100 {
			if _, exists := d.Entries[name]; !exists {
				return errors.New("最多配置 100 个 MCP")
			}
		}
		d.Entries[name] = *e
	}
	d.Revision++
	return s.write(user, session, d)
}
func (s *Service) native(user, session string) (map[string]*Definition, []string, error) {
	p, err := location(user, session)
	if err != nil {
		return nil, nil, err
	}
	root, err := safefs.Open(s.data)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	base := filepath.Dir(p)
	read := func(rel string) (map[string]*Definition, error) {
		raw, e := root.ReadAll(filepath.Join(base, rel), 4<<20)
		if os.IsNotExist(e) {
			return map[string]*Definition{}, nil
		}
		if e != nil {
			return nil, errors.New("无法安全读取 Claude MCP 配置")
		}
		var state struct {
			Servers map[string]*Definition `json:"mcpServers"`
		}
		if json.Unmarshal(raw, &state) != nil {
			return nil, errors.New("Claude MCP 配置 JSON 无效")
		}
		if state.Servers == nil {
			state.Servers = map[string]*Definition{}
		}
		var original struct {
			Servers map[string]json.RawMessage `json:"mcpServers"`
		}
		_ = json.Unmarshal(raw, &original)
		for k, v := range state.Servers {
			if v == nil {
				delete(state.Servers, k)
			} else {
				n := Normalize(*v)
				decoder := json.NewDecoder(strings.NewReader(string(original.Servers[k])))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(new(Definition)); err != nil {
					n = Definition{Type: "unsupported"}
				}
				state.Servers[k] = &n
			}
		}
		return state.Servers, nil
	}
	n, err := read("home/.claude.json")
	if err != nil {
		return nil, nil, err
	}
	project, err := read("workspace/.mcp.json")
	if err != nil {
		return nil, nil, err
	}
	names := []string{}
	for k := range project {
		names = append(names, k)
	}
	sort.Strings(names)
	return n, names, nil
}
func merged(u, d Document) map[string]Entry {
	m := map[string]Entry{}
	for k, v := range u.Entries {
		m[k] = v
	}
	for k, v := range d.Entries {
		m[k] = v
	}
	return m
}
func (s *Service) View(user, session string) (View, error) {
	unlock, _ := s.lock(context.Background(), user)
	defer unlock()
	d, err := s.read(user, session)
	if err != nil {
		return View{}, err
	}
	u, err := s.read(user, "")
	if err != nil {
		return View{}, err
	}
	v := View{Revision: d.Revision, UserRevision: u.Revision, Items: []Item{}, ProjectNames: []string{}, External: []External{}}
	if session == "" {
		for name, e := range d.Entries {
			e.Config = Mask(e.Config)
			v.Items = append(v.Items, Item{Name: name, Entry: e, Source: "user", Status: "configured"})
		}
	} else {
		native, projects, err := s.native(user, session)
		if err != nil {
			return v, err
		}
		v.ProjectNames = projects
		v.External = s.external(user, session)
		effective := merged(u, d)
		names := map[string]bool{}
		for n := range effective {
			names[n] = true
		}
		for n := range native {
			names[n] = true
		}
		for n := range d.Applied {
			names[n] = true
		}
		for n := range d.Pending {
			names[n] = true
		}
		for name := range names {
			e, managed := effective[name]
			actual := native[name]
			i := Item{Name: name, Entry: e, Source: "user", Status: "pending", NativeRevision: digest(actual)}
			if _, ok := d.Entries[name]; ok {
				i.Source = "session"
			}
			if actual != nil {
				m := Mask(*actual)
				i.Native = &m
			}
			var want *Definition
			if managed && !e.Disabled {
				want = &e.Config
			}
			prev, owned := d.Applied[name]
			pending, intent := d.Pending[name]
			if !managed && !owned && !intent {
				i.Source = "native"
				i.Status = "unmanaged"
				if actual != nil {
					i.Config = *actual
				}
			} else if !managed && (owned || intent) && !conflict(actual, prev, pending, owned, intent) {
				i.Status = "pending_delete"
				if actual != nil {
					i.Config = *actual
				}
			} else if managed && e.Disabled && !owned && !intent {
				i.Status = "disabled"
			} else if conflict(actual, prev, pending, owned, intent) {
				i.Status = "conflict"
			} else if same(want, actual) {
				i.Status = "applied"
			}
			i.Config = Mask(i.Config)
			v.Items = append(v.Items, i)
		}
	}
	sort.Slice(v.Items, func(i, j int) bool { return v.Items[i].Name < v.Items[j].Name })
	return v, nil
}

// Adopt explicitly records the observed native value as owned. The caller must
// supply the fingerprint it reviewed. Release relinquishes management without
// modifying Claude and disables inheritance in this workspace.
func (s *Service) Adopt(user, session, name string, revision int64, fingerprint string, release bool) error {
	unlock, _ := s.lock(context.Background(), user)
	defer unlock()
	if !ValidName(name) {
		return errors.New("MCP 名称无效")
	}
	d, err := s.read(user, session)
	if err != nil {
		return err
	}
	if d.Revision != revision {
		return ErrRevision
	}
	n, _, err := s.native(user, session)
	if err != nil {
		return err
	}
	actual := n[name]
	if digest(actual) != fingerprint {
		return ErrRevision
	}
	if release {
		delete(d.Applied, name)
		delete(d.Pending, name)
		d.Entries[name] = Entry{Disabled: true} // A released native definition must not conflict with the disabled marker.
	} else {
		if actual == nil {
			return errors.New("原生配置不存在")
		}
		if err = Validate(name, *actual); err != nil {
			return err
		}
		d.Entries[name] = Entry{Config: *actual}
		d.Applied[name] = actual
		delete(d.Pending, name)
	}
	d.Revision++
	return s.write(user, session, d)
}

// Apply changes one native entry using the CLI, with a compare-before-write
// expectation. Payloads must travel over stdin, never through a shell.
type Apply func(context.Context, string, *Definition, *Definition) error

func (s *Service) Sync(ctx context.Context, user, session string, apply Apply) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	unlock, lockErr := s.lock(ctx, user)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	d, err := s.read(user, session)
	if err != nil {
		return err
	}
	u, err := s.read(user, "")
	if err != nil {
		return err
	}
	entries := merged(u, d)
	if len(entries)+len(d.Applied)+len(d.Pending) == 0 {
		return nil
	}
	native, _, err := s.native(user, session)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for n := range entries {
		names[n] = true
	}
	for n := range d.Applied {
		names[n] = true
	}
	for n := range d.Pending {
		names[n] = true
	}
	ordered := []string{}
	for n := range names {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)
	var conflicts []string
	for _, name := range ordered {
		if err = ctx.Err(); err != nil {
			return err
		}
		e, exists := entries[name]
		actual := native[name]
		prev, owned := d.Applied[name]
		intent, pending := d.Pending[name]
		// Disabling an unowned name means not inheriting it, not deleting a native server.
		if exists && e.Disabled && !owned && !pending {
			continue
		}
		if conflict(actual, prev, intent, owned, pending) {
			conflicts = append(conflicts, name)
			continue
		}
		var want *Definition
		if exists && !e.Disabled {
			n := Normalize(e.Config)
			if err = Validate(name, n); err != nil {
				return err
			}
			want = &n
		}
		if owned && !pending && same(actual, want) && same(prev, want) {
			continue
		}
		if !same(actual, want) {
			d.Pending[name] = want
			if err = s.write(user, session, d); err != nil {
				return err
			}
			if err = apply(ctx, name, actual, want); err != nil {
				return fmt.Errorf("MCP %s 应用失败，请检查容器或终端配置后重试", name)
			}
		}
		delete(d.Pending, name)
		if want == nil {
			delete(d.Applied, name)
		} else {
			d.Applied[name] = want
		}
		if err = s.write(user, session, d); err != nil {
			return err
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%w：%s", ErrConflict, strings.Join(conflicts, ", "))
	}
	return nil
}
func (s *Service) Definition(user, session, name string) (Definition, error) {
	unlock, _ := s.lock(context.Background(), user)
	defer unlock()
	d, err := s.read(user, session)
	if err != nil {
		return Definition{}, err
	}
	u, err := s.read(user, "")
	if err != nil {
		return Definition{}, err
	}
	e, ok := merged(u, d)[name]
	if !ok || e.Disabled {
		return Definition{}, errors.New("请先配置并启用该 MCP")
	}
	return e.Config, Validate(name, e.Config)
}

func (s *Service) lock(ctx context.Context, user string) (func(), error) {
	value, _ := s.mu.LoadOrStore(user, make(chan struct{}, 1))
	gate := value.(chan struct{})
	select {
	case gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-gate
			return nil, err
		}
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func conflict(actual, previous, intent *Definition, owned, pending bool) bool {
	if pending {
		return actual != nil && !same(actual, previous) && !same(actual, intent)
	}
	return (!owned && actual != nil) || (owned && !same(previous, actual))
}

// Import validates every definition before a single atomic write. Existing
// names are rejected; overwrites go through the explicit edit flow.
func (s *Service) Import(user, session string, revision int64, servers map[string]Definition) error {
	unlock, _ := s.lock(context.Background(), user)
	defer unlock()
	d, err := s.read(user, session)
	if err != nil {
		return err
	}
	if revision != d.Revision {
		return ErrRevision
	}
	if len(servers) == 0 || len(servers)+len(d.Entries) > 100 {
		return errors.New("导入数量无效，最多 100 个 MCP")
	}
	for name, def := range servers {
		if _, ok := d.Entries[name]; ok {
			return fmt.Errorf("MCP %s 已存在，请单独编辑", name)
		}
		def = Normalize(def)
		if err = Validate(name, def); err != nil {
			return err
		}
		for _, values := range []map[string]string{def.Env, def.Headers} {
			for _, v := range values {
				if v == KeepSecret {
					return errors.New("导入需要实际凭证值")
				}
			}
		}
		d.Entries[name] = Entry{Config: def}
	}
	d.Revision++
	return s.write(user, session, d)
}

// External sources are informational only. Registry entries indicate installed
// plugins, not whether they expose MCP tools; no plugin paths are followed.
func (s *Service) external(user, session string) []External {
	out := []External{}
	root, err := safefs.Open(s.data)
	if err != nil {
		return out
	}
	defer root.Close()
	p, err := location(user, session)
	if err != nil {
		return out
	}
	base := filepath.Join(filepath.Dir(p), "home")
	raw, err := root.ReadAll(filepath.Join(base, ".claude.json"), 4<<20)
	if err == nil {
		var state struct {
			Projects map[string]struct {
				MCP map[string]json.RawMessage `json:"mcpServers"`
			} `json:"projects"`
		}
		if json.Unmarshal(raw, &state) == nil {
			for name := range state.Projects["/workspace"].MCP {
				if ValidName(name) {
					out = append(out, External{Name: name, Source: "local"})
				}
			}
		}
	}
	raw, err = root.ReadAll(filepath.Join(base, ".claude/plugins/installed_plugins.json"), 4<<20)
	if err == nil {
		var registry struct {
			Plugins map[string]json.RawMessage `json:"plugins"`
		}
		if json.Unmarshal(raw, &registry) == nil {
			for name := range registry.Plugins {
				if len(name) <= 256 {
					out = append(out, External{Name: name, Source: "plugin"})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}
