package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
	"agentbox/internal/tunnel"
)

func fixtureTokenCount(t *testing.T, s *Server) int {
	t.Helper()
	address := (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(s.cfg.DataDir, "state.db")), RawQuery: "mode=ro"}).String()
	database, err := sql.Open("sqlite", address)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM tokens").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func pairingFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, _, handler := clientTestServer(t)
	if err := s.cfg.ApplySettings(config.SettingsPatch{Tunnel: &config.TunnelConfig{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	s.pairs = newPairStore()
	if err := s.store.SetPassword("alice", hashPassword("synthetic-old-password")); err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateToken("client-fixture-root", "root"); err != nil {
		t.Fatal(err)
	}
	return s, handler
}

func pairRoutes(channel string) (string, string) {
	if channel == "desktop" {
		return "/api/clients/pair", "/api/clients/pair/redeem"
	}
	return "/api/tunnel/pair", "/api/tunnel/pair/redeem"
}

func issueFixturePair(t *testing.T, handler http.Handler, channel string) string {
	t.Helper()
	issue, _ := pairRoutes(channel)
	w := clientRequest(handler, "alice", http.MethodPost, issue, `{"origin":"https://pairing.invalid"}`)
	var response struct {
		Code string `json:"code"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Code == "" {
		t.Fatal("pair issue", w.Code)
	}
	if channel == "tunnel" {
		decoded, err := tunnel.DecodePairCode(response.Code)
		if err != nil {
			t.Fatal(err)
		}
		return decoded.Code
	}
	return response.Code
}

func TestClientPairingCannotOutlivePasswordOrAccountIdentity(t *testing.T) {
	for _, channel := range []string{"desktop", "tunnel"} {
		for _, change := range []string{"self_password", "admin_password", "delete_and_recreate", "recreate_same_hash"} {
			t.Run(channel+"/"+change, func(t *testing.T) {
				s, handler := pairingFixture(t)
				original, _ := s.store.GetUser("alice")
				code := issueFixturePair(t, handler, channel)
				switch change {
				case "self_password":
					w := clientRequest(handler, "alice", "POST", "/api/me/password", `{"old_password":"synthetic-old-password","new_password":"synthetic-new-password"}`)
					if w.Code != 200 {
						t.Fatal("change own password", w.Code)
					}
					if user, ok := s.store.TokenUser("client-fixture-alice"); !ok || !verifyPassword(user.PassHash, "synthetic-new-password") {
						t.Fatal("self reset lost the current login")
					}
				case "admin_password":
					w := clientRequest(handler, "root", "POST", "/api/users/alice/password", `{"password":"synthetic-new-password"}`)
					if w.Code != 200 {
						t.Fatal("admin password reset", w.Code)
					}
					if _, ok := s.store.TokenUser("client-fixture-alice"); ok {
						t.Fatal("admin reset left the target logged in")
					}
				case "delete_and_recreate", "recreate_same_hash":
					w := clientRequest(handler, "root", "DELETE", "/api/users/alice", "")
					if w.Code != 200 {
						t.Fatal("delete user", w.Code)
					}
					if change == "recreate_same_hash" {
						if err := s.store.CreateUser(store.User{Name: "alice", Role: store.RoleUser, PassHash: original.PassHash, CreatedAt: original.CreatedAt.Add(time.Nanosecond)}); err != nil {
							t.Fatal(err)
						}
					} else {
						w = clientRequest(handler, "root", "POST", "/api/users", `{"username":"alice","password":"synthetic-new-password"}`)
						if w.Code != 201 {
							t.Fatal("recreate user", w.Code)
						}
					}
				}
				before := fixtureTokenCount(t, s)
				_, redeem := pairRoutes(channel)
				w := clientRequest(handler, "", "POST", redeem, `{"code":"`+code+`"}`)
				if w.Code != 401 {
					t.Fatalf("old pairing credential survived %s: HTTP %d", change, w.Code)
				}
				if after := fixtureTokenCount(t, s); after != before {
					t.Fatal("invalid pair created a token", before, after)
				}
			})
		}
	}
}

func TestPairingConcurrentRedemptionIssuesOnlyOneToken(t *testing.T) {
	for _, channel := range []string{"desktop", "tunnel"} {
		t.Run(channel, func(t *testing.T) {
			s, handler := pairingFixture(t)
			code := issueFixturePair(t, handler, channel)
			before := fixtureTokenCount(t, s)
			_, redeem := pairRoutes(channel)
			results := make(chan *httptest.ResponseRecorder, 12)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for i := range 12 {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					request := httptest.NewRequest("POST", redeem, strings.NewReader(`{"code":"`+code+`"}`))
					request.RemoteAddr = fmt.Sprintf("198.51.100.%d:1234", i+1)
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					results <- response
				}()
			}
			close(start)
			workers.Wait()
			close(results)
			successes := 0
			for response := range results {
				if response.Code == 200 {
					successes++
					var result struct{ Token, User string }
					if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.User != "alice" {
						t.Fatal("invalid identity response")
					}
					user, ok := s.store.TokenUser(result.Token)
					if !ok || user.Name != "alice" {
						t.Fatal("pair did not issue an authenticated token")
					}
				} else if response.Code != 401 {
					t.Fatal("unexpected redemption status", response.Code)
				}
			}
			if successes != 1 || fixtureTokenCount(t, s) != before+1 {
				t.Fatal("concurrent code redemption minted multiple tokens", successes)
			}
		})
	}
}

func TestPairingIssueRejectsStaleAuthenticatedRequest(t *testing.T) {
	for _, channel := range []string{"desktop", "tunnel"} {
		for _, change := range []string{"password", "recreated"} {
			t.Run(channel+"/"+change, func(t *testing.T) {
				s, _ := pairingFixture(t)
				authenticated, ok := s.store.TokenUser("client-fixture-alice")
				if !ok {
					t.Fatal("fixture authentication failed")
				}
				if change == "password" {
					if err := s.store.SetPassword("alice", "synthetic-changed-hash"); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := s.store.DeleteUser("alice"); err != nil {
						t.Fatal(err)
					}
					if err := s.store.CreateUser(store.User{Name: "alice", Role: store.RoleUser, PassHash: authenticated.PassHash, CreatedAt: authenticated.CreatedAt.Add(time.Nanosecond)}); err != nil {
						t.Fatal(err)
					}
				}
				issue, _ := pairRoutes(channel)
				request := httptest.NewRequest("POST", issue, strings.NewReader(`{"origin":"https://pairing.invalid"}`))
				request = request.WithContext(context.WithValue(request.Context(), ctxUser, authenticated))
				response := httptest.NewRecorder()
				if channel == "desktop" {
					s.handleClientPair(response, request)
				} else {
					s.handleTunnelPair(response, request)
				}
				if response.Code != 401 {
					t.Fatal("stale request acquired replacement authentication", response.Code)
				}
				if len(s.pairs.codes) != 0 || len(s.clientPairStore().codes) != 0 {
					t.Fatal("stale request issued a delegation")
				}
			})
		}
	}
}

func TestPairStoreReissueAtCapacityKeepsOnlyNewCode(t *testing.T) {
	p := newPairStore()
	first, _ := p.issue("alice")
	for i := 1; i < pairMaxOutstand; i++ {
		if _, ok := p.issue(fmt.Sprintf("user-%03d", i)); !ok {
			t.Fatal("fixture capacity", i)
		}
	}
	if _, ok := p.issue("another-user"); ok {
		t.Fatal("capacity bypassed")
	}
	second, ok := p.issue("alice")
	if !ok {
		t.Fatal("a user's existing code must be replaceable at capacity")
	}
	if _, ok := p.redeem(first); ok {
		t.Fatal("superseded code remains valid")
	}
	if user, ok := p.redeem(second); !ok || user != "alice" {
		t.Fatal("replacement code unavailable")
	}
	if len(p.codes) != pairMaxOutstand-1 {
		t.Fatal("other users' outstanding codes changed")
	}
}

func TestPasswordLoginRetainsWireContractAfterPasswordResetAndRecreation(t *testing.T) {
	s, handler := pairingFixture(t)
	login := func(password string, want int) string {
		t.Helper()
		body, err := json.Marshal(map[string]string{"username": "alice", "password": password})
		if err != nil {
			t.Fatal(err)
		}
		response := clientRequest(handler, "", "POST", "/api/login", string(body))
		if response.Code != want {
			t.Fatal("password login status", response.Code, "want", want)
		}
		var result struct{ Token, User, Role string }
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if want == 200 {
			user, ok := s.store.TokenUser(result.Token)
			if !ok || user.Name != "alice" || result.User != "alice" || result.Role != store.RoleUser {
				t.Fatal("successful password login changed its token/user/role contract")
			}
		} else if result.Token != "" {
			t.Fatal("failed password login disclosed a token")
		}
		return result.Token
	}
	oldToken := login("synthetic-old-password", 200)
	response := clientRequest(handler, "root", "POST", "/api/users/alice/password", `{"password":"synthetic-new-password"}`)
	if response.Code != 200 {
		t.Fatal("admin reset", response.Code)
	}
	if _, ok := s.store.TokenUser(oldToken); ok {
		t.Fatal("reset left the previous login authenticated")
	}
	login("synthetic-old-password", 401)
	newToken := login("synthetic-new-password", 200)
	if response := clientRequest(handler, "root", "DELETE", "/api/users/alice", ""); response.Code != 200 {
		t.Fatal("delete user", response.Code)
	}
	if response := clientRequest(handler, "root", "POST", "/api/users", `{"username":"alice","password":"replacement-password"}`); response.Code != 201 {
		t.Fatal("recreate user", response.Code)
	}
	if _, ok := s.store.TokenUser(newToken); ok {
		t.Fatal("replacement account inherited the old login")
	}
	login("synthetic-new-password", 401)
	login("replacement-password", 200)
	if err := s.store.CreateToken("other-root-login", "root"); err != nil {
		t.Fatal(err)
	}
	if response := clientRequest(handler, "root", "POST", "/api/users/root/password", `{"password":"admin-new-password"}`); response.Code != 200 {
		t.Fatal("admin self reset", response.Code)
	}
	if _, ok := s.store.TokenUser("client-fixture-root"); !ok {
		t.Fatal("admin self reset revoked its current token")
	}
	if _, ok := s.store.TokenUser("other-root-login"); ok {
		t.Fatal("admin self reset retained another token")
	}
}
