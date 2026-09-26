package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"

	"agentbox/internal/safefs"
)

// Restore validates before creating any destination content. Target must not
// exist; publication is a no-replace rename of a private staging directory.
// It never overwrites a running instance or reuses old SQLite WAL files.
func Restore(ctx context.Context, archive, target string) (*Manifest, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := verify(ctx, f)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(abs); !os.IsNotExist(err) {
		return nil, errors.New("restore target must be a new directory")
	}
	stage, err := os.MkdirTemp(filepath.Dir(abs), ".agentbox-restore-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	root, err := safefs.Open(stage)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(&contextReader{ctx, f})
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var links, dirs []Entry
	for _, entry := range m.Entries {
		hdr, err := tr.Next()
		if err != nil {
			return nil, err
		}
		actual := entryFromHeader(hdr)
		actual.SHA256 = entry.SHA256
		if !reflect.DeepEqual(actual, entry) {
			return nil, errors.New("archive changed after verification")
		}
		if err = root.MkdirAll(path.Dir(entry.Name), 0o700); err != nil {
			return nil, err
		}
		switch entry.Type {
		case tar.TypeDir:
			if err = root.MkdirAll(entry.Name, 0o700); err != nil {
				return nil, err
			}
			dirs = append(dirs, entry)
		case tar.TypeSymlink:
			links = append(links, entry)
		case tar.TypeReg:
			h := sha256.New()
			_, err = root.WriteAtomic(entry.Name, io.TeeReader(tr, h), safefs.WriteOptions{
				Mode: os.FileMode(entry.Mode), Chown: os.Geteuid() == 0, UID: entry.UID, GID: entry.GID,
				ModTime: entry.ModTime, MaxBytes: entry.Size, Limit: true,
			})
			if err != nil {
				return nil, err
			}
			if hex.EncodeToString(h.Sum(nil)) != entry.SHA256 {
				return nil, errors.New("archive content changed after verification")
			}
		}
	}
	if err = checkDB(ctx, filepath.Join(stage, "data/state.db")); err != nil {
		return nil, err
	}
	if err = checkGitSecrets(filepath.Join(stage, "data/state.db"), filepath.Join(stage, "data")); err != nil {
		return nil, err
	}
	configRaw, readErr := root.ReadAll("config.json", 4<<20)
	if readErr != nil {
		return nil, readErr
	}
	if err = checkGitOAuthSecrets(configRaw, filepath.Join(stage, "data")); err != nil {
		return nil, err
	}
	if err = rewriteConfig(root, m); err != nil {
		return nil, err
	}
	// All content is materialized before any symlink can become an ancestor.
	for _, entry := range links {
		uid, gid := entry.UID, entry.GID
		if os.Geteuid() != 0 {
			uid, gid = os.Getuid(), os.Getgid()
		}
		if err = root.Symlink(entry.Link, entry.Name, uid, gid); err != nil {
			return nil, err
		}
		if err = root.SetLinkTime(entry.Name, entry.ModTime); err != nil {
			return nil, err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		entry := dirs[i]
		uid, gid := -1, -1
		if os.Geteuid() == 0 {
			uid, gid = entry.UID, entry.GID
		}
		if err = root.SetDirMetadata(entry.Name, os.FileMode(entry.Mode), uid, gid, entry.ModTime); err != nil {
			return nil, err
		}
	}
	report, _ := json.MarshalIndent(m, "", "  ")
	if _, err = root.WriteFile("backup-manifest.json", report, safefs.WriteOptions{Mode: 0o600}); err != nil {
		return nil, err
	}
	parent, err := safefs.Open(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if err = parent.RenameTo(filepath.Base(stage), parent, filepath.Base(abs), false); err != nil {
		return nil, err
	}
	return m, nil
}

// Preserve unknown/future configuration fields. Only deployment paths change;
// old files are restored into an independent instance, never original host paths.
func rewriteConfig(root *safefs.Root, m *Manifest) error {
	raw, err := root.ReadAll("config.json", 4<<20)
	if err != nil {
		return err
	}
	var cfg map[string]json.RawMessage
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("invalid configuration")
	}
	cfg["data_dir"] = json.RawMessage(`"data"`)
	cfg["cache_dir"] = json.RawMessage(`"cache"`)
	var accounts []map[string]json.RawMessage
	if raw := cfg["accounts"]; len(raw) > 0 {
		if err = json.Unmarshal(raw, &accounts); err != nil {
			return err
		}
	}
	for _, acct := range accounts {
		var id, cred string
		if err = json.Unmarshal(acct["id"], &id); err != nil {
			return err
		}
		if raw := acct["credentials_dir"]; len(raw) > 0 {
			if err = json.Unmarshal(raw, &cred); err != nil {
				return err
			}
		}
		if cred != "" {
			name, ok := m.Credentials[id]
			if !ok {
				return fmt.Errorf("no credential backup for account %s", id)
			}
			acct["credentials_dir"] = json.RawMessage(strconv.Quote(name))
		}
	}
	if _, ok := cfg["accounts"]; ok {
		cfg["accounts"], err = json.Marshal(accounts)
		if err != nil {
			return err
		}
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	_, err = root.WriteFile("config.json", out, safefs.WriteOptions{Mode: 0o600})
	return err
}
