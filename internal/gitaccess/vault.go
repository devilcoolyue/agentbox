// Package gitaccess owns Git credentials and remote endpoint policy. It never
// executes repository code on the host.
package gitaccess

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"

	"agentbox/internal/safefs"
)

var ErrKey = errors.New("Git 凭证主密钥缺失或损坏，请恢复配套备份；不会生成新密钥覆盖已有凭证")
var ErrSecret = errors.New("Git 凭证无法解密，请检查主密钥与数据库是否来自同一份备份")

// Version 1 ciphertext uses master.key; version 2 embeds the immutable key ID.
// Rotation retains old keys, so interrupted DB/config rewrites and old backups
// remain readable. The keyring is never mounted into user containers.
type Vault struct {
	DataDir string
	mu      sync.Mutex
}
type keyring struct {
	Version int               `json:"version"`
	Active  string            `json:"active"`
	Keys    map[string][]byte `json:"keys"`
}

func readKeyring(root *safefs.Root) (keyring, error) {
	var ring keyring
	raw, err := root.ReadAll("git-secrets/keyring.json", 1<<20)
	if err != nil {
		return ring, err
	}
	if json.Unmarshal(raw, &ring) != nil || ring.Version != 1 || !validKeyID(ring.Active) || len(ring.Keys) == 0 || len(ring.Keys) > 1024 {
		return ring, ErrKey
	}
	for id, key := range ring.Keys {
		if !validKeyID(id) || len(key) != 32 {
			return ring, ErrKey
		}
	}
	if len(ring.Keys[ring.Active]) != 32 {
		return ring, ErrKey
	}
	return ring, nil
}
func validKeyID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func keyAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrKey
	}
	return cipher.NewGCM(block)
}
func (v *Vault) key(create bool, id string) ([]byte, string, error) {
	root, err := safefs.Open(v.DataDir)
	if err != nil {
		return nil, "", ErrKey
	}
	defer root.Close()
	ring, ringErr := readKeyring(root)
	if ringErr == nil {
		if id == "" {
			id = ring.Active
		}
		if id != "legacy" {
			key, ok := ring.Keys[id]
			if !ok {
				return nil, "", ErrKey
			}
			return key, id, nil
		}
	} else if !os.IsNotExist(ringErr) {
		return nil, "", ErrKey
	} else if id != "" && id != "legacy" {
		return nil, "", ErrKey
	}
	key, err := root.ReadAll("git-secrets/master.key", 32)
	if os.IsNotExist(err) && create && id == "" {
		if err = root.MkdirAll("git-secrets", 0700); err != nil {
			return nil, "", ErrKey
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, "", err
		}
		if _, err = root.WriteFile("git-secrets/master.key", key, safefs.WriteOptions{Mode: 0600}); err != nil {
			return nil, "", ErrKey
		}
	}
	if err != nil || len(key) != 32 {
		return nil, "", ErrKey
	}
	return key, "legacy", nil
}
func (v *Vault) Seal(secret, associated []byte, createKey bool) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	key, id, err := v.key(createKey, "")
	if err != nil {
		return nil, err
	}
	aead, err := keyAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	prefix := []byte{1}
	if id != "legacy" {
		prefix = append([]byte{2}, []byte(id)...)
	}
	return append(prefix, aead.Seal(nonce, nonce, secret, associated)...), nil
}
func (v *Vault) Open(sealed, associated []byte) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(sealed) == 0 {
		return nil, ErrSecret
	}
	id, offset := "legacy", 1
	if sealed[0] == 2 {
		if len(sealed) < 33 {
			return nil, ErrSecret
		}
		id = string(sealed[1:33])
		offset = 33
		if !validKeyID(id) {
			return nil, ErrSecret
		}
	} else if sealed[0] != 1 {
		return nil, ErrSecret
	}
	key, _, err := v.key(false, id)
	if err != nil {
		return nil, err
	}
	aead, err := keyAEAD(key)
	if err != nil {
		return nil, err
	}
	n := aead.NonceSize()
	if len(sealed) < offset+n+aead.Overhead() {
		return nil, ErrSecret
	}
	plain, err := aead.Open(nil, sealed[offset:offset+n], sealed[offset+n:], associated)
	if err != nil {
		return nil, ErrSecret
	}
	return plain, nil
}

// Rotate must run under the instance data_dir lock with server stopped. Adding
// a key is atomic and never removes a key needed by partially rewritten data.
func (v *Vault) Rotate() (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	root, err := safefs.Open(v.DataDir)
	if err != nil {
		return "", ErrKey
	}
	defer root.Close()
	ring, err := readKeyring(root)
	if os.IsNotExist(err) {
		ring = keyring{Version: 1, Keys: map[string][]byte{}}
	} else if err != nil {
		return "", ErrKey
	}
	if len(ring.Keys) >= 1024 {
		return "", errors.New("Git 密钥版本达到上限，请先迁移或归档旧实例")
	}
	key, idBytes := make([]byte, 32), make([]byte, 16)
	if _, err = rand.Read(key); err != nil {
		return "", err
	}
	if _, err = rand.Read(idBytes); err != nil {
		return "", err
	}
	id := hex.EncodeToString(idBytes)
	ring.Keys[id] = key
	ring.Active = id
	if err = root.MkdirAll("git-secrets", 0700); err != nil {
		return "", ErrKey
	}
	raw, err := json.Marshal(ring)
	if err != nil {
		return "", err
	}
	if _, err = root.WriteFile("git-secrets/keyring.json", raw, safefs.WriteOptions{Mode: 0600}); err != nil {
		return "", ErrKey
	}
	return id, nil
}
func (v *Vault) ActiveKeyID() (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, id, err := v.key(false, "")
	return id, err
}
