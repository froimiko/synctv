package bootstrap

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/synctv-org/synctv/internal/db"
)

const embyGrantCleanupInterval = 15 * time.Minute

func cleanExpiredEmbyGrants(now time.Time, cleanup func(time.Time) error) {
	if err := cleanup(now); err != nil {
		log.WithField("category", "database_error").Error("emby grant cleanup failed")
	}
}

func runEmbyGrantCleanup(
	ctx context.Context,
	ticks <-chan time.Time,
	now func() time.Time,
	cleanup func(time.Time) error,
) {
	cleanExpiredEmbyGrants(now().UTC(), cleanup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			cleanExpiredEmbyGrants(now().UTC(), cleanup)
		}
	}
}

func InitEmbyGrantCleanup(ctx context.Context) error {
	ticker := time.NewTicker(embyGrantCleanupInterval)
	go func() {
		defer ticker.Stop()
		runEmbyGrantCleanup(ctx, ticker.C, time.Now, db.DeleteExpiredEmbyRootGrants)
	}()
	return nil
}
