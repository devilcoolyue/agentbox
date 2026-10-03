package syncclient

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
	_ "modernc.org/sqlite"
)

var ErrPending = errors.New("unfinished sync batch requires reconciliation")
var ErrStateChanged = errors.New("sync state changed; reload before continuing")

const maxStateJSON = 256 << 20
const maxStoredMetadata = 256 << 20

// StateStore is native-only metadata under a private application directory,
// outside every selected project. No credentials, leases or file bytes enter
// this database. The caller owns the trusted application directory; it must
// never accept its location from the renderer or a remote server.
// FULL SQLite transactions persist intent before publication. Platform power
// loss guarantees still depend on SQLite and the underlying local filesystem.
type StateStore struct {
	db        *sql.DB
	root      *syncfs.Root
	directory string
}

type SavedBinding struct {
	Archived  bool                `json:"archived,omitempty"`
	ID        string              `json:"id"`
	Revision  int64               `json:"revision"`
	Binding   syncproto.Binding   `json:"binding"`
	Directory string              `json:"directory"`
	Ancestors []string            `json:"ancestors"`
	Baseline  *syncproto.Baseline `json:"baseline,omitempty"`
	Pending   *Batch              `json:"pending,omitempty"`
}
type Batch struct {
	Resolution      *BatchResolution   `json:"resolution,omitempty"`
	ID              string             `json:"id"`
	ProjectRevision int64              `json:"project_revision"`
	Plan            Plan               `json:"plan"`
	Options         PlanOptions        `json:"options"`
	Local           syncproto.Manifest `json:"local"`
	Remote          syncproto.Manifest `json:"remote"`
	Operations      []SavedOperation   `json:"operations"`
}
type SavedOperation struct {
	RemoteRetirement string           `json:"remote_retirement,omitempty"`
	RecoveryState    string           `json:"recovery_state,omitempty"` // discarding/discarded local copy; original reference remains auditable.
	ID               string           `json:"id"`
	Status           string           `json:"status"` // prepared, started, verified; started is ambiguous after a crash.
	Recovery         *syncfs.Recovery `json:"recovery,omitempty"`
}

func stateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func OpenState(directory string) (*StateStore, error) {
	return OpenStateContext(context.Background(), directory)
}

func OpenStateContext(ctx context.Context, directory string) (*StateStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	root, err := syncfs.OpenContext(ctx, abs)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*StateStore, error) { root.Close(); return nil, err }
	if runtime.GOOS != "windows" {
		info, err := os.Stat(abs)
		if err != nil {
			return fail(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			return fail(errors.New("local sync state requires a private application directory"))
		}
	}
	// Reject pre-existing links/special files before handing the trusted app path
	// to SQLite, which owns its own journal handles. Project paths never reach it.
	for _, name := range []string{"sync.db", "sync.db-journal", "sync.db-wal", "sync.db-shm"} {
		f, e := root.OpenFile(name)
		if e == nil {
			f.Close()
		} else if !os.IsNotExist(e) {
			return fail(e)
		}
	}
	dbPath := filepath.ToSlash(filepath.Join(abs, "sync.db"))
	if !strings.HasPrefix(dbPath, "/") {
		dbPath = "/" + dbPath
	}
	uri := (&url.URL{Scheme: "file", Path: dbPath}).String()
	db, err := sql.Open("sqlite", uri+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return fail(err)
	}
	db.SetMaxOpenConns(1)
	failed := func(err error) (*StateStore, error) { db.Close(); return fail(err) }
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return failed(err)
	}
	if version < 0 || version > 5 {
		return failed(errors.New("unsupported local sync state version"))
	}
	if _, err = db.ExecContext(ctx, "PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL"); err != nil {
		return failed(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return failed(err)
	}
	defer tx.Rollback()
	// Read inside the initialization transaction: another process may have
	// completed initialization after our first version check.
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		tx.Rollback()
		return failed(err)
	}
	if version == 0 {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&count); err != nil || count != 0 {
			tx.Rollback()
			return failed(errors.New("unrecognized local sync database"))
		}
		if _, err = tx.ExecContext(ctx, `CREATE TABLE metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE bindings(id TEXT PRIMARY KEY, revision INTEGER NOT NULL, state BLOB NOT NULL);
CREATE TABLE batches(binding TEXT NOT NULL, id TEXT NOT NULL, state BLOB NOT NULL, PRIMARY KEY(binding,id));
PRAGMA user_version=5;`); err != nil {
			tx.Rollback()
			return failed(err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO metadata VALUES ('device',?)", stateID()); err != nil {
			tx.Rollback()
			return failed(err)
		}
	} else if version >= 1 && version <= 4 {
		// Version 2 introduced archived mappings. Version 3 adds per-file choices
		// and explicitly abandoned unknown batches. Old readers must not label
		// the new unknown outcome as a completed batch or ignore saved choices.
		// Version 4 adds durable local recovery disposal. Older readers must
		// refuse rather than advertise a deliberately removed copy as available.
		// Version 5 retains remote cleanup intent/audit. Older readers must not
		// present retired recovery contents as available or drop their state.
		if _, err = tx.ExecContext(ctx, "PRAGMA user_version=5"); err != nil {
			tx.Rollback()
			return failed(err)
		}
	} else if version != 5 {
		tx.Rollback()
		return failed(errors.New("unsupported local sync state version"))
	}
	var device string
	if err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='device'").Scan(&device); err != nil || !syncproto.ValidOperationID(device) {
		tx.Rollback()
		return failed(errors.New("missing or corrupt local sync device identity"))
	}
	if err = tx.Commit(); err != nil {
		return failed(err)
	}
	if err = os.Chmod(filepath.Join(abs, "sync.db"), 0600); err != nil {
		return failed(err)
	}
	return &StateStore{db: db, root: root, directory: abs}, nil
}
func (s *StateStore) Close() error { return errors.Join(s.db.Close(), s.root.Close()) }
func (s *StateStore) Device() (string, error) {
	if err := s.root.CheckIdentity(); err != nil {
		return "", err
	}
	var id string
	err := s.db.QueryRow("SELECT value FROM metadata WHERE key='device'").Scan(&id)
	if err == nil && !syncproto.ValidOperationID(id) {
		err = syncproto.ErrInvalid
	}
	return id, err
}
func pathsOverlap(a, b string) bool {
	// Conservative case folding also handles default macOS/Windows volumes.
	// False positives on a case-sensitive volume are preferable to double writers.
	a, b = strings.ToLower(filepath.Clean(a)), strings.ToLower(filepath.Clean(b))
	return a == b || strings.HasPrefix(a, strings.TrimRight(b, string(filepath.Separator))+string(filepath.Separator)) || strings.HasPrefix(b, strings.TrimRight(a, string(filepath.Separator))+string(filepath.Separator))
}
func (s *StateStore) Register(binding syncproto.Binding, directory string) (SavedBinding, error) {
	return s.RegisterContext(context.Background(), binding, directory)
}

func (s *StateStore) RegisterContext(ctx context.Context, binding syncproto.Binding, directory string) (SavedBinding, error) {
	if err := ctx.Err(); err != nil {
		return SavedBinding{}, err
	}
	var saved SavedBinding
	if binding.Validate() != nil {
		return saved, ErrBinding
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return saved, err
	}
	if pathsOverlap(abs, s.directory) {
		return saved, ErrBinding
	}
	local, err := syncfs.OpenContext(ctx, abs)
	if err != nil {
		return saved, err
	}
	defer local.Close()
	caps, err := local.Probe()
	if err != nil {
		return saved, err
	}
	ancestors, err := local.AncestorIdentitiesContext(ctx)
	if err != nil {
		return saved, err
	}
	stateAncestors, err := s.root.AncestorIdentitiesContext(ctx)
	if err != nil {
		return saved, err
	}
	if slices.Contains(ancestors, stateAncestors[0]) || slices.Contains(stateAncestors, binding.LocalID) {
		return saved, ErrBinding
	}
	if caps.DirectoryID != binding.LocalID || ancestors[0] != binding.LocalID {
		return saved, ErrBinding
	}
	if err = s.root.CheckIdentity(); err != nil {
		return saved, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return saved, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT state FROM bindings")
	if err != nil {
		return saved, err
	}
	count := 0
	for rows.Next() {
		var raw []byte
		var other SavedBinding
		if err = rows.Scan(&raw); err == nil {
			err = decodeState(raw, &other)
		}
		if err != nil {
			rows.Close()
			return saved, err
		}
		count++
		if other.Archived {
			continue
		}
		sameRemote := (other.Binding.ServerID == binding.ServerID || other.Binding.Server == binding.Server) && other.Binding.User == binding.User && other.Binding.Workspace == binding.Workspace
		if slices.Contains(ancestors, other.Binding.LocalID) || slices.Contains(other.Ancestors, binding.LocalID) || pathsOverlap(other.Directory, abs) || sameRemote && (other.Binding.Project == binding.Project || remotePathsOverlap(other.Binding.ProjectPath, binding.ProjectPath)) {
			rows.Close()
			return saved, ErrBinding
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return saved, err
	}
	if count >= 1024 {
		return saved, syncproto.ErrLimit
	}
	saved = SavedBinding{ID: stateID(), Revision: 1, Binding: binding, Directory: abs, Ancestors: ancestors}
	raw, err := json.Marshal(saved)
	if err != nil {
		return SavedBinding{}, err
	}
	if err = checkStateCapacity(tx, "", len(raw)); err != nil {
		return SavedBinding{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO bindings VALUES (?,?,?)", saved.ID, saved.Revision, raw); err != nil {
		return SavedBinding{}, err
	}
	if err = local.CheckIdentity(); err != nil {
		return SavedBinding{}, err
	}
	if err = tx.Commit(); err != nil {
		return SavedBinding{}, err
	}
	return saved, nil
}
func remotePathsOverlap(a, b string) bool {
	return a == "." || b == "." || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func decodeState(raw []byte, saved *SavedBinding) error {
	if len(raw) > maxStateJSON || json.Unmarshal(raw, saved) != nil || !syncproto.ValidOperationID(saved.ID) || saved.Revision < 1 || saved.Binding.Validate() != nil || !filepath.IsAbs(saved.Directory) {
		return syncproto.ErrInvalid
	}
	if saved.Archived && saved.Pending != nil {
		return syncproto.ErrInvalid
	}
	if len(saved.Ancestors) == 0 || len(saved.Ancestors) > 256 || saved.Ancestors[0] != saved.Binding.LocalID {
		return syncproto.ErrInvalid
	}
	for _, id := range saved.Ancestors {
		if !syncproto.ValidHash(id) {
			return syncproto.ErrInvalid
		}
	}
	if saved.Baseline != nil {
		b := saved.Baseline
		if b.Binding != saved.Binding || b.Local.Validate() != nil || b.Remote.Validate() != nil || b.Local.RulesHash != b.Remote.RulesHash {
			return syncproto.ErrInvalid
		}
	}
	if p := saved.Pending; p != nil {
		if p.Resolution != nil || !syncproto.ValidOperationID(p.ID) || p.ProjectRevision < 1 || len(p.Operations) != len(p.Plan.Operations) {
			return syncproto.ErrInvalid
		}
		plan, err := BuildPlan(saved.Binding, saved.Baseline, p.Local, p.Remote, p.Options)
		if err != nil || plan.Digest != p.Plan.Digest || p.Plan.Ready(p.Plan.Digest) != nil {
			return syncproto.ErrInvalid
		}
		ids := map[string]bool{}
		unfinished := false
		for _, op := range p.Operations {
			if op.RecoveryState != "" || op.RemoteRetirement != "" {
				return syncproto.ErrInvalid
			}
			if !syncproto.ValidOperationID(op.ID) || ids[op.ID] {
				return syncproto.ErrInvalid
			}
			ids[op.ID] = true
			if op.Status != "prepared" && op.Status != "started" && op.Status != "verified" {
				return syncproto.ErrInvalid
			}
			if unfinished && op.Status != "prepared" {
				return syncproto.ErrInvalid
			}
			if op.Status != "verified" {
				unfinished = true
			}
			if op.Recovery != nil && (!syncproto.ValidPath(op.Recovery.Path) || !syncproto.ValidHash(op.Recovery.Hash) || op.Recovery.Size < 0 || op.Recovery.Size > syncproto.DefaultMaxFileBytes) {
				return syncproto.ErrInvalid
			}
		}
	}
	return nil
}
func (s *StateStore) Load(id string) (SavedBinding, error) {
	var saved SavedBinding
	if !syncproto.ValidOperationID(id) {
		return saved, syncproto.ErrInvalid
	}
	if err := s.root.CheckIdentity(); err != nil {
		return saved, err
	}
	var raw []byte
	if err := s.db.QueryRow("SELECT state FROM bindings WHERE id=?", id).Scan(&raw); err != nil {
		return saved, err
	}
	if err := decodeState(raw, &saved); err != nil {
		return SavedBinding{}, err
	}
	if saved.ID != id {
		return SavedBinding{}, syncproto.ErrInvalid
	}
	return saved, nil
}

// Bindings discovers persisted mappings after process restart. Native callers
// supply the currently authenticated server identity and user; other identities
// are not returned to their UI. The local database itself is per OS user.
func (s *StateStore) Bindings(serverID, user string) ([]SavedBinding, error) {
	if !syncproto.ValidHash(serverID) || user == "" {
		return nil, ErrBinding
	}
	return s.bindingsMatching(func(saved SavedBinding) bool { return saved.Binding.ServerID == serverID && saved.Binding.User == user })
}

// BindingsForServer exposes local historical records only for the authenticated
// URL/user. A changed installation ID is shown for explicit re-confirmation;
// it never authorizes sync or retrieval from the replacement server.
func (s *StateStore) BindingsForServer(server, user string) ([]SavedBinding, error) {
	normalized, err := syncproto.NormalizeServer(server)
	if err != nil || normalized != server || user == "" {
		return nil, ErrBinding
	}
	return s.bindingsMatching(func(saved SavedBinding) bool { return saved.Binding.Server == server && saved.Binding.User == user })
}
func (s *StateStore) bindingsMatching(matches func(SavedBinding) bool) ([]SavedBinding, error) {
	if err := s.root.CheckIdentity(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT state FROM bindings ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SavedBinding{}
	count := 0
	for rows.Next() {
		count++
		if count > 1024 {
			return nil, syncproto.ErrLimit
		}
		var raw []byte
		var saved SavedBinding
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = decodeState(raw, &saved); err != nil {
			return nil, err
		}
		if matches(saved) {
			result = append(result, saved)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *StateStore) change(id string, revision int64, apply func(*SavedBinding) error, archive bool) (SavedBinding, error) {
	var saved SavedBinding
	if err := s.root.CheckIdentity(); err != nil {
		return saved, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return saved, err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRow("SELECT state FROM bindings WHERE id=?", id).Scan(&raw); err != nil {
		return saved, err
	}
	if err = decodeState(raw, &saved); err != nil {
		return SavedBinding{}, err
	}
	if saved.Archived {
		return SavedBinding{}, ErrBinding
	}
	if saved.ID != id || saved.Revision != revision {
		return SavedBinding{}, ErrStateChanged
	}
	var history []byte
	var archived *Batch
	var batchID string
	if archive && saved.Pending != nil {
		archived = saved.Pending
		batchID = archived.ID
	}
	if err = apply(&saved); err != nil {
		return SavedBinding{}, err
	}
	if archived != nil {
		history, err = json.Marshal(archived)
		if err != nil {
			return SavedBinding{}, err
		}
	}
	if saved.Pending != nil {
		var count int
		if err = tx.QueryRow("SELECT count(*) FROM batches WHERE binding=?", id).Scan(&count); err != nil {
			return SavedBinding{}, err
		}
		if count >= 1000 {
			return SavedBinding{}, syncproto.ErrLimit
		}
	}
	saved.Revision++
	raw, err = json.Marshal(saved)
	if err != nil {
		return SavedBinding{}, err
	}
	if len(raw) > maxStateJSON {
		return SavedBinding{}, syncproto.ErrLimit
	}
	// Validate the exact serialized state before publishing it.
	var verified SavedBinding
	if err = decodeState(raw, &verified); err != nil {
		return SavedBinding{}, err
	}
	if err = checkStateCapacity(tx, id, len(raw)+len(history)); err != nil {
		return SavedBinding{}, err
	}
	if _, err = tx.Exec("UPDATE bindings SET revision=?,state=? WHERE id=?", saved.Revision, raw, id); err != nil {
		return SavedBinding{}, err
	}
	if history != nil {
		if _, err = tx.Exec("INSERT INTO batches VALUES (?,?,?)", id, batchID, history); err != nil {
			return SavedBinding{}, err
		}
	}
	if err = s.root.CheckIdentity(); err != nil {
		return SavedBinding{}, err
	}
	if err = tx.Commit(); err != nil {
		return SavedBinding{}, err
	}
	return saved, nil
}

// Begin must succeed before acquiring any file mutation permission. It rebuilds
// the plan from persisted baseline and supplied complete scans, so confirmation
// cannot bless a caller-edited operation. Executor must independently rescan,
// check capability, binding/project revision, and acquire/renew a lease.
func (s *StateStore) Begin(id string, revision, projectRevision int64, preview Plan, confirmation string, local, remote syncproto.Manifest, options PlanOptions) (SavedBinding, error) {
	return s.change(id, revision, func(saved *SavedBinding) error {
		if saved.Pending != nil {
			return ErrPending
		}
		if projectRevision < 1 {
			return syncproto.ErrInvalid
		}
		rebuilt, err := BuildPlan(saved.Binding, saved.Baseline, local, remote, options)
		if err != nil {
			return err
		}
		if preview.Digest != rebuilt.Digest {
			return ErrStateChanged
		}
		if err = rebuilt.Ready(confirmation); err != nil {
			return err
		}
		operations := make([]SavedOperation, len(rebuilt.Operations))
		for i := range operations {
			operations[i] = SavedOperation{ID: stateID(), Status: "prepared"}
		}
		saved.Pending = &Batch{ID: stateID(), ProjectRevision: projectRevision, Plan: rebuilt, Options: options, Local: local, Remote: remote, Operations: operations}
		return nil
	}, false)
}

// StartOperation is the durable point of no blind retry. If publication or the
// caller dies afterwards, started remains until explicit receipt/rescan review.
func (s *StateStore) StartOperation(id string, revision int64, operationID string) (SavedBinding, error) {
	return s.change(id, revision, func(saved *SavedBinding) error {
		if saved.Pending == nil {
			return ErrStateChanged
		}
		for i, op := range saved.Pending.Operations {
			if op.Status == "verified" {
				continue
			}
			if op.ID != operationID || op.Status != "prepared" {
				return ErrPending
			}
			saved.Pending.Operations[i].Status = "started"
			return nil
		}
		return ErrStateChanged
	}, false)
}

// VerifyOperation records caller-verified publication. Receipt alone is not a
// baseline commit: Commit still requires complete post-execution scans.
func (s *StateStore) VerifyOperation(id string, revision int64, operationID string, recovery *syncfs.Recovery) (SavedBinding, error) {
	return s.change(id, revision, func(saved *SavedBinding) error {
		if saved.Pending == nil {
			return ErrStateChanged
		}
		for i, op := range saved.Pending.Operations {
			if op.Status == "verified" {
				continue
			}
			if op.ID != operationID || op.Status != "started" {
				return ErrPending
			}
			saved.Pending.Operations[i].Status = "verified"
			saved.Pending.Operations[i].Recovery = recovery
			return nil
		}
		return ErrStateChanged
	}, false)
}
func sameManifest(a, b syncproto.Manifest) bool {
	x, e := a.Digest()
	y, f := b.Digest()
	return e == nil && f == nil && x == y
}
func projected(p *Batch) (syncproto.Manifest, syncproto.Manifest, error) {
	clone := func(m syncproto.Manifest) syncproto.Manifest {
		n := m
		n.Entries = make(map[string]syncproto.Entry, len(m.Entries))
		for k, v := range m.Entries {
			n.Entries[k] = v
		}
		return n
	}
	local, remote := clone(p.Local), clone(p.Remote)
	for _, op := range p.Plan.Operations {
		target := &remote
		toLocal := op.Kind == "download" || strings.HasSuffix(op.Kind, "_local")
		if toLocal {
			target = &local
		}
		if op.After == nil {
			delete(target.Entries, op.Path)
		} else {
			e := *op.After
			if toLocal && !p.Options.LocalExecutable {
				e.Executable = false
			}
			target.Entries[op.Path] = e
		}
	}
	if local.Validate() != nil || remote.Validate() != nil {
		return local, remote, syncproto.ErrInvalid
	}
	return local, remote, nil
}
func (s *StateStore) Commit(id string, revision int64, local, remote syncproto.Manifest) (SavedBinding, error) {
	return s.change(id, revision, func(saved *SavedBinding) error {
		p := saved.Pending
		if p == nil {
			return ErrStateChanged
		}
		for _, op := range p.Operations {
			if op.Status != "verified" {
				return ErrPending
			}
		}
		wantLocal, wantRemote, err := projected(p)
		if err != nil {
			return err
		}
		if !sameManifest(local, wantLocal) || !sameManifest(remote, wantRemote) {
			return syncproto.ErrChanged
		}
		saved.Baseline = &syncproto.Baseline{Binding: saved.Binding, Local: local, Remote: remote}
		saved.Pending = nil
		return nil
	}, true)
}
func (s *StateStore) History(id, batchID string) (Batch, error) {
	var batch Batch
	if !syncproto.ValidOperationID(id) || !syncproto.ValidOperationID(batchID) {
		return batch, syncproto.ErrInvalid
	}
	if err := s.root.CheckIdentity(); err != nil {
		return batch, err
	}
	var raw []byte
	if err := s.db.QueryRow("SELECT state FROM batches WHERE binding=? AND id=?", id, batchID).Scan(&raw); err != nil {
		return batch, err
	}
	if len(raw) > maxStateJSON || json.Unmarshal(raw, &batch) != nil || batch.ID != batchID {
		return Batch{}, fmt.Errorf("invalid saved sync batch")
	}
	return batch, nil
}

// Bound logical metadata; SQLite may retain reusable free pages and a rollback
// journal. This is not a strict physical disk quota; disk-full errors still stop
// publication and leave the previous committed state authoritative.
func checkStateCapacity(tx *sql.Tx, replacing string, bytes int) error {
	var used int64
	if err := tx.QueryRow("SELECT (SELECT coalesce(sum(length(state)),0) FROM bindings WHERE id != ?) + (SELECT coalesce(sum(length(state)),0) FROM batches)", replacing).Scan(&used); err != nil {
		return err
	}
	if used+int64(bytes) > maxStoredMetadata {
		return syncproto.ErrLimit
	}
	return nil
}

// RecordRecovery persists the verified copy reference before local publication.
// A crash after this transaction can always locate the copy by operation ID.
func (s *StateStore) RecordRecovery(id string, revision int64, operationID string, recovery syncfs.Recovery) (SavedBinding, error) {
	return s.change(id, revision, func(saved *SavedBinding) error {
		if saved.Pending == nil {
			return ErrStateChanged
		}
		for i, op := range saved.Pending.Operations {
			if op.ID == operationID && op.Status == "started" {
				planned := saved.Pending.Plan.Operations[i]
				if planned.Kind != "download" && planned.Kind != "delete_local" || planned.Before == nil || planned.Before.Kind != "file" || recovery.Hash != planned.Before.Hash || recovery.Size != planned.Before.Size || !strings.HasPrefix(recovery.Path, ".agentbox-sync/recovery/") {
					return syncproto.ErrInvalid
				}
				if op.Recovery != nil && *op.Recovery != recovery {
					return ErrStateChanged
				}
				saved.Pending.Operations[i].Recovery = &recovery
				return nil
			}
		}
		return ErrStateChanged
	}, false)
}
