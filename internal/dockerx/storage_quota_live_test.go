package dockerx

import (
	"context"
	"os"
	"testing"
	"time"

	"agentbox/internal/config"
	"github.com/docker/docker/api/types/container"
)

// Test the actual serialized production profile through Docker/libseccomp.
// Synthetic tmpfs only; no accounts, user mounts, provider, or image pulls.
func TestStorageQuotaProfileLive(t *testing.T) {
	ref := os.Getenv("AGENTBOX_QUOTA_PROFILE_TEST_IMAGE")
	if ref == "" {
		t.Skip("requires a prepared Linux image with Python")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	m, err := New(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	info, err := m.cli.ImageInspect(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	probe := `import os,fcntl,ctypes,errno,pty,termios
fd=os.open('/tmp',os.O_RDONLY)
for cmd in (0x401c5820,0x40086602,0x40046602):
 try:fcntl.ioctl(fd,cmd,bytes(28))
 except PermissionError:pass
 else:raise AssertionError('attribute mutation reached filesystem')
lib=ctypes.CDLL(None,use_errno=True)
# libc ioctl takes a 64-bit unsigned long, kernel decodes the low 32 bits.
for cmd in (0x401c5820,0x40086602,0x40046602):
 result=lib.ioctl(fd,ctypes.c_ulong(0xffffffff00000000|cmd),ctypes.create_string_buffer(28))
 assert result==-1 and ctypes.get_errno() in (errno.EPERM,errno.EACCES)
a,b=pty.openpty();termios.tcgetattr(b);fcntl.ioctl(b,termios.TIOCGWINSZ,bytes(8));os.close(a);os.close(b)
print('production quota profile and normal terminal ioctls passed')`
	for _, browser := range []bool{false, true} {
		cc := &container.Config{Image: info.ID, User: "1000:1000", Entrypoint: []string{"python3", "-I", "-c", probe}, Labels: map[string]string{"agentbox.quota-test": "1"}}
		hc := &container.HostConfig{NetworkMode: "none", ReadonlyRootfs: true, SecurityOpt: []string{"no-new-privileges:true"}, Tmpfs: map[string]string{"/tmp": "rw,nosuid,nodev,uid=1000,gid=1000,size=8m"}}
		if browser {
			browserContainerOptions(cc, hc, map[string]string{"agentbox.browser": "1"})
		}
		if err := quotaContainerOptions(cc, hc); err != nil {
			t.Fatal(err)
		}
		created, err := m.cli.ContainerCreate(ctx, cc, hc, nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			c, done := context.WithTimeout(context.Background(), 15*time.Second)
			defer done()
			if err := m.cli.ContainerRemove(c, created.ID, container.RemoveOptions{Force: true}); err != nil {
				t.Error(err)
			}
		})
		if err := m.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
			t.Fatal(err)
		}
		results, failures := m.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
		select {
		case result := <-results:
			if result.StatusCode != 0 {
				t.Fatalf("profile probe exited %d, browser=%v", result.StatusCode, browser)
			}
		case err := <-failures:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
