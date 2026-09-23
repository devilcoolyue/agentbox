package server

import (
	"agentbox/internal/safefs"
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
	"agentbox/internal/store"
)

// OAuth 刷新令牌是轮换制：任何一份副本刷新成功都会作废旧令牌。因此账号池
// 里的凭证文件与各会话 home 里的副本必须保持在同一条令牌链上。这里做
// 「谁新用谁」的双向收敛：会话里 CLI 刷新后写回池子，池子更新后播发给
// 会话。触发点有两个：每次 startSession（含每轮对话/终端连接前），以及
// 兜底的周期循环（覆盖交互终端里长时间运行导致的中途刷新）。
const credSyncInterval = 45 * time.Second

func (s *Server) credSyncLoop() {
	for {
		time.Sleep(credSyncInterval)
		for _, sess := range s.store.All() {
			acct, ok := s.cfg.Account(sess.AccountID)
			if !ok || acct.CredentialsDir == "" {
				continue
			}
			s.syncRotatingCred(acct, sess)
		}
	}
}

// syncRotatingCred 让池子与该会话 home 的轮换凭证文件收敛到较新的一份。
func (s *Server) syncRotatingCred(acct config.Account, sess store.Session) {
	poolName, homeRel := agent.RotatingCredFile(sess.Agent)
	pool, err := safefs.Open(acct.CredentialsDir)
	if err != nil {
		return
	}
	defer pool.Close()
	home, err := s.openDataDir(s.homeDir(sess))
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
