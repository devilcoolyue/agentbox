package workspace

import (
	"agentbox/internal/store"
	"context"
	"errors"
	"fmt"
	"syscall"
)

var ErrCapacity = errors.New("容器容量不足")

// ErrDiskSpace remains a capacity refusal for existing callers.
var ErrDiskSpace = fmt.Errorf("%w：数据盘可用空间低于保留值", ErrCapacity)

// Caller holds the global start gate until Docker and the session record agree.
// Inspect every known container: saved status may be stale after an external stop.
func (s *Service) admit(ctx context.Context, current store.Session) error {
	limits := s.cfg.GetResources()
	if limits.MinFreeBytes > 0 {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(s.cfg.DataDir, &stat); err != nil {
			return err
		}
		if stat.Bavail*uint64(stat.Bsize) < uint64(limits.MinFreeBytes) {
			return ErrDiskSpace
		}
	}
	if limits.MaxRunning == 0 && limits.MaxRunningPerUser == 0 {
		return nil
	}
	total, owned := 0, 0
	for _, sess := range s.store.All() {
		if sess.ContainerID == "" {
			continue
		}
		running, err := s.dock.Running(ctx, sess.ContainerID)
		if err != nil {
			return fmt.Errorf("检查容器容量失败: %w", err)
		}
		if !running {
			continue
		}
		if sess.ID == current.ID {
			return nil
		} // reconnect never needs a new slot
		total++
		if sess.User == current.User {
			owned++
		}
	}
	if limits.MaxRunning > 0 && total >= limits.MaxRunning {
		return fmt.Errorf("%w：已达全局上限 %d", ErrCapacity, limits.MaxRunning)
	}
	if limits.MaxRunningPerUser > 0 && owned >= limits.MaxRunningPerUser {
		return fmt.Errorf("%w：已达用户上限 %d", ErrCapacity, limits.MaxRunningPerUser)
	}
	return nil
}
