package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/imageupdate"
	"agentbox/internal/store"
)

type componentInspectorFunc func(context.Context, string) (imageupdate.Image, error)

func (f componentInspectorFunc) InspectCLIImage(ctx context.Context, ref string) (imageupdate.Image, error) {
	return f(ctx, ref)
}

func TestUpdateComponentsObserveLabelsWithoutClaimingCompatibility(t *testing.T) {
	s, _ := accessTestServer(t)
	calls := 0
	v := s.updateComponents(t.Context(), componentInspectorFunc(func(ctx context.Context, ref string) (imageupdate.Image, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second {
			t.Fatal("unbounded image inspection")
		}
		if ref != s.cfg.GetAgentImage() {
			t.Fatal("inspected another image", ref)
		}
		return imageupdate.Image{ID: "sha256:fixture", Claude: "2.1.280", Codex: "0.145.0", User: "private-image-user"}, nil
	}))
	if calls != 1 || v.Image.State != "labels_only" || v.Image.Claude != "2.1.280" || v.Image.ObservedAt == 0 || v.Server.Schema != store.SchemaVersion || v.Server.Candidate != "preflight_required" || v.Desktop.Installed != "browser_unknown" || v.Desktop.Protocol != 1 {
		t.Fatalf("wrong evidence scope: %+v", v)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "private-image-user") {
		t.Fatal("image configuration leaked")
	}
	for _, enabled := range []bool{false, true} {
		if err := s.cfg.ApplySettings(config.SettingsPatch{DesktopSync: &enabled}); err != nil {
			t.Fatal(err)
		}
		v = s.updateComponents(t.Context(), nil)
		if v.Desktop.Sync != enabled || v.Image.State != "unavailable" {
			t.Fatal("offline observation invented availability", v)
		}
	}
}

func TestUpdateComponentsHideErrorsAndInvalidateChangedImage(t *testing.T) {
	s, _ := accessTestServer(t)
	v := s.updateComponents(t.Context(), componentInspectorFunc(func(context.Context, string) (imageupdate.Image, error) {
		return imageupdate.Image{}, errors.New("/private/path?token=secret")
	}))
	raw, _ := json.Marshal(v)
	if v.Image.State != "unavailable" || strings.Contains(string(raw), "private/path") || strings.Contains(string(raw), "secret") {
		t.Fatal(string(raw))
	}
	v = s.updateComponents(t.Context(), componentInspectorFunc(func(context.Context, string) (imageupdate.Image, error) {
		image := "changed:image"
		if err := s.cfg.ApplySettings(config.SettingsPatch{AgentImage: &image}); err != nil {
			t.Fatal(err)
		}
		return imageupdate.Image{ID: "old-id", Claude: "old-version"}, nil
	}))
	if v.Image.State != "changed" || v.Image.Claude != "" || v.Image.ID != "" || v.Image.Reference != "changed:image" {
		t.Fatal("painted stale image metadata", v)
	}
}

func TestUpdateComponentsRequireAdmin(t *testing.T) {
	s, _ := newTestServer(t)
	for _, role := range []string{store.RoleAdmin, store.RoleUser} {
		if err := s.store.CreateUser(store.User{Name: role, Role: role}); err != nil {
			t.Fatal(err)
		}
		if err := s.store.CreateToken(role, role); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		token  string
		status int
	}{{"", 401}, {store.RoleUser, 403}, {store.RoleAdmin, 200}} {
		r := httptest.NewRequest("GET", "/api/updates/components", nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal(tc, w.Code, w.Body.String())
		}
		if tc.status == 200 && w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cacheable observation")
		}
	}
}
