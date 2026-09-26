package linkapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agentbox/internal/netaccess"
	"github.com/gorilla/websocket"
)

func TestTransparentCapabilityNegotiation(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "old-server", true: "new-server"}[supported], func(t *testing.T) {
			seen := make(chan []string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get(netaccess.RulesHeader))
				var rules []string
				if err != nil || json.Unmarshal(raw, &rules) != nil || r.Header.Get(netaccess.CapabilityHeader) != netaccess.Protocol {
					http.Error(w, "bad rules", 400)
					return
				}
				seen <- rules
				hdr := http.Header{}
				if supported {
					hdr.Set(netaccess.CapabilityHeader, netaccess.Protocol)
				}
				up := websocket.Upgrader{}
				conn, err := up.Upgrade(w, r, hdr)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					if _, _, err = conn.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			sess, _, err := dialTunnel(ctx, Config{Server: srv.URL, User: "alice", Token: "synthetic", Transparent: true, Allow: []string{"DB.corp:443"}}, nil)
			if supported {
				if err != nil {
					t.Fatal(err)
				}
				sess.Close()
			} else if err == nil {
				sess.Close()
				t.Fatal("silently accepted old server")
			}
			select {
			case rules := <-seen:
				if len(rules) != 1 || rules[0] != "db.corp:443" {
					t.Fatal(rules)
				}
			case <-ctx.Done():
				t.Fatal("rules not sent")
			}
		})
	}
}
func TestTransparentValidationKeepsLegacyRules(t *testing.T) {
	cfg := Config{Allow: []string{"0.0.0.0/0"}}
	if _, _, err := cfg.Validate(); err != nil {
		t.Fatal("legacy config rejected", err)
	}
	cfg.Transparent = true
	if _, _, err := cfg.Validate(); err == nil {
		t.Fatal("transparent default route accepted")
	}
}
