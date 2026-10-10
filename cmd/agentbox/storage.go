package main

import (
	"agentbox/internal/clientidentity"
	"agentbox/internal/storagepolicy"
	"agentbox/internal/toolstore"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"io"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"agentbox/internal/config"
)

// Configure only after the operator has stopped writers and copied/verified
// any relocated data. No source data is deleted by this command.
func configureStorage(ctx context.Context, args []string, checkStopped func(context.Context, []string) error) error {
	flags := flag.NewFlagSet("storage-config", flag.ContinueOnError)
	path := flags.String("config", "config.json", "configuration path")
	policyPath := flags.String("policy", "", "storage policy JSON file")
	destination := flags.String("data-dir", "", "verified existing data destination (optional)")
	reserve := flags.Int64("min-free-bytes", -1, "minimum available bytes for new work")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *policyPath == "" {
		return errors.New("storage-config requires --policy")
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	if *destination == "" {
		*destination = c.DataDir
	}
	if *reserve < 0 {
		*reserve = c.GetResources().MinFreeBytes
	}
	var locks []*os.File
	defer func() {
		for _, f := range locks {
			f.Close()
		}
	}()
	roots := []string{c.DataDir}
	if *destination != c.DataDir {
		roots = append(roots, *destination)
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return errors.New("absolute clean data path required")
		}
		for current := root; ; current = filepath.Dir(current) {
			st, err := os.Lstat(current)
			if err != nil {
				return err
			}
			if st.Mode()&os.ModeSymlink != 0 {
				return errors.New("data path ancestry cannot be symlinked")
			}
			if current == filepath.Dir(current) {
				break
			}
		}
		f, err := os.OpenFile(filepath.Join(root, "agentbox.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		locks = append(locks, f)
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return errors.New("stop the service before changing storage policy")
		}
	}
	if err = checkStopped(ctx, roots); err != nil {
		return err
	}
	file, err := os.Open(*policyPath)
	if err != nil {
		return err
	}
	defer file.Close()
	var policy config.StoragePolicy
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&policy); err != nil {
		return err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing storage policy input")
	}
	return c.RelocateStorage(*destination, policy, *reserve)
}

func prepareStorage(ctx context.Context, args []string, checkStopped func(context.Context, []string) error) error {
	flags := flag.NewFlagSet("storage-prepare", flag.ContinueOnError)
	path := flags.String("config", "config.json", "configuration path")
	adopt := flags.Bool("adopt", false, "assign existing offline user trees to their project quotas")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*adopt || flags.NArg() != 0 {
		return errors.New("storage-prepare requires --adopt")
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	if c.GetStoragePolicy().Mount == "" {
		return errors.New("storage quotas are not configured")
	}
	f, err := os.OpenFile(filepath.Join(c.DataDir, "agentbox.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("stop service before adoption")
	}
	roots := []string{c.DataDir}
	if c.GetToolStorage().Root != "" {
		roots = append(roots, c.GetToolStorage().Root)
	}
	if err = checkStopped(ctx, roots); err != nil {
		return err
	}
	identity, err := clientidentity.LoadOrCreate(c.DataDir)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(c.DataDir, "state.db"))+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "select name,created_at from users")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var user, created string
		if err = rows.Scan(&user, &created); err != nil {
			return err
		}
		stamp, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return err
		}
		owner := toolstore.OwnerKey(identity, user, stamp.UTC().Format(time.RFC3339Nano))
		if err = storagepolicy.Adopt(ctx, c.GetStoragePolicy(), c.DataDir, c.GetToolStorage().Root, user, owner); err != nil {
			return err
		}
	}
	return rows.Err()
}
