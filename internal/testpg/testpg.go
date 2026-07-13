// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package testpg

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pdb "github.com/pulsys-io/pulsys/internal/db"
)

// envDSN is the explicit DSN env var. Must support CREATEDB.
const envDSN = "PULSYS_TEST_PG_DSN"

// envTCImage opts in to the testcontainers-go fallback.
const envTCImage = "PULSYS_TEST_TC_IMAGE"

// state holds process-wide cached resources: the admin DSN, the
// template database name, and a guard so multiple Acquire calls boot
// the template at most once.
type state struct {
	mu        sync.Mutex
	adminDSN  string
	template  string
	bootErr   error
	booted    bool
	teardown  func()
	tcStarted bool
}

var globalState state

// Acquire returns a *pgxpool.Pool against a fresh, fully-migrated
// Postgres database. The database is unique to this Acquire call;
// t.Cleanup drops it.
//
// Acquire skips the test when no admin DSN is configured (neither
// PULSYS_TEST_PG_DSN nor PULSYS_TEST_TC_IMAGE is set), so
// `go test ./...` stays portable.
func Acquire(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if err := globalState.boot(t); err != nil {
		t.Fatalf("testpg: bootstrap: %v", err)
	}
	if globalState.adminDSN == "" {
		t.Skip("testpg: no admin DSN configured (set PULSYS_TEST_PG_DSN or PULSYS_TEST_TC_IMAGE)")
	}

	dbName := fmt.Sprintf("pulsys_t_%s", randomSuffix())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := withAdmin(ctx, globalState.adminDSN, func(p *pgxpool.Pool) error {
		_, err := p.Exec(ctx,
			fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, qident(dbName), qident(globalState.template)))
		return err
	}); err != nil {
		t.Fatalf("testpg: CREATE DATABASE from template: %v", err)
	}

	dsn := replaceDBName(globalState.adminDSN, dbName)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("testpg: pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// Drop the cloned DB. Don't fail the test if drop fails -
		// that just leaves a few empty DBs behind for a janitor.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		_ = withAdmin(ctx2, globalState.adminDSN, func(p *pgxpool.Pool) error {
			_, err := p.Exec(ctx2,
				fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, qident(dbName)))
			return err
		})
	})
	return pool
}

// AdminDSN returns the active admin DSN (post-boot), useful for
// helpers that need to run migrations against the cloned database.
// Empty when no admin is available.
func AdminDSN() string {
	globalState.mu.Lock()
	defer globalState.mu.Unlock()
	return globalState.adminDSN
}

// boot bootstraps the package on first call. Returns nil when the
// helper finishes (either with a configured admin or with an
// intentional "skip" — neither env var set).
func (s *state) boot(t *testing.T) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.booted {
		return s.bootErr
	}
	s.booted = true

	if dsn := strings.TrimSpace(os.Getenv(envDSN)); dsn != "" {
		s.adminDSN = dsn
	} else if img := strings.TrimSpace(os.Getenv(envTCImage)); img != "" {
		dsn, teardown, err := startTestContainer(img)
		if err != nil {
			s.bootErr = fmt.Errorf("testpg: testcontainers: %w", err)
			return s.bootErr
		}
		s.adminDSN = dsn
		s.teardown = teardown
		s.tcStarted = true
	} else {
		// Neither set: the tests will Skip().
		return nil
	}

	tmpl, err := ensureTemplate(s.adminDSN)
	if err != nil {
		s.bootErr = fmt.Errorf("testpg: ensure template: %w", err)
		return s.bootErr
	}
	s.template = tmpl
	return nil
}

// ensureTemplate creates (if missing) a template database whose name
// is keyed by the sha of all migration files, runs migrations into
// it, and marks it as a template. Returns the template database name.
//
// Cross-process safety: multiple `go test` packages run in separate
// processes; without coordination they would race on the initial
// CREATE DATABASE. We serialize on a Postgres session-level advisory
// lock keyed by the migration sha. The lock-holder creates +
// migrates + marks-template; everyone else waits on the lock and
// observes the finished template once it's released.
func ensureTemplate(adminDSN string) (string, error) {
	sha, err := migrationsSHA()
	if err != nil {
		return "", err
	}
	tmpl := "pulsys_tmpl_" + sha[:12]
	// Map the (hashed) name onto a stable int8 the advisory-lock API
	// expects. Hash entropy is overkill here; modulo to int63 range.
	lockKey := lockKeyFromSHA(sha)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Open a dedicated connection (NOT through the pool) so the
	// advisory lock lives for the full bootstrap and releases
	// implicitly on conn.Close.
	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		return "", err
	}
	cfg.MaxConns = 1
	holder, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer holder.Close()

	conn, err := holder.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Release()

	// Blocking acquire. Any other process trying to bootstrap the
	// same template waits here until we're done.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return "", fmt.Errorf("advisory_lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	// Re-check under the lock.
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, tmpl).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return tmpl, nil
	}

	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, qident(tmpl))); err != nil {
		return "", err
	}

	if err := pdb.Migrate(replaceDBName(adminDSN, tmpl), pdb.MigrateUp); err != nil {
		// Best-effort cleanup on failure so a retry starts clean.
		_, _ = conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, qident(tmpl)))
		return "", err
	}
	if err := pdb.MigrateRiver(ctx, replaceDBName(adminDSN, tmpl)); err != nil {
		_, _ = conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, qident(tmpl)))
		return "", err
	}

	if _, err := conn.Exec(ctx,
		fmt.Sprintf(`UPDATE pg_database SET datistemplate=true WHERE datname=%s`, qlit(tmpl))); err != nil {
		return "", err
	}
	return tmpl, nil
}

// lockKeyFromSHA derives a stable int64 advisory-lock key from the
// migrations sha. We mask off the sign bit so the value is a
// non-negative pg int8 (Postgres takes a single int8 here).
func lockKeyFromSHA(sha string) int64 {
	var v int64
	for i := 0; i < len(sha) && i < 16; i++ {
		c := sha[i]
		var d int64
		switch {
		case c >= '0' && c <= '9':
			d = int64(c - '0')
		case c >= 'a' && c <= 'f':
			d = int64(c-'a') + 10
		}
		v = (v << 4) | d
	}
	if v < 0 {
		v = ^v
	}
	return v
}

// migrationsSHA returns a deterministic sha256 over every migration
// file's name + contents. Used to derive the template DB name so a
// schema change automatically forces a fresh template.
func migrationsSHA() (string, error) {
	sub, err := pdb.MigrationsFS()
	if err != nil {
		return "", err
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// Stable order.
	sortStrings(names)
	h := sha256.New()
	for _, n := range names {
		_, _ = h.Write([]byte(n))
		_, _ = h.Write([]byte{0})
		b, err := fs.ReadFile(sub, n)
		if err != nil {
			return "", err
		}
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// withAdmin opens a transient pool to the admin DSN, runs fn, and
// closes the pool. Used for one-shot CREATE / DROP database calls
// that can't run from within a transaction on the target DB.
func withAdmin(ctx context.Context, dsn string, fn func(*pgxpool.Pool) error) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return err
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	return fn(pool)
}

// replaceDBName returns dsn with its database segment swapped to
// newName. Works for both postgres:// and postgresql:// schemes.
func replaceDBName(dsn, newName string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		// Fall back to a naive replacement; the tests will surface
		// any malformed DSN downstream.
		return dsn
	}
	u.Path = "/" + newName
	return u.String()
}

// qident quotes an SQL identifier (doubles embedded double-quotes).
func qident(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// qlit quotes an SQL string literal.
func qlit(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// randomSuffix returns 8 hex chars for unique database names.
func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sortStrings is a tiny sort.Strings shim that avoids importing the
// sort package solely for one slice.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
