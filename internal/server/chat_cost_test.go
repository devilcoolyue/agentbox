package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
	"github.com/gorilla/websocket"
)

func TestChatCostMatchesSettlementLiveAndHistory(t *testing.T) {
	for _, tc := range []struct {
		name, agent, line, source string
		pricing                   bool
		cost                      int64
		partial                   bool
	}{
		{"codex cached and reasoning", "codex", `{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":100,"reasoning_output_tokens":90}}`, "table", true, 1800, false},
		{"claude submodels", "claude", `{"type":"assistant","message":{"id":"msg-main","model":"main","usage":{"input_tokens":100}}}
{"type":"assistant","message":{"id":"msg-child","model":"child","usage":{"input_tokens":20}}}`, "table", true, 240, false},
		{"claude message pricing", "claude", `{"type":"assistant","message":{"id":"msg-main","model":"main","usage":{"input_tokens":100,"output_tokens":100}}}`, "table", true, 1200, false},
		{"codex unpriced", "codex", `{"type":"turn.completed","usage":{"input_tokens":100}}`, "unpriced", false, 0, true},
		{"claude unpriced", "claude", `{"type":"assistant","message":{"id":"msg-main","model":"main","usage":{"input_tokens":100}}}`, "unpriced", false, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sess := newTestServer(t)
			if err := s.store.Put(sess); err != nil {
				t.Fatal(err)
			}
			if tc.pricing {
				s.cfg.Pricing = map[string]config.ModelPrice{tc.agent: {TokenRates: config.TokenRates{Input: 2, Output: 10, CacheRead: 0.5}}}
			}
			if _, err := s.store.Grant(sess.User, 100000, "grant", "", "admin"); err != nil {
				t.Fatal(err)
			}
			s.chat = newChatManager(s)
			room := s.chat.room(sess.ID)
			room.recordChat(logEntry{Kind: "user", Text: "fixture", Turn: &chatTurnMetadata{ID: "turn", Model: "fixture"}}, "user_message", "")
			tid := s.activeThread(sess)
			base := store.UsageEvent{User: sess.User, SessionID: sess.ID, ThreadID: tid, TurnID: "turn", Agent: tc.agent, Model: "fixture", Kind: store.UsageKindChat}
			var tally usageTally
			for _, line := range strings.Split(tc.line, "\n") {
				tally.Observe(base, []byte(line))
				if tc.agent == "claude" {
					tally.Observe(base, []byte(line))
				}
			} // Repeated message IDs must not double-charge.
			if room.flushUsage(&tally, time.Second) == 0 {
				t.Fatal("no settlement")
			}
			if room.flushUsage(&tally, time.Second) != 0 {
				t.Fatal("duplicate settlement")
			}
			// Unrelated title cost must not appear in the answer.
			title := base
			title.Kind = store.UsageKindTitle
			title.CostMicroUSD = 7
			if err := s.store.InsertUsage(title); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				u := websocket.Upgrader{}
				conn, err := u.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				cw := &connWriter{c: conn}
				room.attach(cw)
				defer room.detach(cw)
				room.publishTurnCost(tid, "turn")
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var live struct {
				Type string             `json:"type"`
				Cost store.ChatTurnCost `json:"cost"`
			}
			if err := conn.ReadJSON(&live); err != nil {
				t.Fatal(err)
			}
			want := store.ChatTurnCost{TurnID: "turn", CostMicroUSD: tc.cost, Source: tc.source, Partial: tc.partial}
			if live.Type != "turn_cost" || live.Cost != want {
				t.Fatalf("got %+v want %+v", live, want)
			}
			s.cfg.Pricing = map[string]config.ModelPrice{tc.agent: {TokenRates: config.TokenRates{Input: 9999, Output: 9999}}}
			w := httptest.NewRecorder()
			s.handleHistory(w, httptest.NewRequest("GET", "/history", nil), sess)
			var history struct {
				Costs map[string]store.ChatTurnCost `json:"costs"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &history); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(history.Costs["turn"], want) {
				t.Fatalf("historical amount changed: %s", w.Body.String())
			}
			quota, _ := s.store.GetQuota(sess.User)
			if quota.BalanceMicroUSD != 100000-tc.cost-7 {
				t.Fatal("display path charged again", quota)
			}
		})
	}
}
