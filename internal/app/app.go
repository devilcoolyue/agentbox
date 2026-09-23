// Package app owns the server lifetime and its exclusive data-directory lock.
// Business and HTTP packages do not depend on app.
package app

import (
	"context"
	"errors"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/server"
)

func Run(ctx context.Context, cfg *config.Config) (err error) {
	lock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	srv, err := server.NewContext(ctx, cfg)
	if err != nil {
		_ = lock.Close()
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		closeErr := srv.Close(closeCtx)
		err = errors.Join(err, closeErr)
		if closeErr != nil && closeCtx.Err() != nil {
			// Never unlock live data underneath a still-draining task. The CLI exits
			// on this error; embedded callers retain the lock until cleanup finishes.
			go func() { _ = srv.Close(context.Background()); _ = lock.Close() }()
		} else {
			err = errors.Join(err, lock.Close())
		}
	}()
	return srv.Run(ctx)
}
