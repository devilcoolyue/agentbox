package server

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"agentbox/internal/store"
)

// 用量变更推送：使用记录页与终端顶栏「本空间已花」原本只靠轮询（30 秒 / 15 秒），
// 终端消耗明明一秒内就落了库，页面上却要等到下一次轮询。做法照 cc-switch 的
// usage-log-recorded：任何一次用量写入提交后（对话结算、Claude 逐条结算、终端补记）
// 由 store 的钩子通知这里，经 /api/usage/live 推给开着页面的浏览器，浏览器收到再
// 走 /usage/events 重新读。
//
// 推送只带空间 ID、不带金额与 token：读数据仍走原接口，可见范围照旧由那边判断。
// 管理员能收到所有用户的变更，普通用户只收到自己的。
const (
	// usageLiveDebounce 合并一小段时间内的多次写入：CLI 一次回复会落好几行，
	// 一趟扫描可能覆盖好几个空间，合成一条通知，免得浏览器连着重读。
	usageLiveDebounce = 200 * time.Millisecond
	// usageLiveMaxSessions 是一条通知最多列出的空间数；超过就不列（sessions 为
	// null），浏览器按「哪个空间都可能变了」处理。
	usageLiveMaxSessions = 64
	// closeUsageAuth：登录失效后关闭推送连接。前端见 4000–4999 不再重连。
	closeUsageAuth = 4001
)

type usageLiveHub struct {
	mu   sync.Mutex
	subs map[*usageLiveSub]struct{}
}

func newUsageLiveHub() *usageLiveHub {
	return &usageLiveHub{subs: map[*usageLiveSub]struct{}{}}
}

// usageLiveSub 是一条推送连接攒着、还没发出去的变更。
type usageLiveSub struct {
	user   string
	signal chan struct{} // 容量 1：有待发变更时非空，多次写入自然合并

	mu       sync.Mutex
	admin    bool
	sessions map[string]bool
	overflow bool
}

func (h *usageLiveHub) subscribe(user string, admin bool) *usageLiveSub {
	sub := &usageLiveSub{user: user, admin: admin, signal: make(chan struct{}, 1), sessions: map[string]bool{}}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

func (h *usageLiveHub) unsubscribe(sub *usageLiveSub) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

// publish 跑在写用量的那条协程上（store 提交之后），只做内存标记，不碰网络。
func (h *usageLiveHub) publish(writes []store.UsageWrite) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		sub.offer(writes)
	}
}

func (sub *usageLiveSub) offer(writes []store.UsageWrite) {
	hit := false
	sub.mu.Lock()
	for _, w := range writes {
		if !sub.admin && w.User != sub.user {
			continue
		}
		hit = true
		switch {
		case w.SessionID == "" || sub.sessions[w.SessionID]:
		case len(sub.sessions) >= usageLiveMaxSessions:
			sub.overflow = true
		default:
			sub.sessions[w.SessionID] = true
		}
	}
	sub.mu.Unlock()
	if hit {
		select {
		case sub.signal <- struct{}{}:
		default:
		}
	}
}

func (sub *usageLiveSub) setAdmin(admin bool) {
	sub.mu.Lock()
	sub.admin = admin
	sub.mu.Unlock()
}

// take 取走攒下的空间列表；nil 表示超过上限、不逐个列出。
func (sub *usageLiveSub) take() []string {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	var ids []string
	if !sub.overflow {
		ids = make([]string, 0, len(sub.sessions))
		for id := range sub.sessions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	sub.sessions = map[string]bool{}
	sub.overflow = false
	return ids
}

type usageLiveNotice struct {
	Type     string   `json:"type"`
	Sessions []string `json:"sessions"`
}

// handleUsageLive 是用量变更的推送连接，服务端只写不读。心跳周期里顺带复查
// 登录：令牌失效就关掉（不能让退出登录的页面继续收别人的空间 ID），角色变了
// 就跟着调整能收到的范围。
func (s *Server) handleUsageLive(w http.ResponseWriter, r *http.Request) {
	user, token := reqUser(r), bearerToken(r)
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	release, ok := s.track(func() { _ = conn.Close() })
	if !ok {
		return
	}
	defer release()
	sub := s.usageLive.subscribe(user.Name, user.Role == store.RoleAdmin)
	defer s.usageLive.unsubscribe(sub)

	// 读循环只为收 pong 与关闭帧；浏览器不会发数据过来。
	conn.SetReadLimit(1 << 10)
	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()

	ping := time.NewTicker(wsPingPeriod)
	defer ping.Stop()
	for {
		select {
		case <-gone:
			return
		case <-ping.C:
			current, ok := s.store.TokenUser(token)
			if !ok || current.Name != user.Name {
				closeWithReason(conn, closeUsageAuth, "登录已失效")
				return
			}
			sub.setAdmin(current.Role == store.RoleAdmin)
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)); err != nil {
				return
			}
		case <-sub.signal:
			wait := time.NewTimer(usageLiveDebounce)
			select {
			case <-gone:
				wait.Stop()
				return
			case <-wait.C:
			}
			conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := conn.WriteJSON(usageLiveNotice{Type: "usage", Sessions: sub.take()}); err != nil {
				return
			}
		}
	}
}
