package dockerx

import (
	"agentbox/internal/config"
	"encoding/json"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQuotaGuardKeepsSandboxAndMasksIOCTL(t *testing.T) {
	for _, browser := range []bool{false, true} {
		cc := &container.Config{Labels: map[string]string{}}
		hc := &container.HostConfig{SecurityOpt: []string{"no-new-privileges:true"}}
		if browser {
			browserContainerOptions(cc, hc, map[string]string{"agentbox.browser": "1"})
		}
		if err := quotaContainerOptions(cc, hc); err != nil {
			t.Fatal(err)
		}
		if len(hc.SecurityOpt) != 2 || hc.SecurityOpt[0] != "no-new-privileges:true" || cc.Labels[quotaGuardLabel] != "2" {
			t.Fatal("missing guard")
		}
		var p struct {
			DefaultAction string
			Syscalls      []struct {
				Comment  string
				Names    []string
				Action   string
				ErrnoRet int
				Args     []struct {
					Index           int
					Value, ValueTwo uint64
					Op              string
				}
			}
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(hc.SecurityOpt[1], "seccomp=")), &p); err != nil {
			t.Fatal(err)
		}
		if p.DefaultAction != "SCMP_ACT_ERRNO" {
			t.Fatal("default profile weakened")
		}

		ns := false
		for _, call := range p.Syscalls {
			if call.Comment == "Allow create user namespaces" {
				ns = true
			}
		}
		if ns != browser {
			t.Fatal("namespace policy changed")
		}
		allowed := func(command uint64) bool {
			for _, call := range p.Syscalls {
				for _, name := range call.Names {
					if name != "ioctl" || call.Action != "SCMP_ACT_ALLOW" {
						continue
					}
					if len(call.Args) != 1 || call.Args[0].Index != 1 || call.Args[0].Op != "SCMP_CMP_MASKED_EQ" {
						t.Fatal("unbounded ioctl allow")
					}
					if command&call.Args[0].Value == call.Args[0].ValueTwo {
						return true
					}
				}
			}
			return false
		}
		for _, cmd := range []uint64{0x401c5820, 0x40086602, 0x40046602, 0xffffffff401c5820} {
			if allowed(cmd) {
				t.Fatalf("allowed %x", cmd)
			}
		}
		for _, cmd := range []uint64{0x801c581f, 0x5401, 0x5413, 0, 0xffffffff, 0x401c5821} {
			if !allowed(cmd) {
				t.Fatalf("blocked normal ioctl %x", cmd)
			}
		}

	}
}

// Restart admission must accept the profile created by this binary while
// rejecting missing, retired and unknown guards on running workspaces.
func TestStorageQuotaRestartAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, guard        string
		running, wantError bool
	}{
		{"current", "2", true, false}, {"legacy", "1", true, true},
		{"missing", "", true, true}, {"unknown", "3", true, true},
		{"stopped-legacy", "1", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Labels": map[string]string{quotaGuardLabel: tc.guard}}, "State": map[string]any{"Running": tc.running}})
			}))
			defer server.Close()
			cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.45"))
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			m := &Manager{cli: cli, cfg: &config.Config{StoragePolicy: config.StoragePolicy{Mount: "/quota"}}}
			err = m.CheckStorageQuotaContainer(t.Context(), "workspace")
			if (err != nil) != tc.wantError {
				t.Fatalf("admission error = %v, want rejection %v", err, tc.wantError)
			}
		})
	}
}
