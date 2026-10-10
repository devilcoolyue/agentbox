// Package toolstore prepares service-owned mount roots. User-writable cache
// contents are never traversed by the host; initialization runs inside Docker.
package toolstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"agentbox/internal/safefs"
)

const PublicMount = "/opt/agentbox-tools"
const CacheMount = "/var/cache/agentbox-user"
const Layout = "1"

var keyRE = regexp.MustCompile(`^[a-f0-9]{64}$`)

func OwnerKey(instance, user, created string) string {
	sum := sha256.Sum256([]byte("tool-cache-v1\x00" + instance + "\x00" + user + "\x00" + created))
	return hex.EncodeToString(sum[:])
}

type Paths struct{ Public, Cache string }

func Prepare(root, key string, uid, gid int) (Paths, error) {
	if !keyRE.MatchString(key) {
		return Paths{}, fmt.Errorf("invalid cache owner identity")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return Paths{}, fmt.Errorf("initialize tool storage with scripts/tool-storage.py first: %w", err)
	}
	if !info.IsDir() || info.Mode()&0022 != 0 {
		return Paths{}, fmt.Errorf("tool storage root must be service-owned and not group/world writable")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		return Paths{}, fmt.Errorf("tool storage root has foreign owner")
	}
	r, err := safefs.Open(root)
	if err != nil {
		return Paths{}, err
	}
	defer r.Close()
	// Ancestors are never mounted. A container can replace cache contents, but
	// cannot swap its mount source or select a different user's directory.
	for _, dir := range []string{"public", "private", "private/" + key} {
		mode := os.FileMode(0700)
		if dir == "public" {
			mode = 0755
		}
		if err = r.MkdirAll(dir, mode); err != nil {
			return Paths{}, err
		}
		st, e := r.Lstat(dir)
		if e != nil {
			return Paths{}, e
		}
		meta, ok := st.Sys().(*syscall.Stat_t)
		if !ok || int(meta.Uid) != os.Geteuid() || st.Mode()&0022 != 0 {
			return Paths{}, fmt.Errorf("untrusted tool storage ancestor %s", dir)
		}
		if dir == "public" && st.Mode().Perm()&0005 != 0005 {
			return Paths{}, fmt.Errorf("public tools must be readable and searchable by containers")
		}

	}
	leaf := "private/" + key + "/cache"
	if err = r.MkdirAll(leaf, 0700); err != nil {
		return Paths{}, err
	}
	if err = r.Chown(leaf, uid, gid); err != nil {
		return Paths{}, err
	}
	return Paths{filepath.Join(root, "public"), filepath.Join(root, filepath.FromSlash(leaf))}, nil
}

func GoBin(root, arch, version string) (string, error) {
	if version == "" {
		return "", nil
	}
	if arch != "amd64" && arch != "arm64" {
		return "", fmt.Errorf("unsupported shared Go architecture")
	}
	r, err := safefs.Open(filepath.Join(root, "public"))
	if err != nil {
		return "", err
	}
	defer r.Close()
	rel := "go/linux-" + arch + "/" + version
	// Publisher writes this marker only after archive verification and extraction.
	raw, err := r.ReadAll(rel+"/.agentbox-tool.json", 64<<10)
	if err != nil {
		return "", fmt.Errorf("shared Go SDK is not published: %w", err)
	}
	var meta struct {
		Format                  int
		Kind, Version, Platform string
	}
	if json.Unmarshal(raw, &meta) != nil || meta.Format != 1 || meta.Kind != "go" || meta.Version != version || meta.Platform != "linux-"+arch {
		return "", fmt.Errorf("shared Go publication metadata mismatch")
	}

	f, err := r.OpenFile(rel + "/bin/go")
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.Mode()&0111 == 0 {
		return "", fmt.Errorf("shared Go is not executable")
	}
	return PublicMount + "/" + rel + "/bin", nil
}
