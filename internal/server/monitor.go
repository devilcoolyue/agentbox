// 运维监控 API：后端进程与主机的实时资源占用，加上每个会话容器的 CPU/内存
// 明细。CPU 是累计计数器，无法从单次读数还原速率，所以每次请求只取一次快照并
// 缓存下来，用「本次 vs 上次请求」的差值测速——采样窗口就是前端两次轮询的真实
// 间隔（约 5 秒），请求本身不阻塞。
// /proc 读取仅在 Linux 有效（生产环境即 Linux）；其它平台文件不存在，各项优雅
// 归零而非报错。
package server

import (
	"bufio"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

// clkTck 是内核记账的时钟节拍（USER_HZ），Linux 上恒为 100。无 cgo 拿不到
// sysconf(_SC_CLK_TCK)，直接用这个约定值把 /proc 里的 tick 换算成秒。
const clkTck = 100.0

// CPU 速率靠「本次请求 vs 上次请求」的计数器差得到，采样窗口即前端两次轮询的
// 真实间隔（约 5 秒），无需在单次请求里阻塞。窗口落在下面区间外的这一帧不给
// 速率：太短多半是并发或狂刷（除以极小窗口会炸出假高），太长说明管理员离开又
// 回来（拿旧基准会平均成失真的低值）。
const (
	monMinWindow = time.Second
	monMaxWindow = 60 * time.Second
)

// monState 缓存上一次请求的计数器快照。CPU 是累计计数器，单次读数还原不出
// 速率，所以把上一帧留下来给下一帧做差。
type monState struct {
	mu   sync.Mutex
	prev *monSample
}

func newMonState() *monState { return &monState{} }

// monSample 是一次请求里同时取到的三方累计计数器。
type monSample struct {
	at    time.Time
	proc  uint64                     // 进程 utime+stime，时钟节拍
	host  cpuTimes                   // 主机 /proc/stat 聚合忙/总节拍
	stats map[string]dockerx.RawStat // 每个运行中容器的累计快照
}

// rotate 用本帧快照更新缓存，返回可用于测速的上一帧与本帧窗口。窗口无效时返回
// 0：太短则保留旧基准（等下次拉开间隔），太长则刷新基准（下一帧才准）。
func (m *monState) rotate(cur monSample) (monSample, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.prev
	if prev == nil {
		m.prev = &cur
		return monSample{}, 0
	}
	d := cur.at.Sub(prev.at)
	if d < monMinWindow {
		return monSample{}, 0 // 太近：不动基准，避免除以极小窗口
	}
	m.prev = &cur
	if d > monMaxWindow {
		return monSample{}, 0 // 太远：基准已刷新，但这帧不测速
	}
	return *prev, d
}

type monitorView struct {
	Now        int64           `json:"now"`       // 服务端当前时间(ms)，前端据此算运行时长
	Window     int64           `json:"window_ms"` // 采样窗口，展示用
	Process    processStat     `json:"process"`
	Host       hostStat        `json:"host"`
	Summary    monitorSummary  `json:"summary"`
	Containers []containerStat `json:"containers"`
}

type processStat struct {
	CPUPercent float64 `json:"cpu_percent"` // 单核百分比，多核可超过 100
	RSS        uint64  `json:"rss"`         // 常驻内存，bytes
	HeapAlloc  uint64  `json:"heap_alloc"`  // Go 堆在用，bytes
	HeapSys    uint64  `json:"heap_sys"`    // Go 向系统申请，bytes
	Goroutines int     `json:"goroutines"`
	Uptime     int64   `json:"uptime_ms"`
}

type hostStat struct {
	CPUCount   int     `json:"cpu_count"`
	CPUPercent float64 `json:"cpu_percent"` // 全核合计占用 0-100
	Load1      float64 `json:"load1"`
	MemTotal   uint64  `json:"mem_total"`
	MemUsed    uint64  `json:"mem_used"`
	DiskTotal  uint64  `json:"disk_total"` // data_dir 所在文件系统容量
	DiskUsed   uint64  `json:"disk_used"`  // 已用（含系统保留块，近似）
}

type monitorSummary struct {
	Total      int     `json:"total"`       // 会话容器总数
	Running    int     `json:"running"`     // 运行中的
	CPUPercent float64 `json:"cpu_percent"` // 运行中容器 CPU 合计
	MemUsage   uint64  `json:"mem_usage"`   // 运行中容器内存合计
}

type containerStat struct {
	SessionID  string  `json:"session_id"`
	Name       string  `json:"name"`
	User       string  `json:"user"`
	Agent      string  `json:"agent"`
	AccountID  string  `json:"account_id"`
	Running    bool    `json:"running"`
	CreatedAt  int64   `json:"created_at"`  // 会话创建时间(ms)
	StartedAt  int64   `json:"started_at"`  // 容器本次启动时间(ms)，0=未运行/未知
	CPUPercent float64 `json:"cpu_percent"` // 单核百分比
	MemUsage   uint64  `json:"mem_usage"`
	MemLimit   uint64  `json:"mem_limit"`
	Pids       uint64  `json:"pids"`
}

// handleMonitor 采样一次窗口并返回后端进程、主机与全部会话容器的资源快照。
func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessions := s.store.All()

	// 先分辨哪些容器真的在跑：停机容器没有 stats 可读，但仍要在列表里列出。
	type meta struct {
		sess      store.Session
		running   bool
		startedAt time.Time
	}
	metas := make([]meta, 0, len(sessions))
	var runningIDs []string
	for _, sess := range sessions {
		m := meta{sess: sess}
		if sess.ContainerID != "" {
			m.running, m.startedAt = s.dock.ContainerState(ctx, sess.ContainerID)
		}
		if m.running {
			runningIDs = append(runningIDs, sess.ContainerID)
		}
		metas = append(metas, m)
	}

	// 只取一次瞬时计数器；速率靠与上一帧做差（见 monState.rotate）。
	cur := monSample{
		at:    time.Now(),
		proc:  readProcCPUTicks(),
		host:  readHostCPU(),
		stats: s.dock.StatSnapshots(ctx, runningIDs),
	}
	prev, window := s.mon.rotate(cur)
	rate := window > 0 // 本帧窗口有效，可给 CPU 速率

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	memTotal, memUsed := readMeminfo()
	diskTotal, diskFree := readDiskUsage(s.cfg.DataDir)

	view := monitorView{
		Now:    cur.at.UnixMilli(),
		Window: window.Milliseconds(), // 0 = 首帧/窗口无效，前端据此显示「测量中」
		Process: processStat{
			RSS:        readRSS(),
			HeapAlloc:  mem.HeapAlloc,
			HeapSys:    mem.Sys,
			Goroutines: runtime.NumGoroutine(),
			Uptime:     time.Since(s.startedAt).Milliseconds(),
		},
		Host: hostStat{
			CPUCount:  runtime.NumCPU(),
			Load1:     readLoad1(),
			MemTotal:  memTotal,
			MemUsed:   memUsed,
			DiskTotal: diskTotal,
			DiskUsed:  diskTotal - diskFree,
		},
		Containers: []containerStat{},
	}
	if rate {
		view.Process.CPUPercent = procCPUPercent(prev.proc, cur.proc, window)
		view.Host.CPUPercent = hostCPUPercent(prev.host, cur.host)
	}

	for _, m := range metas {
		row := containerStat{
			SessionID: m.sess.ID,
			Name:      m.sess.Name,
			User:      m.sess.User,
			Agent:     m.sess.Agent,
			AccountID: m.sess.AccountID,
			Running:   m.running,
			CreatedAt: m.sess.CreatedAt.UnixMilli(),
		}
		view.Summary.Total++
		if m.running {
			view.Summary.Running++
			if !m.startedAt.IsZero() {
				row.StartedAt = m.startedAt.UnixMilli()
			}
			// 内存/进程数是瞬时值，首帧就有；CPU% 要等上一帧同容器的快照。
			if b := cur.stats[m.sess.ContainerID]; b.OK {
				row.MemUsage = b.MemUsage
				row.MemLimit = b.MemLimit
				row.Pids = b.Pids
				view.Summary.MemUsage += b.MemUsage
				if rate {
					if a, ok := prev.stats[m.sess.ContainerID]; ok && a.OK {
						row.CPUPercent = containerCPUPercent(a, b)
						view.Summary.CPUPercent += row.CPUPercent
					}
				}
			}
		}
		view.Containers = append(view.Containers, row)
	}
	// 运行中的排在前面，各自按创建时间倒序，最近建的会话在上。
	sort.SliceStable(view.Containers, func(i, j int) bool {
		a, b := view.Containers[i], view.Containers[j]
		if a.Running != b.Running {
			return a.Running
		}
		return a.CreatedAt > b.CreatedAt
	})

	writeJSON(w, http.StatusOK, view)
}

// containerCPUPercent 把两次累计 CPU 纳秒差除以窗口挂钟纳秒，得到单核百分比
// （多核容器可超过 100，与 docker stats 语义一致）。
func containerCPUPercent(a, b dockerx.RawStat) float64 {
	wall := b.Read.Sub(a.Read).Seconds()
	if wall <= 0 || b.CPUTotal < a.CPUTotal {
		return 0
	}
	return float64(b.CPUTotal-a.CPUTotal) / (wall * 1e9) * 100
}

// --- /proc 采样助手（Linux 专用，缺文件即返回零值） ---

// readProcCPUTicks 读 /proc/self/stat 的 utime+stime（时钟节拍）。comm 字段带
// 括号且可能含空格，按最后一个 ')' 切开再取其后的字段最稳妥。
func readProcCPUTicks() uint64 {
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	s := string(raw)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return 0
	}
	// ')' 之后依次是 state ppid ... utime(第 12) stime(第 13)，0 基下标 11、12。
	f := strings.Fields(s[i+2:])
	if len(f) < 13 {
		return 0
	}
	utime, _ := strconv.ParseUint(f[11], 10, 64)
	stime, _ := strconv.ParseUint(f[12], 10, 64)
	return utime + stime
}

func procCPUPercent(t0, t1 uint64, wall time.Duration) float64 {
	sec := wall.Seconds()
	if sec <= 0 || t1 < t0 {
		return 0
	}
	return (float64(t1-t0) / clkTck) / sec * 100
}

// cpuTimes 是 /proc/stat 首行聚合的忙/总节拍。
type cpuTimes struct{ busy, total uint64 }

func readHostCPU() cpuTimes {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		// user nice system idle iowait irq softirq steal guest guest_nice
		fields := strings.Fields(line)[1:]
		var total, idle uint64
		for i, v := range fields {
			n, _ := strconv.ParseUint(v, 10, 64)
			total += n
			if i == 3 || i == 4 { // idle + iowait 记作空闲
				idle += n
			}
		}
		return cpuTimes{busy: total - idle, total: total}
	}
	return cpuTimes{}
}

func hostCPUPercent(a, b cpuTimes) float64 {
	if b.total <= a.total || b.busy < a.busy {
		return 0
	}
	return float64(b.busy-a.busy) / float64(b.total-a.total) * 100
}

// readMeminfo 返回主机内存总量与已用（total - available），单位 bytes。
func readMeminfo() (total, used uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var memTotal, memAvail uint64
	var haveAvail bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64) // kB
		switch fields[0] {
		case "MemTotal:":
			memTotal = v << 10
		case "MemAvailable:":
			memAvail, haveAvail = v<<10, true
		}
	}
	if !haveAvail || memAvail > memTotal {
		return memTotal, 0
	}
	return memTotal, memTotal - memAvail
}

// readRSS 读 /proc/self/statm 第二个字段（常驻页数）换算成 bytes。
func readRSS() uint64 {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0
	}
	pages, _ := strconv.ParseUint(fields[1], 10, 64)
	return pages * uint64(os.Getpagesize())
}

// readDiskUsage returns total and available bytes of the filesystem holding
// path. Statfs works on Linux and macOS; the server never builds for Windows.
func readDiskUsage(path string) (total, free uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bsize := uint64(st.Bsize)
	return st.Blocks * bsize, st.Bavail * bsize
}

func readLoad1() float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 1 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}
