package server

import (
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
	pool := filepath.Join(acct.CredentialsDir, poolName)
	home := filepath.Join(s.homeDir(sess), filepath.FromSlash(homeRel))

	pi, perr := os.Stat(pool)
	hi, herr := os.Stat(home)
	switch {
	case herr != nil && perr != nil:
		return
	case herr != nil:
		return // 会话尚未播种，SeedCredentials 会从池子拷入
	case perr != nil:
		s.copyCred(home, pool, 0, hi.ModTime()) // 首次发布到池子
		return
	}

	diff := hi.ModTime().Sub(pi.ModTime())
	switch {
	case diff > 2*time.Second:
		// 会话里刷新过 → 写回池子。刷新失败后 CLI 可能清空令牌字段，
		// 这种"残骸"不能覆盖池子里仍持有刷新令牌的文件。
		if !hasRefreshToken(home) && hasRefreshToken(pool) {
			return
		}
		s.copyCred(home, pool, 0, hi.ModTime())
	case diff < -2*time.Second:
		if !hasRefreshToken(pool) && hasRefreshToken(home) {
			return
		}
		s.copyCred(pool, home, dockerx.AgentUID, pi.ModTime())
	}
}

var refreshTokenRe = regexp.MustCompile(`"refreshToken"\s*:\s*"[^"]`)

// hasRefreshToken reports whether the file carries a non-empty refresh token.
// codex API-key auth.json has none on either side, so the guard never fires.
func hasRefreshToken(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return refreshTokenRe.Match(raw)
}

// copyCred 原子替换目标文件并把 mtime 对齐到来源，避免下轮重复拷贝。
// uid 非 0 时把目标 chown 给容器用户（会话 home 内的副本需要）。
func (s *Server) copyCred(src, dst string, uid int, mtime time.Time) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("credsync %s: %v", dst, err)
		return
	}
	if uid != 0 {
		_ = os.Chown(tmp, uid, dockerx.AgentGID)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		log.Printf("credsync %s: %v", dst, err)
		return
	}
	_ = os.Chtimes(dst, mtime, mtime)
}
