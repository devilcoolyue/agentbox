package server

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/imageupdate"
	"agentbox/internal/modelcatalog"
)

// cliCatalogRetry spaces out sandbox runs after a failed or partial read; a
// complete read is kept for as long as the configured image stays the same.
const cliCatalogRetry = 5 * time.Minute

// Variables rather than functions so tests can stand in for Docker.
var (
	agentImageIdentity = func(ctx context.Context, s *Server) (imageupdate.Image, error) {
		if s.dock == nil {
			return imageupdate.Image{}, errors.New("Docker 不可用")
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return s.dock.InspectCLIImage(ctx, s.cfg.GetAgentImage())
	}
	readCLICatalog = func(ctx context.Context, s *Server, imageID string) ([]byte, error) {
		return s.dock.ReadCLICatalog(ctx, imageID)
	}
)

type cliCatalogCache struct {
	mu       sync.Mutex
	imageID  string
	models   map[string][]modelcatalog.Model
	complete bool
	err      error
	at       time.Time
	running  chan struct{}
}

// officialSource says where official model data came from.
type officialSource struct {
	Kind       string `json:"kind"`                  // cli | builtin
	CLIVersion string `json:"cli_version,omitempty"` // with kind=cli
	VerifiedAt string `json:"verified_at,omitempty"` // date of the compiled-in snapshot
	// kind=builtin because the image's CLIs could not be read; the reason is
	// only logged, never sent.
	Unavailable bool `json:"cli_unavailable,omitempty"`
}

// cliModels reads the catalogs of the configured image's CLIs once per image;
// concurrent callers share one sandbox run.
func (s *Server) cliModels(ctx context.Context) (map[string][]modelcatalog.Model, imageupdate.Image, error) {
	img, err := agentImageIdentity(ctx, s)
	if err != nil {
		log.Printf("models: 无法读取 Agent 镜像信息，官方模型目录改用内置快照: %v", err)
		return nil, img, err
	}
	c := &s.cliCatalog
	for {
		c.mu.Lock()
		if c.imageID == img.ID && c.running == nil && (c.complete || time.Since(c.at) < cliCatalogRetry) {
			models, err := c.models, c.err
			c.mu.Unlock()
			return models, img, err
		}
		if wait := c.running; wait != nil {
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, img, ctx.Err()
			}
		}
		done := make(chan struct{})
		c.running = done
		c.mu.Unlock()

		raw, err := readCLICatalog(ctx, s, img.ID)
		var models map[string][]modelcatalog.Model
		var failed map[string]string
		if err == nil {
			models, failed, err = modelcatalog.ParseCLI(raw)
		}
		if err != nil || len(failed) > 0 {
			log.Printf("models: Agent 镜像 %s 的 CLI 模型目录读取不完整: err=%v failed=%v", img.ID, err, failed)
		}
		c.mu.Lock()
		c.running = nil
		close(done)
		// A cancelled request says nothing about the image; the next one retries.
		if ctx.Err() == nil {
			c.imageID, c.models, c.err, c.at = img.ID, models, err, time.Now()
			c.complete = err == nil && len(failed) == 0
		}
		c.mu.Unlock()
		return models, img, err
	}
}

// officialModels is the official catalog for one agent type: the CLI's own
// list merged over the compiled-in snapshot, or the snapshot alone when the
// image cannot be read.
func (s *Server) officialModels(ctx context.Context, agentType string) ([]modelcatalog.Model, officialSource) {
	snapshot := modelcatalog.Builtin()
	src := officialSource{Kind: "builtin", VerifiedAt: snapshot.VerifiedAt}
	models, img, _ := s.cliModels(ctx)
	if cli := models[agentType]; len(cli) > 0 {
		src.Kind = "cli"
		src.CLIVersion = img.Codex
		if agentType == config.AgentClaude {
			src.CLIVersion = img.Claude
		}
		return modelcatalog.Merge(cli, snapshot.Models[agentType]), src
	}
	src.Unavailable = true
	return snapshot.Models[agentType], src
}
