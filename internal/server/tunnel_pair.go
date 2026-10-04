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

	"agentbox/internal/store"
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
	// This private memory-only proof is never returned or persisted. A password
	// reset or same-name replacement account invalidates outstanding codes.
	principal store.User
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
	return p.issueUser(store.User{Name: user})
}

func (p *pairStore) issueUser(user store.User) (string, bool) {
	code := randomPairCode()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	for c, e := range p.codes {
		if e.user == user.Name {
			delete(p.codes, c)
		}
	}
	if len(p.codes) >= pairMaxOutstand {
		return "", false
	}
	p.codes[code] = pairEntry{user: user.Name, principal: user, expires: time.Now().Add(pairCodeTTL)}
	return code, true
}

// redeem consumes a code, returning the user it was issued to. A code works
// exactly once.
func (p *pairStore) redeem(code string) (string, bool) {
	entry, ok := p.redeemEntry(code)
	return entry.user, ok
}

func (p *pairStore) redeemEntry(code string) (pairEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()

	// Compare in constant time against every candidate rather than indexing the
	// map directly, so redemption timing cannot confirm a guessed prefix.
	var entry pairEntry
	var found string
	for c, e := range p.codes {
		if subtle.ConstantTimeCompare([]byte(c), []byte(code)) == 1 {
			entry, found = e, c
		}
	}
	if found == "" {
		return pairEntry{}, false
	}
	delete(p.codes, found)
	return entry, true
}

// Recheck the identity authenticated by middleware before creating a new
// delegation. An in-flight request must not acquire a replacement account.
func (s *Server) authenticatedPairUser(r *http.Request) (store.User, bool) {
	authenticated := reqUser(r)
	current, exists := s.store.GetUser(authenticated.Name)
	return current, exists && !current.CreatedAt.IsZero() && current.CreatedAt.Equal(authenticated.CreatedAt) && current.PassHash == authenticated.PassHash
}

func (s *Server) redeemPairing(pairs *pairStore, code string) (store.User, string, bool, error) {
	entry, valid := pairs.redeemEntry(code)
	if !valid {
		return store.User{}, "", false, nil
	}
	current, exists := s.store.GetUser(entry.user)
	if !exists {
		return store.User{}, "", false, nil
	}
	token := newToken()
	valid, err := s.store.CreateTokenIfUserUnchanged(token, entry.principal)
	if err != nil || !valid {
		return store.User{}, "", false, err
	}
	return current, token, true, nil
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
	user, ok := s.authenticatedPairUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "登录状态已改变，请重新登录")
		return
	}
	code, ok := s.pairs.issueUser(user)
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

	user, tok, ok, err := s.redeemPairing(s.pairs, strings.TrimSpace(req.Code))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建登录失败，请重新配对")
		return
	}
	if !ok {
		time.Sleep(400 * time.Millisecond) // 拖慢在线爆破
		writeErr(w, http.StatusUnauthorized, "配对码无效或已过期，请重新生成")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": user.Name, "token": tok})
}
