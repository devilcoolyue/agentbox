package syncfs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"

	"agentbox/internal/syncproto"
)

// Mutations on one root are serialized; external editors are not locked. Each
// operation rechecks the expected bytes after staging and before publication.
// This is not an atomic compare-and-swap against arbitrary external processes.
type Writer struct {
	Root *Root
	// Guard is supplied by the executor to recheck its lease before publication.
	// A guard failure retains any recovery copy and never authorizes a mutation.
	Guard func(context.Context) error
	// RecoveryReady must durably record the recovery reference before any file
	// publication. If it fails, the previous user file remains intact.
	RecoveryReady func(Recovery) error
	mu            sync.Mutex
}

type Recovery struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// Replace stages and verifies the incoming file, preserves the replaced bytes,
// then atomically renames over the destination. A nil expected value requires
// absence. Errors leave the destination intact unless they happen after the
// final rename, in which case the caller must rescan instead of replaying.
// Parent directories must be prepared separately from a confirmed plan.
func (w *Writer) Replace(ctx context.Context, name string, expected *syncproto.Entry, desired syncproto.Entry, source io.Reader) (Recovery, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := mutationPath(name); err != nil {
		return Recovery{}, err
	}
	if desired.Kind != "file" || !syncproto.ValidHash(desired.Hash) || desired.Size < 0 || desired.Size > syncproto.DefaultMaxFileBytes {
		return Recovery{}, syncproto.ErrInvalid
	}
	if err := w.Root.CheckIdentity(); err != nil {
		return Recovery{}, err
	}
	p, leaf, err := w.Root.parent(name)
	if err != nil {
		return Recovery{}, err
	}
	defer p.Close()
	if err = checkExpected(ctx, p, leaf, expected); err != nil {
		return Recovery{}, err
	}
	temp := ".agentbox-sync-tmp-" + rand.Text()
	f, err := p.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return Recovery{}, err
	}
	defer f.Close()
	defer p.root.Remove(temp)
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(contextReader{ctx, source}, desired.Size+1))
	if err != nil {
		return Recovery{}, err
	}
	if n != desired.Size || hex.EncodeToString(hash.Sum(nil)) != desired.Hash {
		return Recovery{}, errors.New("download size or hash mismatch")
	}
	mode := os.FileMode(0644)
	if info, statErr := p.root.Lstat(leaf); statErr == nil && info.Mode().IsRegular() {
		mode = info.Mode().Perm() & 0777
	}
	if runtime.GOOS != "windows" {
		mode &^= 0111
		if desired.Executable {
			mode |= 0111
		}
	}
	if err = f.Chmod(mode); err != nil {
		return Recovery{}, err
	}
	if err = f.Sync(); err != nil {
		return Recovery{}, err
	}
	tempInfo, err := f.Stat()
	if err != nil {
		return Recovery{}, err
	}
	if err = f.Close(); err != nil {
		return Recovery{}, err
	}
	if err = w.Root.CheckIdentity(); err != nil {
		return Recovery{}, err
	}
	if err = checkExpected(ctx, p, leaf, expected); err != nil {
		return Recovery{}, err
	}
	recovery, err := backupBefore(ctx, w.Root, p, leaf, expected)
	if err != nil {
		return Recovery{}, err
	}
	if recovery.Path != "" && w.RecoveryReady != nil {
		if err = w.RecoveryReady(recovery); err != nil {
			return recovery, err
		}
	}
	publicationChecks := 0
	validate := func() error {
		publicationChecks++
		if err := w.beforePublish(ctx, name, p); err != nil {
			return err
		}
		if err := checkExpected(ctx, p, leaf, expected); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// A local process might have replaced our random staging name. Refuse
		// it again after any Windows sharing-mode wait, before trying to rename.
		staged, err := p.root.Lstat(temp)
		if err != nil || !staged.Mode().IsRegular() || !os.SameFile(tempInfo, staged) {
			return syncproto.ErrChanged
		}
		if publicationChecks > 1 {
			// The wait also gave other local processes time to edit the same
			// staging inode. Rehash before a retry, not only after publication.
			return checkExpected(ctx, p, temp, &desired)
		}
		return nil
	}
	if expected == nil {
		if err = validate(); err != nil {
			return recovery, err
		}
		// Link publishes without overwriting an entry created after preflight.
		// Remove the staging alias immediately; there is never an old file unlink.
		if err = p.root.Link(temp, leaf); err != nil {
			return recovery, err
		}
		if err = p.root.Remove(temp); err != nil {
			return recovery, err
		}
	} else if err = replaceStaged(ctx, p.root, temp, leaf, validate); err != nil {
		return recovery, err
	}
	if err = syncDirectory(p.root); err != nil {
		return recovery, err
	}
	if err = checkExpected(ctx, p, leaf, &desired); err != nil {
		return recovery, err
	}
	return recovery, nil
}

// Delete preserves the previous bytes and removes only an unchanged regular
// file. Directories require empty-directory handling; this never recurses.
func (w *Writer) Delete(ctx context.Context, name string, expected syncproto.Entry) (Recovery, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := mutationPath(name); err != nil {
		return Recovery{}, err
	}
	if expected.Kind != "file" {
		return Recovery{}, syncproto.ErrInvalid
	}
	if err := w.Root.CheckIdentity(); err != nil {
		return Recovery{}, err
	}
	p, leaf, err := w.Root.parent(name)
	if err != nil {
		return Recovery{}, err
	}
	defer p.Close()
	if err = checkExpected(ctx, p, leaf, &expected); err != nil {
		return Recovery{}, err
	}
	recovery, err := backupBefore(ctx, w.Root, p, leaf, &expected)
	if err != nil {
		return Recovery{}, err
	}
	if w.RecoveryReady != nil {
		if err = w.RecoveryReady(recovery); err != nil {
			return recovery, err
		}
	}
	if err = w.Root.CheckIdentity(); err != nil {
		return recovery, err
	}
	if err = w.beforePublish(ctx, name, p); err != nil {
		return recovery, err
	}
	if err = checkExpected(ctx, p, leaf, &expected); err != nil {
		return recovery, err
	}
	if err = ctx.Err(); err != nil {
		return recovery, err
	}
	info, err := p.root.Lstat(leaf)
	if err != nil || !info.Mode().IsRegular() {
		return recovery, syncproto.ErrChanged
	}
	if err = removeTyped(p.root, leaf, info, false); err != nil {
		return recovery, err
	}
	if err = syncDirectory(p.root); err != nil {
		return recovery, err
	}
	return recovery, checkExpected(ctx, p, leaf, nil)
}

func mutationPath(name string) error {
	rules, _ := syncproto.ParseRules("")
	if !syncproto.ValidPath(name) || rules.Ignored(name) {
		return ErrUnsafe
	}
	return nil
}

func checkExpected(ctx context.Context, root *Root, name string, expected *syncproto.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if expected == nil {
		_, err := root.root.Lstat(name)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return syncproto.ErrChanged
	}
	if expected.Kind != "file" || !syncproto.ValidHash(expected.Hash) || expected.Size < 0 || expected.Size > syncproto.DefaultMaxFileBytes {
		return syncproto.ErrInvalid
	}
	actual, err := root.hash(ctx, name, syncproto.DefaultMaxFileBytes)
	if err != nil {
		return err
	}
	if actual.Hash != expected.Hash || actual.Size != expected.Size || runtime.GOOS != "windows" && actual.Executable != expected.Executable {
		return syncproto.ErrChanged
	}
	return nil
}

// Recovery copies live beneath the mapped root, in an always-ignored directory.
// This lets a formerly populated child directory become empty and be removed.
// No automatic cleanup is performed;
// reaching the limit fails before replacing/deleting any user file.
const MaxRecoveryFiles = 1000
const MaxRecoveryBytes int64 = 256 << 20

func backupBefore(ctx context.Context, root, parent *Root, name string, expected *syncproto.Entry) (Recovery, error) {
	if expected == nil {
		return Recovery{}, nil
	}
	if err := root.root.Mkdir(".agentbox-sync", 0700); err != nil && !os.IsExist(err) {
		return Recovery{}, err
	}
	state, err := root.sub(".agentbox-sync")
	if err != nil {
		return Recovery{}, err
	}
	defer state.Close()
	if err = syncDirectory(root.root); err != nil {
		return Recovery{}, err
	}
	if err = state.root.Mkdir("recovery", 0700); err != nil && !os.IsExist(err) {
		return Recovery{}, err
	}
	recovery, err := state.sub("recovery")
	if err != nil {
		return Recovery{}, err
	}
	defer recovery.Close()
	if err = syncDirectory(state.root); err != nil {
		return Recovery{}, err
	}
	directory, err := recovery.root.Open(".")
	if err != nil {
		return Recovery{}, err
	}
	entries, readErr := directory.ReadDir(MaxRecoveryFiles + 1)
	directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return Recovery{}, readErr
	}
	if len(entries) >= MaxRecoveryFiles {
		return Recovery{}, syncproto.ErrLimit
	}
	var total int64
	for _, entry := range entries {
		info, err := recovery.root.Lstat(entry.Name())
		if err != nil {
			return Recovery{}, err
		}
		if !info.Mode().IsRegular() {
			return Recovery{}, ErrUnsafe
		}
		check, err := recovery.OpenFile(entry.Name())
		if err != nil {
			return Recovery{}, err
		}
		check.Close()
		total += info.Size()
		if total > MaxRecoveryBytes-expected.Size {
			return Recovery{}, syncproto.ErrLimit
		}
	}
	input, err := parent.OpenFile(name)
	if err != nil {
		return Recovery{}, err
	}
	defer input.Close()
	leaf := rand.Text() + ".bak"
	f, err := recovery.root.OpenFile(leaf, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Recovery{}, err
	}
	complete := false
	defer func() {
		f.Close()
		if !complete {
			_ = recovery.root.Remove(leaf)
		}
	}()
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(contextReader{ctx, input}, expected.Size+1))
	if err != nil {
		return Recovery{}, err
	}
	if n != expected.Size || hex.EncodeToString(hasher.Sum(nil)) != expected.Hash {
		return Recovery{}, syncproto.ErrChanged
	}
	if err = f.Sync(); err != nil {
		return Recovery{}, err
	}
	if err = f.Close(); err != nil {
		return Recovery{}, err
	}
	if err = syncDirectory(recovery.root); err != nil {
		return Recovery{}, err
	}
	complete = true
	return Recovery{Path: strings.Join([]string{".agentbox-sync", "recovery", leaf}, "/"), Hash: expected.Hash, Size: expected.Size}, nil
}

func (r Recovery) String() string { return fmt.Sprintf("%s (%d bytes)", r.Path, r.Size) }
