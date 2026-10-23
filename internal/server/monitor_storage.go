package server

import (
	"path/filepath"
	"sort"

	"agentbox/internal/storagepolicy"
)

// 运维监控里的存储一栏：每个用户的数据/缓存配额（内核项目配额，每分钟读一次）
// 与目录统计（每 5 分钟遍历一次，按文件逻辑大小），都只读缓存，不拖慢 5 秒一次的轮询。
// 账号删了配额还占着（见 storagepolicy 的登记），这类也列出来，免得预算算不平。

type monitorStorage struct {
	QuotaAt    int64                `json:"quota_at"` // 配额读取时间(ms)，0=未启用或还没读到
	QuotaError string               `json:"quota_error,omitempty"`
	Budget     int64                `json:"budget_bytes"`         // 配额总预算，0=不限
	Allocated  int64                `json:"allocated_bytes"`      // 已登记项目的限额合计（含已删除用户）
	Unassigned *storagepolicy.Usage `json:"unassigned,omitempty"` // 用户目录以外的占用：镜像、交换文件、备份等
	ScannedAt  int64                `json:"scanned_at"`           // 目录统计时间(ms)，0=首次统计中
	Partial    bool                 `json:"partial"`              // 目录统计没走完
	Users      []userStorage        `json:"users"`
}

type userStorage struct {
	User     string               `json:"user"`
	Deleted  bool                 `json:"deleted,omitempty"` // 账号已删除，配额仍未释放
	Data     *storagepolicy.Usage `json:"data,omitempty"`    // 数据配额（users/<u>）
	Cache    *storagepolicy.Usage `json:"cache,omitempty"`   // 缓存配额（工具目录下的私有缓存）
	Files    int64                `json:"files_bytes"`       // 目录统计：users/<u> 合计
	Shared   int64                `json:"shared_bytes"`      // 目录统计：共享目录
	Sessions int                  `json:"sessions"`          // 名下工作空间数
}

func (s *Server) monitorStorageView(sessionsByUser map[string]int) monitorStorage {
	s.storage.RLock()
	report, quota, quotaAt, quotaErr := s.storage.report, s.storage.quota, s.storage.quotaAt, s.storage.quotaErr
	s.storage.RUnlock()

	view := monitorStorage{
		QuotaAt:    quotaAt,
		QuotaError: quotaErr,
		Budget:     s.cfg.GetStoragePolicy().BudgetBytes,
		ScannedAt:  report.ScannedAt,
		Partial:    report.Partial,
		Users:      []userStorage{},
	}
	rows := map[string]*userStorage{}
	row := func(name string) *userStorage {
		if r := rows[name]; r != nil {
			return r
		}
		r := &userStorage{User: name}
		rows[name] = r
		return r
	}
	owners := map[string]string{} // 缓存配额按 owner key 登记，换回用户名
	known := map[string]bool{}
	for _, u := range s.store.ListUsers() {
		row(u.Name)
		known[u.Name] = true
		if key, err := s.storageOwner(u); err == nil {
			owners[key] = u.Name
		}
	}
	if quotaAt > 0 {
		unassigned := quota.Unassigned
		view.Unassigned = &unassigned
	}
	dataRoot := filepath.Join(s.cfg.DataDir, "users")
	for _, p := range quota.Projects {
		usage := p.Usage
		view.Allocated += usage.LimitBytes
		switch p.Kind {
		case "data":
			name := filepath.Base(p.Root)
			if filepath.Dir(p.Root) != dataRoot {
				continue
			}
			r := row(name)
			r.Data = &usage
		case "cache":
			name, ok := owners[p.Owner]
			if !ok {
				// 已删除账号的缓存只登记了 owner key，查不回用户名
				name = "cache:" + shortKey(p.Owner)
			}
			row(name).Cache = &usage
		}
	}
	for name, bytes := range report.Users {
		row(name).Files = bytes
	}
	for name, bytes := range report.Shared {
		row(name).Shared = bytes
	}
	for name, r := range rows {
		r.Sessions = sessionsByUser[name]
		r.Deleted = !known[name] // 包括 cache:<key> 这种查不回名字的
		view.Users = append(view.Users, *r)
	}
	// 占用大的在上：按数据配额已用，没有配额时按目录统计
	used := func(r userStorage) int64 {
		if r.Data != nil {
			return r.Data.UsedBytes
		}
		return r.Files
	}
	sort.Slice(view.Users, func(i, j int) bool {
		a, b := view.Users[i], view.Users[j]
		if a.Deleted != b.Deleted {
			return !a.Deleted
		}
		if used(a) != used(b) {
			return used(a) > used(b)
		}
		return a.User < b.User
	})
	return view
}

func shortKey(key string) string {
	if len(key) > 8 {
		return key[:8]
	}
	return key
}
