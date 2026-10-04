package syncfs

import (
	"crypto/rand"
	"fmt"
	"os"
	"runtime"
	"strings"

	"agentbox/internal/syncproto"
)

type Capabilities struct {
	NamePolicy  NamePolicy `json:"name_policy"`
	Executable  bool       `json:"executable"`
	DirectoryID string     `json:"directory_id"`
}

// Probe creates a private, random directory in the selected root, then removes
// only its own files. Do not infer case/normalization behavior from OS alone:
// APFS can be case-sensitive, and Windows volumes can vary per directory.
func (r *Root) Probe() (Capabilities, error) {
	if err := r.CheckIdentity(); err != nil {
		return Capabilities{}, err
	}
	name := ".agentbox-sync-tmp-" + rand.Text()
	if err := r.root.Mkdir(name, 0700); err != nil {
		return Capabilities{}, err
	}
	probe, err := r.sub(name)
	if err != nil {
		return Capabilities{}, err
	}
	defer r.root.Remove(name)
	defer probe.Close()
	created := []string{}
	defer func() {
		for _, file := range created {
			_ = probe.root.Remove(file)
		}
	}()
	check := func(original, alternate string) (bool, error) {
		f, err := probe.root.OpenFile(original, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return false, err
		}
		created = append(created, original)
		info, err := f.Stat()
		closeErr := f.Close()
		if err != nil {
			return false, err
		}
		if closeErr != nil {
			return false, closeErr
		}
		other, err := probe.root.Lstat(alternate)
		if os.IsNotExist(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if !os.SameFile(info, other) {
			return false, syncproto.ErrChanged
		}
		return false, nil
	}
	caseSensitive, err := check("AbOx-CaSe", "abox-case")
	if err != nil {
		return Capabilities{}, err
	}
	normalizationSensitive, err := check("abox-\u00e9", "abox-e\u0301")
	if err != nil {
		return Capabilities{}, err
	}
	identity, err := r.Identity()
	if err != nil {
		return Capabilities{}, err
	}
	maxPath := 4096
	if runtime.GOOS == "windows" {
		maxPath = 240 - len(r.absolute)
		if maxPath < 1 {
			return Capabilities{}, fmt.Errorf("local mapping path is too long")
		}
	}
	return Capabilities{DirectoryID: identity, Executable: runtime.GOOS != "windows", NamePolicy: NamePolicy{Windows: runtime.GOOS == "windows", CaseSensitive: caseSensitive, NormalizationSensitive: normalizationSensitive, MaxPathUnits: maxPath}}, r.CheckIdentity()
}

// Identity reads the pinned directory's native file ID without writing probes.
// It is used to detect mapping overlap through filesystem path aliases.
func (r *Root) Identity() (string, error) {
	if err := r.CheckIdentity(); err != nil {
		return "", err
	}
	identity, err := directoryIdentity(r.root)
	if err != nil {
		return "", err
	}
	return syncproto.HashBytes([]byte(strings.Join([]string{runtime.GOOS, identity}, ":"))), nil
}
