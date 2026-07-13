// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package db

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// migrationFiles is the embedded directory of versioned SQL
// migrations.  It MUST be kept in sync with migrations/ on disk;
// new migrations are added as a new pair of files
//
//	migrations/<version>_<name>.up.sql
//	migrations/<version>_<name>.down.sql
//
// where <version> is a strictly monotonic integer.  golang-migrate
// fails loudly on gaps or duplicate versions, which is the
// behavior we want -- migrations are the database equivalent of
// load-bearing commit history and should not be silently
// reordered.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// MigrationsFS exposes the embedded migrations as a fs.FS for
// callers that want to introspect / list the migration set
// (e.g. cmd/pulsys-db's `migrate status`).  Returns a sub-FS
// rooted at the migrations directory so paths look like
// "0001_initial_schema.up.sql".
func MigrationsFS() (fs.FS, error) {
	return fs.Sub(migrationFiles, "migrations")
}

// MigrationDirection narrows the surface area of the public
// migration API: only Up and Down are supported.  Steps, force,
// goto are intentionally not exposed; callers that need them can
// drop down to the underlying golang-migrate API via the
// CLI tool (cmd/pulsys-db).
type MigrationDirection int

const (
	// MigrateUp applies every available migration above the
	// current version.
	MigrateUp MigrationDirection = iota
	// MigrateDown reverts every applied migration, leaving the
	// database empty of Pulsys schema.  Production deployments
	// should NEVER use this; it exists for tests and dev.
	MigrateDown
)

// Migrate runs the embedded migrations against the database
// identified by dsn.  It opens its own connection (golang-migrate
// requires a *sql.DB-style handle, not a pgxpool) and closes it
// before returning.
//
// The function is intentionally a one-shot: no callbacks, no
// progress reporting.  CLI consumers print "migrate: starting"
// before calling and "migrate: done" after.
func Migrate(dsn string, dir MigrationDirection) error {
	if dsn == "" {
		return errors.New("db: empty DSN")
	}
	sub, err := MigrationsFS()
	if err != nil {
		return fmt.Errorf("db: migrations fs: %w", err)
	}
	src, err := iofs.New(sub, ".")
	if err != nil {
		return fmt.Errorf("db: open embedded migration source: %w", err)
	}

	url, err := pgxMigrateURL(dsn)
	if err != nil {
		return fmt.Errorf("db: build migrate url: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return fmt.Errorf("db: open migrator: %w", err)
	}
	defer func() {
		// migrate.Migrate.Close returns (sourceErr, dbErr); we
		// log via the returned tuple but cannot return both, so
		// we drop them on the explicit-close path.  The schema
		// state is already committed by the time we reach this
		// defer.
		_, _ = m.Close()
	}()

	switch dir {
	case MigrateUp:
		err = m.Up()
	case MigrateDown:
		err = m.Down()
	default:
		return fmt.Errorf("db: unknown migration direction %d", dir)
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate: %w", err)
	}
	return nil
}

// MigrationVersion reports the current applied migration version
// and whether the database is in a "dirty" state (i.e. a previous
// migration failed mid-flight and the schema is not at a clean
// boundary).  Returns (0, false, nil) when no migration has
// ever been applied.
func MigrationVersion(dsn string) (version uint, dirty bool, err error) {
	if dsn == "" {
		return 0, false, errors.New("db: empty DSN")
	}
	sub, err := MigrationsFS()
	if err != nil {
		return 0, false, fmt.Errorf("db: migrations fs: %w", err)
	}
	src, err := iofs.New(sub, ".")
	if err != nil {
		return 0, false, fmt.Errorf("db: open embedded migration source: %w", err)
	}
	url, err := pgxMigrateURL(dsn)
	if err != nil {
		return 0, false, fmt.Errorf("db: build migrate url: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return 0, false, fmt.Errorf("db: open migrator: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	v, d, err := m.Version()
	if err != nil {
		if errors.Is(err, migrate.ErrNilVersion) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("db: version: %w", err)
	}
	return v, d, nil
}

// pgxMigrateURL rewrites a DSN into the form golang-migrate's
// pgx/v5 driver expects: scheme `pgx5://`.  Accepts either of:
//
//   - "postgres://user:pass@host:port/db?sslmode=..."
//   - "postgresql://..."
//   - "pgx5://..."        (already correct)
//
// Anything else is returned unchanged so an unknown scheme
// surfaces as a "driver not found" error from migrate itself.
func pgxMigrateURL(dsn string) (string, error) {
	switch {
	case len(dsn) > len("pgx5://") && dsn[:len("pgx5://")] == "pgx5://":
		return dsn, nil
	case len(dsn) > len("postgres://") && dsn[:len("postgres://")] == "postgres://":
		return "pgx5://" + dsn[len("postgres://"):], nil
	case len(dsn) > len("postgresql://") && dsn[:len("postgresql://")] == "postgresql://":
		return "pgx5://" + dsn[len("postgresql://"):], nil
	}
	return "", fmt.Errorf("db: unsupported DSN scheme (want postgres:// or postgresql://)")
}
