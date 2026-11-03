package cluster

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Advisory lock IDs. 727001 is used by the database migrations.
const (
	LockScheduler int64 = 727101
)

// RunAsLeader runs fn on exactly one server of the cluster at a time. It holds a
// PostgreSQL session advisory lock on a dedicated connection; when that connection
// breaks (server crash, network split) the database releases the lock and another
// server takes over within about 15 seconds. fn must return when its context ends.
func RunAsLeader(ctx context.Context, pool *pgxpool.Pool, lock int64, name string, fn func(ctx context.Context)) {
	for ctx.Err() == nil {
		if lead(ctx, pool, lock, name, fn) {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(15 * time.Second):
		}
	}
}

// lead returns true when it held the lock (so the caller retries at once).
func lead(ctx context.Context, pool *pgxpool.Pool, lock int64, name string, fn func(ctx context.Context)) bool {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lock).Scan(&got); err != nil || !got {
		conn.Release()
		return false
	}
	slog.Info("this server is now the leader", "job", name)
	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(lctx)
	}()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-done:
			break loop
		case <-t.C:
			pctx, pc := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Ping(pctx)
			pc()
			if err != nil {
				slog.Warn("lost leadership: database connection failed", "job", name, "err", err)
				break loop
			}
		}
	}
	cancel()
	<-done
	// Destroy rather than release: closing the session is what frees the lock even
	// when the unlock call cannot reach the database.
	conn.Conn().Close(context.Background())
	conn.Release()
	slog.Info("leadership released", "job", name)
	return ctx.Err() == nil
}
