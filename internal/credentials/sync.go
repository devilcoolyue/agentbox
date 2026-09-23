package credentials

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

// syncRotatingCred 让池子与该会话 home 的轮换凭证文件收敛到较新的一份。
func (s *Service) syncRotatingCred(acct config.Account, sess store.Session) {
	// Re-read policy: callers may hold a snapshot taken before an admin edit.
	current, ok := s.cfg.Account(acct.ID)
	if !ok || !s.allowed(current, sess.User) {
		return
	}
	acct = current
	poolName, homeRel := agent.RotatingCredFile(sess.Agent)
	pool, err := safefs.Open(acct.CredentialsDir)
	if err != nil {
		return
	}
	defer pool.Close()
	home, err := s.openHome(sess)
	if err != nil {
		return
	}
	defer home.Close()
	poolData, pi, perr := readCredential(pool, poolName)
	homeData, hi, herr := readCredential(home, filepath.FromSlash(homeRel))
	if herr != nil {
		return
	} // unseeded or unsafe home: never read outside it
	if perr != nil {
		if os.IsNotExist(perr) {
			syncCredential(pool, poolName, homeData, 0, hi.ModTime())
		}
		return
	}
	diff := hi.ModTime().Sub(pi.ModTime())
	switch {
	case diff > 2*time.Second:
		if !refreshTokenRe.Match(homeData) && refreshTokenRe.Match(poolData) {
			return
		}
		syncCredential(pool, poolName, homeData, 0, hi.ModTime())
	case diff < -2*time.Second:
		if !refreshTokenRe.Match(poolData) && refreshTokenRe.Match(homeData) {
			return
		}
		syncCredential(home, filepath.FromSlash(homeRel), poolData, dockerx.AgentUID, pi.ModTime())
	}
}

var refreshTokenRe = regexp.MustCompile(`"refreshToken"\s*:\s*"[^"]`)

const credentialMaxBytes = 4 << 20

func readCredential(root *safefs.Root, name string) ([]byte, os.FileInfo, error) {
	f, err := root.OpenFile(name)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, credentialMaxBytes+1))
	if err == nil && len(raw) > credentialMaxBytes {
		err = fmt.Errorf("credential file exceeds size limit")
	}
	return raw, info, err
}

func syncCredential(root *safefs.Root, name string, raw []byte, uid int, mtime time.Time) {
	_, err := root.WriteFile(name, raw, safefs.WriteOptions{Mode: 0o600, Chown: uid != 0, UID: uid, GID: dockerx.AgentGID, BestEffortChown: true, ModTime: mtime})
	if err != nil {
		log.Printf("credsync %s: %v", name, err)
	}
}
