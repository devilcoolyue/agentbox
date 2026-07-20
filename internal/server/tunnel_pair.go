package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agentbox/internal/tunnel"
)

// Pairing removes the worst part of setting up a link: typing a server URL,
// username and password into a client on another machine. The user clicks
// "generate" in the browser, copies one string into the abox-link app, and the
// app redeems it for its own session token.
//
// The code is single-use and short-lived, and it never carries the browser's
// own token — a code that leaks is worth little and expires by itself.

const (
	pairCodeTTL     = 10 * time.Minute
	pairMaxOutstand = 200 // cap on unredeemed codes, so nothing can grow unbounded
)

type pairEntry struct {
	user    string
	expires time.Time
}

// pairStore holds unredeemed pairing codes. Memory-only on purpose: a code
// outliving a server restart buys nothing and only widens the window.
type pairStore struct {
	mu    sync.Mutex
	codes map[string]pairEntry
}

func newPairStore() *pairStore { return &pairStore{codes: map[string]pairEntry{}} }

// issue mints a code for user, replacing any the user already had — clicking
// "generate" twice should not leave the first code live.
func (p *pairStore) issue(user string) (string, bool) {
	code := randomPairCode()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	if len(p.codes) >= pairMaxOutstand {
		return "", false
	}
	for c, e := range p.codes {
		if e.user == user {
			delete(p.codes, c)
		}
	}
	p.codes[code] = pairEntry{user: user, expires: time.Now().Add(pairCodeTTL)}
	return code, true
}

// redeem consumes a code, returning the user it was issued to. A code works
// exactly once.
func (p *pairStore) redeem(code string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()

	// Compare in constant time against every candidate rather than indexing the
	// map directly, so redemption timing cannot confirm a guessed prefix.
	var user string
	var found string
	for c, e := range p.codes {
		if subtle.ConstantTimeCompare([]byte(c), []byte(code)) == 1 {
			user, found = e.user, c
		}
	}
	if found == "" {
		return "", false
	}
	delete(p.codes, found)
	return user, true
}

func (p *pairStore) sweepLocked() {
	now := time.Now()
	for c, e := range p.codes {
		if now.After(e.expires) {
			delete(p.codes, c)
		}
	}
}

func randomPairCode() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// handleTunnelPair issues a pairing code for the calling user.
func (s *Server) handleTunnelPair(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.GetTunnel().Enabled {
		writeErr(w, http.StatusForbidden, "内网隧道功能未启用")
		return
	}
	var req struct {
		Origin string `json:"origin"` // the browser's own location.origin
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)

	origin := pairOrigin(req.Origin, r)
	if origin == "" {
		writeErr(w, http.StatusBadRequest, "无法确定服务器地址")
		return
	}
	code, ok := s.pairs.issue(reqUser(r).Name)
	if !ok {
		writeErr(w, http.StatusTooManyRequests, "待用配对码过多，请稍后再试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":       tunnel.EncodePairCode(origin, code),
		"expires_in": int(pairCodeTTL.Seconds()),
	})
}

// pairOrigin decides which base URL to bake into the code. The browser knows
// the address the user actually reaches this server on — including the scheme a
// terminating proxy may hide — so its location.origin is preferred, and the
// request is only a fallback. Taking it from an authenticated user's own page
// is safe: the worst they can do is point their own client elsewhere.
func pairOrigin(claimed string, r *http.Request) string {
	if u, err := url.Parse(strings.TrimSpace(claimed)); err == nil &&
		u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
		return u.Scheme + "://" + u.Host
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	if host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + host
}

// handleTunnelPairRedeem exchanges a pairing code for a session token. It is
// deliberately unauthenticated — the code *is* the credential, which is why it
// is single-use and expires.
func (s *Server) handleTunnelPairRedeem(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.GetTunnel().Enabled {
		writeErr(w, http.StatusForbidden, "内网隧道功能未启用")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	user, ok := s.pairs.redeem(strings.TrimSpace(req.Code))
	if !ok {
		time.Sleep(400 * time.Millisecond) // 拖慢在线爆破
		writeErr(w, http.StatusUnauthorized, "配对码无效或已过期，请重新生成")
		return
	}
	// The account may have been deleted between issue and redeem.
	if _, exists := s.store.GetUser(user); !exists {
		writeErr(w, http.StatusUnauthorized, "账号不存在")
		return
	}

	tok := newToken()
	if err := s.store.CreateToken(tok, user); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": user, "token": tok})
}
