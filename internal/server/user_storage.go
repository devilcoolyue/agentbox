package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/storagepolicy"
	"agentbox/internal/store"
)

// 管理员按用户设置存储上限：建用户时指定，之后在用户管理里改，删用户时释放。
// 上限先写进内核项目配额，成功后再记进 storage_policy.users / cache_users，
// 重启时 ensureUserStorage 按配置重放，两边保持一致。

// Quota helper hooks; tests swap them because the helper needs a prjquota mount.
var (
	storageEnsure     = storagepolicy.Ensure
	storageReadReport = storagepolicy.ReadReport
	storageRetire     = storagepolicy.Retire
)

const maxUserStorageBytes = 1 << 50

// storageLimits: nil keeps the current setting, 0 falls back to the default.
type storageLimits struct {
	DataBytes  *int64 `json:"data_bytes"`
	CacheBytes *int64 `json:"cache_bytes"`
}

func (l storageLimits) set() bool {
	return (l.DataBytes != nil && *l.DataBytes != 0) || (l.CacheBytes != nil && *l.CacheBytes != 0)
}

type userStorageView struct {
	DataBytes   int64  `json:"data_bytes"`
	CacheBytes  int64  `json:"cache_bytes"` // 0 = 没启用共享工具目录，没有缓存配额
	DataCustom  bool   `json:"data_custom"`
	CacheCustom bool   `json:"cache_custom"`
	DataUsed    *int64 `json:"data_used,omitempty"` // 内核配额已用，还没读到时缺省
	CacheUsed   *int64 `json:"cache_used,omitempty"`
}

type storageQuotaSummary struct {
	Enabled      bool   `json:"enabled"`
	CacheEnabled bool   `json:"cache_enabled"`
	DefaultData  int64  `json:"default_data_bytes"`
	DefaultCache int64  `json:"default_cache_bytes"`
	Budget       int64  `json:"budget_bytes"`    // 0 = 不限
	Allocated    int64  `json:"allocated_bytes"` // 计入预算的合计：各用户上限 + 已删除账号剩下的文件
	Error        string `json:"error,omitempty"`
}

type storageLimitError struct {
	status int
	msg    string
}

func (e storageLimitError) Error() string { return e.msg }

// userStorageLimits is a user's effective limits; cache is 0 without shared
// tool storage, where no cache project exists.
func userStorageLimits(p config.StoragePolicy, tools, user string) (data, cache int64) {
	data = p.UserBytes
	if v := p.Users[user]; v > 0 {
		data = v
	}
	if tools != "" {
		cache = p.CacheBytes
		if v := p.CacheUsers[user]; v > 0 {
			cache = v
		}
	}
	return data, cache
}

// storageSize matches the web fmtBytes units (1024-based GB/MB).
func storageSize(b int64) string {
	if b >= 1<<30 {
		return strconv.FormatFloat(math.Round(float64(b)/(1<<30)*100)/100, 'f', -1, 64) + " GB"
	}
	return strconv.FormatInt((b+1<<19)>>20, 10) + " MB"
}

// setUserStorage applies limits to the kernel first and then records them; a
// failed config write puts the previous kernel limits back.
func (s *Server) setUserStorage(ctx context.Context, u store.User, req storageLimits) error {
	tools := s.cfg.GetToolStorage().Root
	if s.cfg.GetStoragePolicy().Mount == "" {
		return storageLimitError{http.StatusBadRequest, "未启用存储配额（storage_policy），不能设置用户存储上限"}
	}
	for _, v := range []*int64{req.DataBytes, req.CacheBytes} {
		if v != nil && *v != 0 && (*v < 1<<20 || *v > maxUserStorageBytes) {
			return storageLimitError{http.StatusBadRequest, "存储上限需在 1 MB 到 1 PB 之间"}
		}
	}
	if req.CacheBytes != nil && *req.CacheBytes != 0 && tools == "" {
		return storageLimitError{http.StatusBadRequest, "未启用共享工具目录，没有缓存配额可设"}
	}
	key, err := s.storageOwner(u)
	if err != nil {
		return err
	}
	s.storageUsersMu.Lock()
	defer s.storageUsersMu.Unlock()
	p := s.cfg.GetStoragePolicy()
	next := p.Clone()
	apply := func(m map[string]int64, v *int64) map[string]int64 {
		if v == nil {
			return m
		}
		if *v == 0 {
			delete(m, u.Name)
			return m
		}
		if m == nil {
			m = map[string]int64{}
		}
		m[u.Name] = *v
		return m
	}
	next.Users = apply(next.Users, req.DataBytes)
	next.CacheUsers = apply(next.CacheUsers, req.CacheBytes)
	data, cache := userStorageLimits(next, tools, u.Name)

	// The helper enforces the budget too; checking here gives a usable message.
	report, err := storageReadReport(ctx, p, s.cfg.DataDir)
	if err != nil {
		return err
	}
	var others int64
	for _, pr := range report.Projects {
		if pr.Owner != key {
			others += pr.Charged()
			continue
		}
		limit, label := data, "数据"
		if pr.Kind == "cache" {
			limit, label = cache, "缓存"
		}
		if !pr.Retired && pr.UsedBytes > limit {
			return storageLimitError{http.StatusConflict, fmt.Sprintf("%s上限不能低于已用的 %s", label, storageSize(pr.UsedBytes))}
		}
	}
	if p.BudgetBytes > 0 && others+data+cache > p.BudgetBytes {
		return storageLimitError{http.StatusConflict, fmt.Sprintf("超出存储总预算：该用户最多可分配 %s（数据+缓存），本次需要 %s",
			storageSize(max(0, p.BudgetBytes-others)), storageSize(data+cache))}
	}

	if _, err = storageEnsure(ctx, next, s.cfg.DataDir, tools, u.Name, key); err != nil {
		return err
	}
	if next.Users[u.Name] != p.Users[u.Name] || next.CacheUsers[u.Name] != p.CacheUsers[u.Name] {
		if err = s.cfg.SetUserStorageLimits(u.Name, next.Users[u.Name], next.CacheUsers[u.Name]); err != nil {
			if _, rerr := storageEnsure(ctx, p, s.cfg.DataDir, tools, u.Name, key); rerr != nil {
				log.Printf("storage: restore limits of %s: %v", u.Name, rerr)
			}
			return err
		}
	}
	if s.storageUsers == nil {
		s.storageUsers = map[string]bool{}
	}
	s.storageUsers[key] = true
	return nil
}

// retireUserStorage releases a deleted user's limits. Leftover files keep
// counting against the budget until an operator removes them.
func (s *Server) retireUserStorage(ctx context.Context, u store.User) {
	p := s.cfg.GetStoragePolicy()
	if p.Mount != "" {
		if key, err := s.storageOwner(u); err != nil {
			log.Printf("delete user %s: storage owner: %v", u.Name, err)
		} else {
			s.storageUsersMu.Lock()
			if err = storageRetire(ctx, p, s.cfg.DataDir, key); err != nil {
				log.Printf("delete user %s: release storage quota: %v", u.Name, err)
			}
			delete(s.storageUsers, key)
			s.storageUsersMu.Unlock()
		}
	}
	if p.Users[u.Name] != 0 || p.CacheUsers[u.Name] != 0 {
		// 同名重建的用户不继承旧上限
		if err := s.cfg.SetUserStorageLimits(u.Name, 0, 0); err != nil {
			log.Printf("delete user %s: prune storage limits: %v", u.Name, err)
		}
	}
	s.refreshStorageQuota(ctx)
}

// refreshStorageQuota reads kernel usage now and updates the monitor's copy.
func (s *Server) refreshStorageQuota(ctx context.Context) (storagepolicy.Report, error) {
	p := s.cfg.GetStoragePolicy()
	if p.Mount == "" {
		return storagepolicy.Report{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	report, err := storageReadReport(ctx, p, s.cfg.DataDir)
	s.storage.Lock()
	if err != nil {
		s.storage.quotaErr = err.Error()
	} else {
		s.storage.quota, s.storage.quotaAt, s.storage.quotaErr = report, time.Now().UnixMilli(), ""
	}
	s.storage.Unlock()
	return report, err
}

// userStorageViews resolves every user's limits and last-read usage.
func (s *Server) userStorageViews(users []store.User) map[string]*userStorageView {
	p := s.cfg.GetStoragePolicy()
	if p.Mount == "" {
		return nil
	}
	tools := s.cfg.GetToolStorage().Root
	s.storage.RLock()
	projects := s.storage.quota.Projects
	s.storage.RUnlock()
	used := map[string]int64{}
	for _, pr := range projects {
		if !pr.Retired {
			used[pr.Kind+":"+pr.Owner] = pr.UsedBytes
		}
	}
	out := map[string]*userStorageView{}
	for _, u := range users {
		data, cache := userStorageLimits(p, tools, u.Name)
		v := &userStorageView{DataBytes: data, CacheBytes: cache, DataCustom: p.Users[u.Name] > 0, CacheCustom: cache > 0 && p.CacheUsers[u.Name] > 0}
		if key, err := s.storageOwner(u); err == nil {
			if b, ok := used["data:"+key]; ok {
				v.DataUsed = &b
			}
			if b, ok := used["cache:"+key]; ok {
				v.CacheUsed = &b
			}
		}
		out[u.Name] = v
	}
	return out
}

func (s *Server) handleStorageQuotas(w http.ResponseWriter, r *http.Request) {
	p := s.cfg.GetStoragePolicy()
	tools := s.cfg.GetToolStorage().Root
	out := storageQuotaSummary{Enabled: p.Mount != "", CacheEnabled: p.Mount != "" && tools != "", DefaultData: p.UserBytes, Budget: p.BudgetBytes}
	if out.CacheEnabled {
		out.DefaultCache = p.CacheBytes
	}
	if out.Enabled {
		report, err := s.refreshStorageQuota(r.Context())
		if err != nil {
			out.Error = err.Error()
		}
		for _, pr := range report.Projects {
			out.Allocated += pr.Charged()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUserStoragePut(w http.ResponseWriter, r *http.Request) {
	u, ok := s.store.GetUser(r.PathValue("name"))
	if !ok {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	var req storageLimits
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if err := s.setUserStorage(r.Context(), u, req); err != nil {
		writeStorageErr(w, u.Name, err)
		return
	}
	s.refreshStorageQuota(r.Context())
	writeJSON(w, http.StatusOK, s.userStorageViews([]store.User{u})[u.Name])
}

func writeStorageErr(w http.ResponseWriter, user string, err error) {
	var limit storageLimitError
	switch {
	case errors.As(err, &limit):
		writeErr(w, limit.status, limit.msg)
	case errors.Is(err, storagepolicy.ErrBudget):
		writeErr(w, http.StatusConflict, "超出存储总预算，请先调小其他用户的上限，或由运维提高预算")
	case errors.Is(err, storagepolicy.ErrPreviousData):
		writeErr(w, http.StatusConflict, "已删除的同名账号还有文件留在 users/"+user+"，请运维清理后再试")
	default:
		log.Printf("storage: set limits of %s: %v", user, err)
		writeErr(w, http.StatusServiceUnavailable, "用户存储尚未就绪，请联系管理员")
	}
}
