package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"agentbox/internal/config"
	"agentbox/internal/gitaccess"
	"agentbox/internal/store"
)

func rotateGitKeys(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("git-key-rotate", flag.ContinueOnError)
	cfgPath := flags.String("config", "config.json", "configuration path")
	resume := flags.Bool("resume", false, "finish rewriting with current active key after an interrupted rotation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	lockedDir := cfg.DataDir
	lock, err := os.OpenFile(filepath.Join(lockedDir, "agentbox.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("Git 密钥轮换需要先停止 agentbox 服务；数据目录仍被使用")
	}
	// Reload after locking, so startup/config writes cannot race this snapshot.
	cfg, err = config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.DataDir != lockedDir {
		return errors.New("配置的数据目录已变更，请重新运行 Git 密钥轮换")
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	vault := &gitaccess.Vault{DataDir: cfg.DataDir}
	// Validate everything before activating a new key; missing legacy material
	// must never be hidden by an apparently successful replacement key.
	transform := func(c store.GitConnection) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, err := vault.Open(c.Secret, c.AssociatedData())
		return c.Secret, err
	}
	if _, err = st.RekeyGitCredentials(transform); err != nil {
		return err
	}
	for _, app := range cfg.GitOAuthAppList() {
		if _, err = vault.Open(app.Secret, app.AssociatedData()); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var id string
	if *resume {
		id, err = vault.ActiveKeyID()
		if err == nil && id == "legacy" {
			err = errors.New("没有待恢复的版本化 Git 密钥轮换")
		}
	} else {
		id, err = vault.Rotate()
	}
	if err != nil {
		return err
	}
	count, err := st.RekeyGitCredentials(func(c store.GitConnection) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		plain, err := vault.Open(c.Secret, c.AssociatedData())
		if err != nil {
			return nil, err
		}
		return vault.Seal(plain, c.AssociatedData(), false)
	})
	if err != nil {
		return fmt.Errorf("Git 连接重加密中断，旧密钥已保留，可用 --resume 重试: %w", err)
	}
	err = cfg.RekeyGitOAuthApps(func(a config.GitOAuthApp) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		plain, err := vault.Open(a.Secret, a.AssociatedData())
		if err != nil {
			return nil, err
		}
		return vault.Seal(plain, a.AssociatedData(), false)
	})
	if err != nil {
		return fmt.Errorf("Git OAuth 应用重加密中断，连接仍可解密，可用 --resume 重试: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"active_key_id": id, "connections": count, "oauth_apps": len(cfg.GitOAuthAppList()), "retained_old_keys": true, "note": "轮换完成；旧密钥保留用于中断恢复与历史备份，未删除。"})
}
