package imageupdate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agentbox/internal/config"
)

// ImageRecord is local provenance keyed by immutable image ID. We deliberately
// do not trust inherited Docker labels: a manually extended image is a new
// migration source. RetiredAt starts a seven-day grace period, not image age.
type ImageRecord struct {
	Base      string `json:"base,omitempty"`
	RetiredAt int64  `json:"retired_at,omitempty"`
}

// maintain uses the same single-flight job and activation gate as normal CLI
// updates. Unknown images migrate at identical versions, even with no upstream
// release. An explicit rollback pauses maintenance for that image.
func (s *Service) maintain(ctx context.Context, img Image, policy config.ImageUpdateConfig, expected string) error {
	s.mu.Lock()
	record, known := s.status.Images[img.ID]
	paused := s.status.MaintenancePaused == img.ID
	s.mu.Unlock()
	if paused {
		s.log("已回退的镜像暂停自动维护；手动更新可重新迁移。\n")
		return nil
	}
	if known && record.Base != "" {
		if _, err := s.backend.InspectCLIImage(ctx, record.Base); err != nil {
			return fmt.Errorf("固定底座不可用，拒绝叠加更新: %w", err)
		}
		return s.collect(ctx)
	}
	if !ValidVersion(img.Claude) || !ValidVersion(img.Codex) {
		return errors.New("旧镜像缺少有效 CLI 版本，无法自动维护")
	}
	s.mu.Lock()
	s.status.Phase = "migrating"
	s.status.Target = Versions{Claude: img.Claude, Codex: img.Codex}
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.log("正在整理旧镜像；CLI 版本保持不变，运行中的空间不受影响。\n")
	// Capture only provably related updater tags. Arbitrary/custom image tags are
	// never automatically adopted into the cleanup set.
	legacy, err := s.backend.LegacyCLIImages(ctx, img.ID)
	if err != nil {
		return err
	}
	flat, err := s.backend.FlattenCLIImage(ctx, img, s.log)
	if err != nil {
		return err
	}
	if flat.Claude != img.Claude || flat.Codex != img.Codex || flat.Browser != img.Browser || flat.User != img.User {
		return errors.New("迁移后镜像元数据变化，未切换")
	}
	if err = s.validate(ctx, flat.ID); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Provenance is durable BEFORE config CAS. A crash before CAS reuses the
	// deterministic staging image; after CAS it already knows the correct base.
	s.mu.Lock()
	if s.status.Images == nil {
		s.status.Images = map[string]ImageRecord{}
	}
	s.status.Images[flat.ID] = ImageRecord{Base: flat.ID, RetiredAt: time.Now().UnixMilli()}
	now := time.Now().UnixMilli()
	for _, id := range legacy {
		if _, ok := s.status.Images[id]; !ok {
			s.status.Images[id] = ImageRecord{RetiredAt: now}
		}
	}
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err = s.cfg.SwitchAgentImage(expected, policy, flat.ID, img.ID, false); err != nil {
		return err
	}
	s.mu.Lock()
	s.status.ResultImage = flat.ID
	s.mu.Unlock()
	s.log("旧镜像整理完成；新启动空间使用固定底座，后续任务再检查 CLI 新版本。\n")
	return nil
}

func (s *Service) collect(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s.mu.Lock()
	records := make(map[string]ImageRecord, len(s.status.Images))
	for id, v := range s.status.Images {
		records[id] = v
	}
	s.mu.Unlock()
	var removed []string
	// Config holds a read lock throughout each collection pass, preventing a
	// simultaneous settings edit/rollback from selecting an image being removed.
	err := s.cfg.WithImageReferences(func(current, previous string) error {
		active, err := s.backend.InspectCLIImage(ctx, current)
		if err != nil {
			return err
		}
		protected := map[string]bool{current: true, previous: true, active.ID: true}
		if previous != "" {
			old, e := s.backend.InspectCLIImage(ctx, previous)
			if e != nil {
				return e
			}
			protected[old.ID] = true
		}
		for id, v := range records {
			if v.Base != "" && (id != v.Base || protected[id]) {
				protected[v.Base] = true
			}
		}
		cutoff := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
		for id, v := range records {
			if protected[id] || v.RetiredAt == 0 || v.RetiredAt > cutoff {
				continue
			}
			ok, e := s.backend.RemoveCLIImage(ctx, id, protected)
			if ok {
				removed = append(removed, id)
			}
			if e != nil {
				return e
			}
		}
		return nil
	})
	if len(removed) > 0 {
		s.mu.Lock()
		for _, id := range removed {
			delete(s.status.Images, id)
		}
		persistErr := s.persistLocked()
		s.mu.Unlock()
		err = errors.Join(err, persistErr)
		s.log(fmt.Sprintf("已回收 %d 个无容器引用且超过保留期的更新镜像。\n", len(removed)))
	}
	return err
}
