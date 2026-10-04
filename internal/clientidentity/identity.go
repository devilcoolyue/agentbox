// Package clientidentity owns the installation identity used by desktop sync.
// It is deliberately outside SQLite and the backup allowlist: normal restarts
// retain identity, while restored installations require a new client baseline.
package clientidentity

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"

	"agentbox/internal/safefs"
	"agentbox/internal/syncproto"
)

const File = "client-instance-id"

// LoadOrCreate accepts an administrator-controlled data directory. Publication
// is durable and no-replace; a malformed existing identity is never regenerated.
func LoadOrCreate(directory string) (string, error) {
	root, err := safefs.Open(directory)
	if err != nil {
		return "", err
	}
	defer root.Close()
	read := func() (string, error) {
		f, err := root.OpenFile(File)
		if err != nil {
			return "", err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return "", err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink != 1 {
			return "", errors.New("invalid desktop identity file")
		}
		raw, err := io.ReadAll(io.LimitReader(f, 66))
		if err != nil {
			return "", err
		}
		if len(raw) != 65 || raw[64] != '\n' || !syncproto.ValidHash(string(raw[:64])) {
			return "", errors.New("invalid desktop identity file")
		}
		return string(raw[:64]), nil
	}
	if id, err := read(); !os.IsNotExist(err) {
		return id, err
	}
	bytes := make([]byte, 32)
	if _, err = rand.Read(bytes); err != nil {
		return "", err
	}
	id := hex.EncodeToString(bytes)
	temp := ".client-instance-" + rand.Text()
	defer root.Remove(temp)
	if _, err = root.WriteAtomic(temp, strings.NewReader(id+"\n"), safefs.WriteOptions{Mode: 0600}); err != nil {
		return "", err
	}
	if err = root.RenameTo(temp, root, File, false); err != nil && !os.IsExist(err) {
		return "", err
	}
	if err = root.Sync(); err != nil {
		return "", err
	}
	return read()
}
