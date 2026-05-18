package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	DefaultRetryMax   = 30 * time.Second
	retryBackoffStart = 250 * time.Millisecond
	retryBackoffMax   = 2 * time.Second
)

// Open creates a pgxpool with exponential backoff retry for up to 30s.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool parse config: %w", err)
	}

	deadline := time.Now().Add(DefaultRetryMax)
	backoff := retryBackoffStart

	var pool *pgxpool.Pool
	for {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("pgxpool new: %w", err)
		}

		pingErr := pool.Ping(ctx)
		if pingErr == nil {
			slog.Info("db: connected to postgres", "status", "pool ready")
			return pool, nil
		}

		pool.Close()
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("db: postgres not ready after %v: %w", DefaultRetryMax, pingErr)
		}

		slog.Info("db: postgres not ready, retrying", "backoff", backoff, "error", pingErr)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > retryBackoffMax {
			backoff = retryBackoffMax
		}
	}
}
