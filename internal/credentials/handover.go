package credentials

import (
	"context"
	"path/filepath"

	"agentbox/internal/agent"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

// Release 把已停止空间的凭证交还给它当前绑定的账号，再由 rebind 改绑到别的账号。
//
// 顺序不能动，全程持有旧账号的锁：
//  1. 先按日常同步把空间里可能续过期的新令牌收回旧账号池——刷新令牌是轮换制，
//     直接删掉的话旧账号池里那份已经作废，别的空间跟着掉登录；
//  2. 再清掉 home 里的登录文件（agent.ClearCredentials）；
//  3. 最后 rebind 写库。锁一直拿到写库之后，旧账号的播发就没机会把凭证再写回来；
//     写库之后 home 里没有登录文件，新账号的同步与播发都不会碰它，下次启动才由
//     SeedCredentials 铺上新账号的凭证。
//
// 撤权的空间不参与收回（同 syncRotatingCred），删掉的就是它手里那份。
func (s *Service) Release(ctx context.Context, sess store.Session, rebind func() error) error {
	release, err := s.Lock(ctx, sess.AccountID)
	if err != nil {
		return err
	}
	defer release()
	if acct, ok := s.cfg.Account(sess.AccountID); ok && acct.CredentialsDir != "" && acct.Type == sess.Agent {
		s.syncRotatingCred(acct, sess)
	}
	home := filepath.Join(s.cfg.DataDir, "users", sess.User, "sessions", sess.ID, "home")
	if err := agent.ClearCredentials(sess.Agent, home, dockerx.AgentUID, dockerx.AgentGID); err != nil {
		return err
	}
	return rebind()
}
