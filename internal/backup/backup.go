// Package backup creates portable, versioned backups without opening the live
// database through store.Open (which would migrate it). All source walks are
// confined; restored symlinks are created only after regular files are verified.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"agentbox/internal/buildinfo"
	"agentbox/internal/config"
	"agentbox/internal/gitaccess"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"modernc.org/sqlite"
)

const FormatVersion = 1

// Large persistent CLI homes commonly contain more than 100k small files.
// Keep finite bounds shared by creation and verification.
const manifestLimit = 256 << 20
const entryLimit = 1000000

type Entry struct {
	Name    string    `json:"name"`
	Type    byte      `json:"type"`
	Size    int64     `json:"size"`
	Mode    int64     `json:"mode"`
	UID     int       `json:"uid"`
	GID     int       `json:"gid"`
	ModTime time.Time `json:"mtime"`
	Link    string    `json:"link,omitempty"`
	SHA256  string    `json:"sha256,omitempty"`
}

type Manifest struct {
	Version     int               `json:"format_version"`
	Mode        string            `json:"mode"`
	Consistency string            `json:"consistency"`
	Created     time.Time         `json:"created"`
	Revision    string            `json:"revision"`
	Credentials map[string]string `json:"credentials"`
	Entries     []Entry           `json:"entries"`
}

type Options struct {
	Config string
	Output string
	Full   bool
	// CheckStopped verifies no running container mounts any protected directory.
	// It is mandatory for full backups and runs while the service flock is held.
	CheckStopped func(context.Context, []string) error
}

type sourceConfig struct {
	DataDir  string `json:"data_dir"`
	Accounts []struct {
		ID             string `json:"id"`
		CredentialsDir string `json:"credentials_dir"`
	} `json:"accounts"`
}

func resolve(base, name string) string {
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	return filepath.Join(base, name)
}

func Create(ctx context.Context, opts Options) (_ *Manifest, err error) {
	configPath, err := filepath.Abs(opts.Config)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var cfg sourceConfig
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	data := resolve(filepath.Dir(configPath), cfg.DataDir)
	var lock *os.File
	if opts.Full {
		lock, err = os.OpenFile(filepath.Join(data, "agentbox.lock"), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		defer lock.Close()
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return nil, errors.New("full backup requires agentbox to be stopped (data directory is locked)")
		}
		// Load again after acquiring the lock: configuration could have changed
		// during shutdown, including its data directory.
		again, e := os.ReadFile(configPath)
		if e != nil || string(again) != string(raw) {
			return nil, errors.New("configuration changed; retry backup")
		}
	}
	protected := []string{data}
	for _, acct := range cfg.Accounts {
		if acct.CredentialsDir != "" {
			protected = append(protected, resolve(filepath.Dir(configPath), acct.CredentialsDir))
		}
	}
	if opts.Full {
		if opts.CheckStopped == nil {
			return nil, errors.New("full backup requires Docker mount verification")
		}
		if err = opts.CheckStopped(ctx, protected); err != nil {
			return nil, err
		}
	}
	output, err := filepath.Abs(opts.Output)
	if err != nil {
		return nil, err
	}
	// Output must not become part of a tree being archived.
	for _, root := range append(protected[1:], filepath.Join(data, "users"), filepath.Join(data, "creds"), filepath.Join(data, "git-secrets"), filepath.Join(data, "home-template"), filepath.Join(filepath.Dir(configPath), "accounts")) {
		if rel, e := filepath.Rel(root, output); e == nil && filepath.IsLocal(rel) {
			return nil, errors.New("backup output cannot be inside an included directory")
		}
	}
	stage, err := os.MkdirTemp(filepath.Dir(output), ".agentbox-backup-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	snapshot := filepath.Join(stage, "state.db")
	if err = snapshotDB(ctx, filepath.Join(data, "state.db"), snapshot); err != nil {
		return nil, err
	}
	if err = checkGitOAuthSecrets(raw, data); err != nil {
		return nil, err
	}
	if err = checkGitSecrets(snapshot, data); err != nil {
		return nil, err
	}
	m := &Manifest{Version: FormatVersion, Mode: "system", Consistency: "online-database-snapshot-files-sequential", Created: time.Now().UTC(), Credentials: map[string]string{}, Entries: []Entry{}}
	if opts.Full {
		m.Mode = "full"
		m.Consistency = "service-locked-containers-stopped"
	}
	m.Revision = buildinfo.Commit()
	f, err := os.OpenFile(filepath.Join(stage, "backup.tar.gz"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	defer tw.Close()
	defer gz.Close()
	add := func(base, rel, name string, optional bool) error {
		return addTree(ctx, tw, m, base, rel, name, optional)
	}
	if err = add(filepath.Dir(configPath), filepath.Base(configPath), "config.json", false); err != nil {
		return nil, err
	}
	if err = add(stage, "state.db", "data/state.db", false); err != nil {
		return nil, err
	}
	for _, name := range []string{"creds", "home-template", "git-secrets"} {
		if err = add(data, name, "data/"+name, true); err != nil {
			return nil, err
		}
	}
	if err = add(filepath.Dir(configPath), "accounts", "accounts", true); err != nil {
		return nil, err
	}
	seenCred := map[string]string{}
	for _, acct := range cfg.Accounts {
		if acct.CredentialsDir == "" {
			continue
		}
		if acct.ID == "" {
			return nil, errors.New("account with credentials has no ID")
		}
		dir := resolve(filepath.Dir(configPath), acct.CredentialsDir)
		dest, ok := seenCred[dir]
		if !ok {
			dest = fmt.Sprintf("credentials/%d", len(seenCred))
			// Account roots are administrator-authorized paths. Open the parent so
			// a symlink in the final directory name fails rather than being followed.
			if err = add(filepath.Dir(dir), filepath.Base(dir), dest, false); err != nil {
				return nil, fmt.Errorf("backup account %s: %w", acct.ID, err)
			}
			seenCred[dir] = dest
		}
		if _, duplicate := m.Credentials[acct.ID]; duplicate {
			return nil, errors.New("duplicate account ID")
		}
		m.Credentials[acct.ID] = dest
	}
	if opts.Full {
		if err = add(data, "users", "data/users", true); err != nil {
			return nil, err
		}
	} else {
		root, e := safefs.Open(data)
		if e != nil {
			return nil, e
		}
		users, e := root.ReadDir("users")
		root.Close()
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		for _, user := range users {
			if !user.IsDir() {
				continue
			}

			// MCP canonical configuration and reconciliation state are control
			// data, not workspace content; include them in system backups too.
			base := path.Join("users", user.Name())
			if err = add(data, path.Join(base, "mcp.json"), "data/"+path.Join(base, "mcp.json"), true); err != nil {
				return nil, err
			}
			root, e := safefs.Open(data)
			if e != nil {
				return nil, e
			}
			sessions, e := root.ReadDir(path.Join(base, "sessions"))
			root.Close()
			if e != nil && !os.IsNotExist(e) {
				return nil, e
			}
			for _, session := range sessions {
				if !session.IsDir() {
					continue
				}
				rel := path.Join(base, "sessions", session.Name(), "mcp.json")
				if err = add(data, rel, "data/"+rel, true); err != nil {
					return nil, err
				}
			}
			rel := path.Join("users", user.Name(), "home-template")
			if err = add(data, rel, "data/"+rel, true); err != nil {
				return nil, err
			}
		}
	}
	again, e := os.ReadFile(configPath)
	if e != nil || string(again) != string(raw) {
		return nil, errors.New("configuration changed during backup; retry")
	}
	if opts.Full {
		if err = opts.CheckStopped(ctx, protected); err != nil {
			return nil, err
		}
	}
	manifest, e := json.Marshal(m)
	if e != nil {
		return nil, e
	}
	if len(manifest) > manifestLimit {
		return nil, errors.New("backup manifest too large")
	}
	if err = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err = tw.Write(manifest); err != nil {
		return nil, err
	}
	if err = tw.Close(); err != nil {
		return nil, err
	}
	if err = gz.Close(); err != nil {
		return nil, err
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if _, err = Verify(ctx, f.Name()); err != nil {
		return nil, fmt.Errorf("verify new backup: %w", err)
	}
	// Publish without overwriting an existing backup, including concurrent jobs.
	if err = os.Link(f.Name(), output); err != nil {
		return nil, err
	}
	return m, nil
}

func addTree(ctx context.Context, tw *tar.Writer, m *Manifest, base, rel, dest string, optional bool) error {
	root, err := safefs.Open(base)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if optional && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	add := func(r *safefs.Root, name, out string, info fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(m.Entries) >= entryLimit {
			return fmt.Errorf("backup exceeds %d entries", entryLimit)
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported special file: %s", out)
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			var err error
			link, err = r.Readlink(name)
			if err != nil {
				return err
			}
		}
		var f *os.File
		if info.Mode().IsRegular() {
			var err error
			f, err = r.OpenFile(name)
			if err != nil {
				return err
			}
			defer f.Close()
			info, err = f.Stat()
			if err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = out
		hdr.Mode &= 0o777
		hdr.ModTime = info.ModTime().UTC()
		hdr.Format = tar.FormatPAX
		if err = tw.WriteHeader(hdr); err != nil {
			return err
		}
		entry := entryFromHeader(hdr)
		if f != nil {
			h := sha256.New()
			if _, err = io.CopyN(io.MultiWriter(tw, h), &contextReader{ctx, f}, info.Size()); err != nil {
				return err
			}
			after, err := f.Stat()
			if err != nil {
				return err
			}
			if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
				return fmt.Errorf("file changed during backup: %s", out)
			}
			entry.SHA256 = hex.EncodeToString(h.Sum(nil))
		}
		m.Entries = append(m.Entries, entry)
		return nil
	}
	if !info.IsDir() {
		return add(root, rel, dest, info)
	}
	sub, err := root.Sub(rel)
	if err != nil {
		return err
	}
	defer sub.Close()
	return fs.WalkDir(sub.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return add(sub, name, path.Join(dest, name), info)
	})
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func entryFromHeader(h *tar.Header) Entry {
	return Entry{Name: h.Name, Type: h.Typeflag, Size: h.Size, Mode: h.Mode, UID: h.Uid, GID: h.Gid, ModTime: h.ModTime.UTC(), Link: h.Linkname}
}

func openDB(file string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: file}
	return sql.Open("sqlite", u.String()+"?mode=ro&_pragma=busy_timeout(5000)")
}

func snapshotDB(ctx context.Context, source, dest string) error {
	db, err := openDB(source)
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Raw(func(driver any) (result error) {
		maker, ok := driver.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite online backup unavailable")
		}
		b, err := maker.NewBackup(dest)
		if err != nil {
			return err
		}
		defer func() {
			if e := b.Finish(); result == nil {
				result = e
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := b.Step(256)
			if err != nil {
				return err
			}
			if !more {
				break
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The snapshot inherits WAL mode from its source. Make only the temporary
	// destination a standalone DELETE-mode database so verification/restoration
	// doesn't need sidecar files; the live database remains in WAL mode.
	u := url.URL{Scheme: "file", Path: dest}
	snapshot, err := sql.Open("sqlite", u.String()+"?mode=rw")
	if err != nil {
		return err
	}
	_, err = snapshot.ExecContext(ctx, "PRAGMA journal_mode=DELETE")
	closeErr := snapshot.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(dest, 0o600); err != nil {
		return err
	}
	return checkDB(ctx, dest)
}

func checkDB(ctx context.Context, file string) error {
	db, err := openDB(file)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("database integrity check: %s", result)
	}
	return nil
}

// Verify checks every entry and its metadata against the embedded manifest.
// Hashes detect corruption, not authenticity; use trusted backup storage.
func Verify(ctx context.Context, file string) (*Manifest, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return verify(ctx, f)
}

func validName(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return false
	}
	first, _, _ := strings.Cut(name, "/")
	return first == "data" || first == "accounts" || first == "credentials" || name == "config.json"
}

func verify(ctx context.Context, r io.Reader) (*Manifest, error) {
	gz, err := gzip.NewReader(&contextReader{ctx, r})
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := []Entry{}
	seen := map[string]byte{}
	ancestors := map[string]bool{}
	var m *Manifest
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if m != nil {
			return nil, errors.New("entries after manifest")
		}
		if hdr.Name == "manifest.json" {
			if hdr.Typeflag != tar.TypeReg || hdr.Size > manifestLimit {
				return nil, errors.New("invalid manifest")
			}
			raw, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			if err = json.Unmarshal(raw, &m); err != nil {
				return nil, err
			}
			continue
		}
		if !validName(hdr.Name) || len(entries) >= entryLimit {
			return nil, errors.New("invalid archive entry path or count")
		}
		if _, ok := seen[hdr.Name]; ok {
			return nil, errors.New("duplicate archive entry")
		}
		for parent := path.Dir(hdr.Name); parent != "."; parent = path.Dir(parent) {
			if typ, ok := seen[parent]; ok && typ != tar.TypeDir {
				return nil, errors.New("archive entry traverses a non-directory")
			}
		}
		// Enforce parent-before-child, including absent implicit top directories.
		if ancestors[hdr.Name] {
			return nil, errors.New("archive parent appears after child")
		}
		for parent := path.Dir(hdr.Name); parent != "."; parent = path.Dir(parent) {
			ancestors[parent] = true
		}
		if hdr.Mode & ^int64(0o777) != 0 || hdr.Size < 0 || hdr.Uid < 0 || hdr.Gid < 0 {
			return nil, errors.New("invalid file mode or size")
		}
		entry := entryFromHeader(hdr)
		switch hdr.Typeflag {
		case tar.TypeReg:
			h := sha256.New()
			if _, err := io.Copy(h, tr); err != nil {
				return nil, err
			}
			entry.SHA256 = hex.EncodeToString(h.Sum(nil))
		case tar.TypeDir, tar.TypeSymlink:
			if hdr.Size != 0 {
				return nil, errors.New("non-file with contents")
			}
		default:
			return nil, errors.New("unsupported archive entry type")
		}
		seen[hdr.Name] = hdr.Typeflag
		entries = append(entries, entry)
	}
	// Consume gzip trailer so truncation/checksum errors cannot hide after tar EOF.
	if _, err = io.Copy(io.Discard, gz); err != nil {
		return nil, err
	}
	if m == nil || m.Version != FormatVersion || (m.Mode != "system" && m.Mode != "full") {
		return nil, errors.New("missing or unsupported backup manifest")
	}
	if !reflect.DeepEqual(entries, m.Entries) {
		return nil, errors.New("backup checksum or metadata mismatch")
	}
	if seen["config.json"] != tar.TypeReg || seen["data/state.db"] != tar.TypeReg {
		return nil, errors.New("backup lacks configuration or database")
	}
	for _, dir := range m.Credentials {
		if !strings.HasPrefix(dir, "credentials/") || seen[dir] != tar.TypeDir {
			return nil, errors.New("invalid credentials mapping")
		}
	}
	return m, nil
}

// Validate against the already-created snapshot, without store.Open/migration.
// A successful backup must not silently omit the key for encrypted credentials.
func checkGitSecrets(snapshot, data string) error {
	db, err := openDB(snapshot)
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	if err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='git_connections'").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	networkColumn := "'{}'"
	if err = db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('git_connections') WHERE name='network'").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		networkColumn = "network"
	}
	rows, err := db.Query("SELECT id,owner,provider,base_url,auth_type,username,secret," + networkColumn + " FROM git_connections")
	if err != nil {
		return err
	}
	defer rows.Close()
	vault := gitaccess.Vault{DataDir: data}
	for rows.Next() {
		var c store.GitConnection
		var network string
		if err = rows.Scan(&c.ID, &c.Owner, &c.Provider, &c.BaseURL, &c.AuthType, &c.Username, &c.Secret, &network); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(network), &c.Network); err != nil {
			return err
		}
		if _, err = vault.Open(c.Secret, c.AssociatedData()); err != nil {
			return err
		}
	}
	return rows.Err()
}

func checkGitOAuthSecrets(raw []byte, data string) error {
	var cfg struct {
		Apps []config.GitOAuthApp `json:"git_oauth_apps"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	vault := gitaccess.Vault{DataDir: data}
	for _, app := range cfg.Apps {
		if _, err := vault.Open(app.Secret, app.AssociatedData()); err != nil {
			return err
		}
	}
	return nil
}
