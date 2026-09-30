package config

import (
	"fmt"
	"regexp"
)

// ImageUpdateConfig controls the in-process, daily CLI image updater.
type ImageUpdateConfig struct {
	Enabled     bool   `json:"enabled"`
	Channel     string `json:"channel"`
	Time        string `json:"time"`
	UpdateCodex bool   `json:"update_codex"`
}

func (p ImageUpdateConfig) normalized() ImageUpdateConfig {
	if p.Channel == "" {
		p.Channel = "stable"
	}
	if p.Time == "" {
		p.Time = "04:00"
	}
	return p
}

var updateTimeRE = regexp.MustCompile(`^(?:[01][0-9]|2[0-3]):[0-5][0-9]$`)

func (p ImageUpdateConfig) validate() error {
	if p.Channel != "stable" && p.Channel != "latest" {
		return fmt.Errorf("Claude 更新渠道必须为 stable 或 latest")
	}
	if !updateTimeRE.MatchString(p.Time) {
		return fmt.Errorf("更新检查时间必须为 HH:MM（00:00–23:59）")
	}
	return nil
}

func (c *Config) ImageUpdateState() (ImageUpdateConfig, string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ImageUpdates.normalized(), c.AgentImage, c.PreviousImage
}

// SwitchAgentImage commits the image and rollback pointer together. A job must
// never overwrite settings changed by an administrator while it was building.
func (c *Config) SwitchAgentImage(expected string, policy ImageUpdateConfig, next, previous string, rollback bool) error {
	return c.mutate(func(w *Config) error {
		if w.AgentImage != expected || w.ImageUpdates.normalized() != policy {
			return fmt.Errorf("更新期间镜像或更新策略已变化，未切换镜像，请重新检查")
		}
		w.AgentImage, w.PreviousImage = next, previous
		if rollback {
			w.ImageUpdates.Enabled = false
		}
		return nil
	})
}
