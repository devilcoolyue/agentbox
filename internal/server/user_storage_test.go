package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/storagepolicy"
	"agentbox/internal/store"
)

// fakeQuota stands in for quota.py: one project per kind and owner, the same
// budget rule, and retirement releasing the limit while usage stays charged.
type fakeQuota struct {
	projects map[string]*storagepolicy.Project
	err      error
	retired  []string
}

func installFakeQuota(t *testing.T) *fakeQuota {
	f := &fakeQuota{projects: map[string]*storagepolicy.Project{}}
	ensure, read, retire := storageEnsure, storageReadReport, storageRetire
	t.Cleanup(func() { storageEnsure, storageReadReport, storageRetire = ensure, read, retire })
	storageEnsure = func(_ context.Context, p config.StoragePolicy, _, tools, user, owner string) (storagepolicy.UserUsage, error) {
		if f.err != nil {
			return storagepolicy.UserUsage{}, f.err
		}
		data, cache := userStorageLimits(p, tools, user)
		total := data + cache
		for _, pr := range f.projects {
			if pr.Owner != owner {
				total += pr.Charged()
			}
		}
		if p.BudgetBytes > 0 && total > p.BudgetBytes {
			return storagepolicy.UserUsage{}, storagepolicy.ErrBudget
		}
		f.set("data", owner, data)
		if tools != "" {
			f.set("cache", owner, cache)
		}
		return storagepolicy.UserUsage{}, nil
	}
	storageReadReport = func(context.Context, config.StoragePolicy, string) (storagepolicy.Report, error) {
		var r storagepolicy.Report
		for _, pr := range f.projects {
			r.Projects = append(r.Projects, *pr)
		}
		return r, nil
	}
	storageRetire = func(_ context.Context, _ config.StoragePolicy, _, owner string) error {
		for _, pr := range f.projects {
			if pr.Owner == owner {
				pr.Retired, pr.LimitBytes = true, 0
			}
		}
		f.retired = append(f.retired, owner)
		return nil
	}
	return f
}

func (f *fakeQuota) set(kind, owner string, limit int64) {
	pr := f.projects[kind+":"+owner]
	if pr == nil {
		pr = &storagepolicy.Project{Kind: kind, Owner: owner}
		f.projects[kind+":"+owner] = pr
	}
	pr.LimitBytes = limit
}

func storageTestServer(t *testing.T, policy map[string]any) (*Server, *fakeQuota) {
	t.Helper()
	s, _ := newTestServer(t)
	mount := filepath.Dir(s.cfg.DataDir)
	doc := map[string]any{"data_dir": s.cfg.DataDir, "auth_token": "test-token-long-enough", "agent_image": "test"}
	if policy != nil {
		doc["tool_storage"] = map[string]any{"root": filepath.Join(mount, "tools")}
		doc["storage_policy"] = policy
		policy["mount"], policy["uuid"] = mount, "0123456789abcdef0123"
	}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	if s.cfg, err = config.Load(path); err != nil {
		t.Fatal(err)
	}
	return s, installFakeQuota(t)
}

func storageCall(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	switch {
	case method == http.MethodPost && path == "/api/users":
		s.handleUserCreate(w, r)
	case method == http.MethodPut:
		r.SetPathValue("name", strings.Split(path, "/")[3])
		s.handleUserStoragePut(w, r)
	case method == http.MethodDelete:
		r.SetPathValue("name", strings.Split(path, "/")[3])
		s.handleUserDelete(w, r.WithContext(context.WithValue(r.Context(), ctxUser, store.User{Name: "root", Role: store.RoleAdmin})))
	case path == "/api/storage/quotas":
		s.handleStorageQuotas(w, r)
	default:
		s.handleUserList(w, r)
	}
	return w
}

func reloadedPolicy(t *testing.T, s *Server) config.StoragePolicy {
	t.Helper()
	c, err := config.Load(s.cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	return c.GetStoragePolicy()
}

const gib = int64(1 << 30)

func TestUserStorageCreateWithLimitsAndBudget(t *testing.T) {
	s, f := storageTestServer(t, map[string]any{"user_bytes": gib, "cache_bytes": 256 << 20, "budget_bytes": 4 * gib,
		"users": map[string]int64{"carol": 3 * gib}}) // 已删除同名账号留下的旧上限

	w := storageCall(t, s, "POST", "/api/users", `{"username":"alice","password":"long-enough","data_bytes":2147483648,"cache_bytes":536870912}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create alice: %d %s", w.Code, w.Body.String())
	}
	alice, _ := s.store.GetUser("alice")
	key, _ := s.storageOwner(alice)
	if f.projects["data:"+key].LimitBytes != 2*gib || f.projects["cache:"+key].LimitBytes != 512<<20 {
		t.Fatalf("kernel limits = %+v %+v", f.projects["data:"+key], f.projects["cache:"+key])
	}
	if p := reloadedPolicy(t, s); p.Users["alice"] != 2*gib || p.CacheUsers["alice"] != 512<<20 {
		t.Fatalf("config not persisted: %+v %+v", p.Users, p.CacheUsers)
	}

	// 没指定的用默认值，并清掉旧上限
	if w = storageCall(t, s, "POST", "/api/users", `{"username":"carol","password":"long-enough"}`); w.Code != http.StatusCreated {
		t.Fatalf("create carol: %d %s", w.Code, w.Body.String())
	}
	if p := reloadedPolicy(t, s); p.Users["carol"] != 0 {
		t.Fatalf("stale limit inherited: %+v", p.Users)
	}

	// 2.5G + 1.25G 已分配，再要 1G+256M 超过 4G 预算：拒绝且不建用户
	w = storageCall(t, s, "POST", "/api/users", `{"username":"dave","password":"long-enough"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "超出存储总预算") || !strings.Contains(w.Body.String(), "256 MB") {
		t.Fatalf("over budget: %d %s", w.Code, w.Body.String())
	}
	if _, ok := s.store.GetUser("dave"); ok {
		t.Fatal("user created over budget")
	}
	if w = storageCall(t, s, "POST", "/api/users", `{"username":"dave","password":"long-enough","data_bytes":1024}`); w.Code != http.StatusBadRequest {
		t.Fatalf("tiny limit: %d %s", w.Code, w.Body.String())
	}

	var summary storageQuotaSummary
	w = storageCall(t, s, "GET", "/api/storage/quotas", "")
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if !summary.Enabled || !summary.CacheEnabled || summary.Allocated != 2*gib+512<<20+gib+256<<20 || summary.Budget != 4*gib || summary.DefaultCache != 256<<20 {
		t.Fatalf("summary = %+v", summary)
	}

	var list []userView
	if err := json.Unmarshal(storageCall(t, s, "GET", "/api/users", "").Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, u := range list {
		if u.Name == "alice" && (u.Storage == nil || !u.Storage.DataCustom || u.Storage.DataBytes != 2*gib || u.Storage.DataUsed == nil) {
			t.Fatalf("alice storage view = %+v", u.Storage)
		}
		if u.Name == "carol" && (u.Storage == nil || u.Storage.DataCustom || u.Storage.DataBytes != gib) {
			t.Fatalf("carol storage view = %+v", u.Storage)
		}
	}
}

func TestUserStorageUpdateAndRelease(t *testing.T) {
	s, f := storageTestServer(t, map[string]any{"user_bytes": gib, "cache_bytes": 256 << 20, "budget_bytes": 4 * gib})
	for _, name := range []string{"alice", "bob"} {
		if w := storageCall(t, s, "POST", "/api/users", `{"username":"`+name+`","password":"long-enough"}`); w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, w.Code, w.Body.String())
		}
	}
	alice, _ := s.store.GetUser("alice")
	key, _ := s.storageOwner(alice)
	f.projects["data:"+key].UsedBytes = 600 << 20

	if w := storageCall(t, s, "PUT", "/api/users/alice/storage", `{"data_bytes":524288000}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "600 MB") {
		t.Fatalf("below usage: %d %s", w.Code, w.Body.String())
	}
	// 其余 1.25G 给 bob，alice 最多 2.75G
	if w := storageCall(t, s, "PUT", "/api/users/alice/storage", `{"data_bytes":3221225472}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "2.75 GB") {
		t.Fatalf("over budget: %d %s", w.Code, w.Body.String())
	}
	w := storageCall(t, s, "PUT", "/api/users/alice/storage", `{"data_bytes":2147483648}`)
	if w.Code != http.StatusOK || f.projects["data:"+key].LimitBytes != 2*gib || reloadedPolicy(t, s).Users["alice"] != 2*gib {
		t.Fatalf("raise: %d %s %+v", w.Code, w.Body.String(), f.projects["data:"+key])
	}
	// 不带的一项保持原样，0 回到默认
	if w = storageCall(t, s, "PUT", "/api/users/alice/storage", `{"cache_bytes":536870912}`); w.Code != http.StatusOK || reloadedPolicy(t, s).Users["alice"] != 2*gib {
		t.Fatalf("partial update: %d %s", w.Code, w.Body.String())
	}
	if w = storageCall(t, s, "PUT", "/api/users/alice/storage", `{"data_bytes":0,"cache_bytes":0}`); w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body.String())
	}
	if p := reloadedPolicy(t, s); len(p.Users) != 0 || len(p.CacheUsers) != 0 || f.projects["data:"+key].LimitBytes != gib {
		t.Fatalf("reset left %+v %+v", p.Users, p.CacheUsers)
	}

	f.err = storagepolicy.ErrPreviousData
	if w = storageCall(t, s, "PUT", "/api/users/bob/storage", `{"data_bytes":2147483648}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "users/bob") {
		t.Fatalf("previous data: %d %s", w.Code, w.Body.String())
	}
	f.err = nil

	if w = storageCall(t, s, "PUT", "/api/users/alice/storage", `{"data_bytes":2147483648}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if w = storageCall(t, s, "DELETE", "/api/users/alice", ""); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if len(f.retired) != 1 || f.retired[0] != key || reloadedPolicy(t, s).Users["alice"] != 0 {
		t.Fatalf("delete kept quota: retired %v users %+v", f.retired, reloadedPolicy(t, s).Users)
	}
	// 只剩 alice 留下的 600M 文件计入预算
	var summary storageQuotaSummary
	json.Unmarshal(storageCall(t, s, "GET", "/api/storage/quotas", "").Body.Bytes(), &summary)
	if summary.Allocated != 600<<20+gib+256<<20 {
		t.Fatalf("allocated after delete = %d", summary.Allocated)
	}
	if s.storage.quotaAt == 0 {
		t.Fatal("monitor copy not refreshed")
	}
}

func TestUserStorageDisabledPolicy(t *testing.T) {
	s, _ := storageTestServer(t, nil)
	if w := storageCall(t, s, "POST", "/api/users", `{"username":"alice","password":"long-enough","data_bytes":2147483648}`); w.Code != http.StatusBadRequest {
		t.Fatalf("limits without policy: %d %s", w.Code, w.Body.String())
	}
	if w := storageCall(t, s, "POST", "/api/users", `{"username":"alice","password":"long-enough"}`); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w := storageCall(t, s, "PUT", "/api/users/alice/storage", `{"data_bytes":2147483648}`); w.Code != http.StatusBadRequest {
		t.Fatalf("update without policy: %d %s", w.Code, w.Body.String())
	}
	if body := storageCall(t, s, "GET", "/api/users", "").Body.String(); strings.Contains(body, `"storage"`) {
		t.Fatalf("storage shown without policy: %s", body)
	}
	if body := storageCall(t, s, "GET", "/api/storage/quotas", "").Body.String(); !strings.Contains(body, `"enabled":false`) {
		t.Fatal(body)
	}
}
