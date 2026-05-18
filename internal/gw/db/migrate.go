package db

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// Migrate runs all pending up migrations from an embedded fs.FS directory.
func Migrate(ctx context.Context, pool *pgxpool.Pool, src fs.FS, dir string) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	source, err := iofs.New(src, dir)
	if err != nil {
		return fmt.Errorf("migrate: create iofs source: %w", err)
	}

	driver, err := pgx.WithInstance(sqlDB, &pgx.Config{})
	if err != nil {
		return fmt.Errorf("migrate: create pgx driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "pgx", driver)
	if err != nil {
		return fmt.Errorf("migrate: new instance: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migrate: up: %w", err)
	}

	ver, dirty, err := m.Version()
	if err != nil && err != migrate.ErrNilVersion {
		return fmt.Errorf("migrate: version: %w", err)
	}
	slog.Info("db: migrations applied", "version", ver, "dirty", dirty)
	return nil
}

// MigrateDown rolls back the last N migrations.
// N=0 means all; N=1 means one step.
func MigrateDown(ctx context.Context, pool *pgxpool.Pool, src fs.FS, dir string, steps int) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	source, err := iofs.New(src, dir)
	if err != nil {
		return fmt.Errorf("migrate: create iofs source: %w", err)
	}

	driver, err := pgx.WithInstance(sqlDB, &pgx.Config{})
	if err != nil {
		return fmt.Errorf("migrate: create pgx driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "pgx", driver)
	if err != nil {
		return fmt.Errorf("migrate: new instance: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if steps == 0 {
		if err := m.Down(); err != nil && err != migrate.ErrNoChange {
			return fmt.Errorf("migrate: down: %w", err)
		}
	} else {
		if err := m.Steps(-steps); err != nil && err != migrate.ErrNoChange {
			return fmt.Errorf("migrate: steps -%d: %w", steps, err)
		}
	}

	slog.Info("db: migration rolled back", "steps", steps)
	return nil
}
