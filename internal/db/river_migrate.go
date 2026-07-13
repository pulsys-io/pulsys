// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// MigrateRiver applies every pending River schema migration against dsn.
// Idempotent: safe to call on every process start and in test bootstrap.
func MigrateRiver(ctx context.Context, dsn string) error {
	if dsn == "" {
		return fmt.Errorf("db: empty DSN")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("db: river migrate pool: %w", err)
	}
	defer pool.Close()
	return MigrateRiverPool(ctx, pool)
}

// MigrateRiverPool applies River migrations using an existing pool.
func MigrateRiverPool(ctx context.Context, pool *pgxpool.Pool) error {
	driver := riverpgxv5.New(pool)
	migrator, err := rivermigrate.New(driver, nil)
	if err != nil {
		return fmt.Errorf("db: river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("db: river migrate up: %w", err)
	}
	return nil
}
