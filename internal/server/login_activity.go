// 管理员「登录与活跃」面板：登录记录与用户最后一次在终端页面人为操作的时间。
// 读接口全部走 s.admin，普通用户拿不到这些数据，也没有任何接口回显自己的记录。
package server

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"agentbox/internal/store"
)

const (
	// termActivityMinGap caps database writes per terminal connection, whatever
	// the browser sends. The page reports at most every 15s (term-activity.ts).
	termActivityMinGap = 10 * time.Second

	loginEventsDefaultLimit = 50
	loginEventsMaxLimit     = 200
	loginUserAgentMaxRunes  = 256
)

// recordLogin notes an issued login token. Failure only logs: being unable to
// keep the record (a full disk, say) must not lock anyone out.
func (s *Server) recordLogin(r *http.Request, user, method string) {
	err := s.store.RecordLogin(store.LoginEvent{
		User: user, At: time.Now().UnixMilli(), Method: method,
		IP: clientIP(r), UserAgent: loginUserAgent(r),
	})
	if err != nil {
		log.Printf("login record for %q: %v", user, err)
	}
}

// loginUserAgent keeps the client-chosen header to a bounded, printable string.
func loginUserAgent(r *http.Request) string {
	clean := strings.Map(func(c rune) rune {
		if c == unicode.ReplacementChar || !unicode.IsPrint(c) {
			return -1
		}
		return c
	}, strings.ToValidUTF8(r.UserAgent(), ""))
	if runes := []rune(clean); len(runes) > loginUserAgentMaxRunes {
		clean = string(runes[:loginUserAgentMaxRunes])
	}
	return clean
}

// noteTermActivity records a human action reported over a terminal connection,
// off the read loop so a busy database never delays the keystrokes behind it.
// The user is whoever authenticated this connection.
func (s *Server) noteTermActivity(user string, at time.Time) {
	s.spawn(func() {
		if err := s.store.TouchTerminalActivity(user, at); err != nil {
			log.Printf("terminal activity for %q: %v", user, err)
		}
	})
}

func (s *Server) handleUserActivity(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUserActivity()
	if err != nil {
		log.Printf("user activity: %v", err)
		writeErr(w, http.StatusInternalServerError, "无法读取用户活跃记录")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) handleLoginEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := loginEventsDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > loginEventsMaxLimit {
			writeErr(w, http.StatusBadRequest, "limit 不合法")
			return
		}
		limit = n
	}
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "before 不合法")
			return
		}
		before = n
	}
	events, err := s.store.LoginEvents(q.Get("user"), before, limit+1)
	if err != nil {
		log.Printf("login events: %v", err)
		writeErr(w, http.StatusInternalServerError, "无法读取登录记录")
		return
	}
	more := len(events) > limit
	if more {
		events = events[:limit]
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "has_more": more})
}
