// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDSN returns a DSN to a fresh, empty Postgres database owned by
// this test. The database is dropped by t.Cleanup, so multiple
// `internal/db` tests no longer race on a single shared schema —
// which was the source of the v=4-after-down flake under
// `-race -count=10` once `internal/admin/store` and
// `internal/auth/store` started running in parallel against the same
// admin DSN.
//
// We can't lean on internal/testpg here without an import cycle
// (testpg imports internal/db for Migrate). The dance is small
// enough to inline.
//
// Skips the test if PULSYS_TEST_PG_DSN is not set.
func testDSN(t *testing.T) string {
	t.Helper()
	admin := strings.TrimSpace(os.Getenv("PULSYS_TEST_PG_DSN"))
	if admin == "" {
		t.Skip("PULSYS_TEST_PG_DSN not set; skipping Postgres integration test")
	}

	suffix := randomSuffix()
	dbName := "pulsys_dbtest_" + suffix

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, dbName)); err != nil {
		pool.Close()
		t.Fatalf("create db: %v", err)
	}
	pool.Close()

	t.Cleanup(func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		p, err := pgxpool.New(ctx2, admin)
		if err != nil {
			return
		}
		defer p.Close()
		_, _ = p.Exec(ctx2, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, dbName))
	})

	return replaceDBName(admin, dbName)
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func replaceDBName(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

// ---------------------------------------------------------------------------
// MigrationsFS / embed.FS surface
// ---------------------------------------------------------------------------

// TestMigrationsFSEmbeddedFiles guards the embed.FS that ships
// migrations into the binary.  This test does NOT need a live
// database; it confirms 0001_initial_schema (up + down) is
// reachable via MigrationsFS so the AMI bootstrap can't be
// shipped with an empty migration set.
func TestMigrationsFSEmbeddedFiles(t *testing.T) {
	sub, err := MigrationsFS()
	if err != nil {
		t.Fatalf("MigrationsFS: %v", err)
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	want := map[string]bool{
		"0001_initial_schema.up.sql":          false,
		"0001_initial_schema.down.sql":        false,
		"0002_auth_oidc.up.sql":               false,
		"0002_auth_oidc.down.sql":             false,
		"0003_drop_oidc_auth_states.up.sql":   false,
		"0003_drop_oidc_auth_states.down.sql": false,
		"0004_registry.up.sql":                false,
		"0004_registry.down.sql":              false,
	}
	for _, e := range entries {
		if _, ok := want[e.Name()]; ok {
			want[e.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing embedded migration: %s", name)
		}
	}
}

// ---------------------------------------------------------------------------
// pgxMigrateURL: pure-function, no DB needed
// ---------------------------------------------------------------------------

func TestPgxMigrateURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"postgres://u:p@h:5432/d?sslmode=disable", "pgx5://u:p@h:5432/d?sslmode=disable", false},
		{"postgresql://u:p@h:5432/d", "pgx5://u:p@h:5432/d", false},
		{"pgx5://u:p@h:5432/d", "pgx5://u:p@h:5432/d", false},
		{"mysql://x@y/z", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := pgxMigrateURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Integration: pool + migrations against a live Postgres
// ---------------------------------------------------------------------------

// TestNewPoolHealth verifies that NewPool dials the configured
// DSN, the resulting Health() returns Healthy with a sane
// latency, and Close shuts the pool down cleanly.
func TestNewPoolHealth(t *testing.T) {
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := NewPool(ctx, Config{DSN: dsn, AppName: "pulsys-test"})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	res := p.Health(ctx)
	if !res.Healthy {
		t.Fatalf("Health: %v", res.Err)
	}
	if res.Latency <= 0 {
		t.Fatalf("Health: zero latency")
	}
	if res.Latency > 5*time.Second {
		t.Fatalf("Health: implausibly slow %s", res.Latency)
	}
}

// TestMigrateUpDownCycle drives the full migration lifecycle:
// down (idempotent), version (zero), up, version (one), inspect
// each created table via the connection, down again.
func TestMigrateUpDownCycle(t *testing.T) {
	// AcquireBlankDSN gives us a brand-new empty DB; t.Cleanup drops
	// it. No shared-state hammer needed.
	dsn := testDSN(t)

	// 1. After reset we should be at version 0.
	v, dirty, err := MigrationVersion(dsn)
	if err != nil {
		t.Fatalf("version after reset: %v", err)
	}
	if v != 0 || dirty {
		t.Fatalf("post-reset version got (%d, dirty=%v) want (0, false)", v, dirty)
	}

	// 2. Up applies every migration (0001..0006).
	if err := Migrate(dsn, MigrateUp); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	v, dirty, err = MigrationVersion(dsn)
	if err != nil {
		t.Fatalf("version after up: %v", err)
	}
	if v != 6 || dirty {
		t.Fatalf("post-up version got (%d, dirty=%v) want (6, false)", v, dirty)
	}

	// 3. Up is idempotent: second up returns NoChange and stays at v=6.
	if err := Migrate(dsn, MigrateUp); err != nil && !errors.Is(err, errors.New("no change")) {
		// The Migrate wrapper swallows ErrNoChange already; an
		// error here would be a regression.
		t.Fatalf("second up: %v", err)
	}

	// 4. Verify each table exists.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := NewPool(ctx, Config{DSN: dsn, AppName: "pulsys-test"})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	for _, table := range []string{"tenants", "users", "tokens", "audit_log", "settings", "oidc_providers", "sessions"} {
		var exists bool
		err := p.Pgx().QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)",
			table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s missing after migrate up", table)
		}
	}

	// 5. Down removes 0001 and brings version back to 0.
	if err := Migrate(dsn, MigrateDown); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	v, dirty, err = MigrationVersion(dsn)
	if err != nil {
		t.Fatalf("version after down: %v", err)
	}
	if v != 0 || dirty {
		t.Fatalf("post-down version got (%d, dirty=%v) want (0, false)", v, dirty)
	}
}

// TestSchemaConstraints exercises the CHECK and UNIQUE constraints
// declared in 0001_initial_schema.up.sql so a future migration
// that loosens them surfaces here.
func TestSchemaConstraints(t *testing.T) {
	dsn := testDSN(t)
	if err := Migrate(dsn, MigrateUp); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := NewPool(ctx, Config{DSN: dsn, AppName: "pulsys-test"})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	// Seed a tenant.
	var tenantID string
	err = p.Pgx().QueryRow(ctx,
		"INSERT INTO tenants(name, display_name) VALUES($1, $2) RETURNING id",
		"acme", "Acme Inc",
	).Scan(&tenantID)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	// users.role must be in {owner,admin,member,reader}.
	_, err = p.Pgx().Exec(ctx,
		"INSERT INTO users(tenant_id, email, display_name, role) VALUES($1, $2, $3, $4)",
		tenantID, "u1@acme.test", "U1", "godmode",
	)
	if err == nil {
		t.Fatalf("expected role check constraint to reject 'godmode'")
	}

	// Per-tenant email uniqueness on lower(email).
	if _, err := p.Pgx().Exec(ctx,
		"INSERT INTO users(tenant_id, email, display_name, role) VALUES($1, $2, $3, $4)",
		tenantID, "u1@acme.test", "U1", "member",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := p.Pgx().Exec(ctx,
		"INSERT INTO users(tenant_id, email, display_name, role) VALUES($1, $2, $3, $4)",
		tenantID, "U1@ACME.TEST", "U1 dup", "member",
	); err == nil {
		t.Fatalf("expected per-tenant lower(email) uniqueness to reject case-variant duplicate")
	}

	// tokens.hash must be 32 bytes (sha256).
	if _, err := p.Pgx().Exec(ctx,
		"INSERT INTO tokens(tenant_id, name, prefix, hash) VALUES($1, $2, $3, $4)",
		tenantID, "t1", "pk_abcd", []byte("too-short"),
	); err == nil {
		t.Fatalf("expected tokens.hash length check to reject short hash")
	}

	// audit_log.outcome must be in {success,failure,denied}.
	if _, err := p.Pgx().Exec(ctx,
		"INSERT INTO audit_log(tenant_id, actor_type, action, outcome) VALUES($1, $2, $3, $4)",
		tenantID, "system", "test.run", "maybe",
	); err == nil {
		t.Fatalf("expected audit_log.outcome check to reject 'maybe'")
	}

	// settings (tenant, scope, key) uniqueness.
	if _, err := p.Pgx().Exec(ctx,
		"INSERT INTO settings(tenant_id, scope, key, value) VALUES($1, $2, $3, $4::jsonb)",
		tenantID, "storage", "cold_tier_endpoint", `{"url":"s3://a"}`,
	); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	if _, err := p.Pgx().Exec(ctx,
		"INSERT INTO settings(tenant_id, scope, key, value) VALUES($1, $2, $3, $4::jsonb)",
		tenantID, "storage", "cold_tier_endpoint", `{"url":"s3://b"}`,
	); err == nil {
		t.Fatalf("expected settings (tenant,scope,key) uniqueness to reject duplicate")
	}
}
