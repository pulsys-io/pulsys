// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package db is the Pulsys Postgres persistence layer.
//
// The package owns:
//
//   - Connection pooling via jackc/pgx/v5 pgxpool.
//   - Embedded SQL migrations executed by golang-migrate with the
//     pgx/v5 driver so the binary ships exactly one Postgres
//     driver instead of two.
//   - A small health-check API the proxy / admin API can wire
//     into /healthz without taking a dependency on pgx types.
//
// The package is import-only for now: cmd/pulsys does not yet
// require Postgres at boot time.  Wiring lands in P3 alongside
// the OIDC / RBAC code so the schema and the enforcement ship
// together, and a single phase covers the "auth requires db"
// jump.  Until then this package is exercised by cmd/pulsys-db
// and the gated integration tests.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Defaults are conservative and sized for a single-node deployment.
// Larger / managed Postgres deployments override these via Config.
const (
	DefaultMaxConns          = 20
	DefaultMinConns          = 2
	DefaultMaxConnLifetime   = 30 * time.Minute
	DefaultMaxConnIdleTime   = 5 * time.Minute
	DefaultHealthCheckPeriod = 30 * time.Second
)

// Config controls the Postgres pool.
//
// DSN is the only required field.  Leaving a numeric field at 0
// causes NewPool to substitute the documented Default* value, so
// the caller can populate just what they need to override.
//
// AppName is set as PGAPPNAME on each connection so connections
// from different Pulsys components (pulsys, pulsys-db,
// admin-api in P4) are distinguishable in pg_stat_activity.
type Config struct {
	DSN               string
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
	AppName           string
}

// applyDefaults fills in zero-value Config fields with their
// documented defaults.  The caller's Config is not modified;
// the returned value is a copy.
func (c Config) applyDefaults() Config {
	if c.MaxConns == 0 {
		c.MaxConns = DefaultMaxConns
	}
	if c.MinConns == 0 {
		c.MinConns = DefaultMinConns
	}
	if c.MaxConnLifetime == 0 {
		c.MaxConnLifetime = DefaultMaxConnLifetime
	}
	if c.MaxConnIdleTime == 0 {
		c.MaxConnIdleTime = DefaultMaxConnIdleTime
	}
	if c.HealthCheckPeriod == 0 {
		c.HealthCheckPeriod = DefaultHealthCheckPeriod
	}
	if c.AppName == "" {
		c.AppName = "pulsys"
	}
	return c
}

// Pool wraps *pgxpool.Pool.
//
// We expose a wrapper instead of returning *pgxpool.Pool directly
// so callers depend on this package rather than on pgxpool
// directly, which lets us swap pool implementations later (e.g.
// for a tracing wrapper in P5) without touching consumer code.
type Pool struct {
	pool *pgxpool.Pool
	cfg  Config
}

// NewPool dials the database, opens a pgxpool with the supplied
// Config, and returns a Pool ready for queries.  The context
// bounds the initial connection attempt; callers should use a
// short deadline (5-30s) and treat any error as fatal at boot.
func NewPool(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.DSN == "" {
		return nil, errors.New("db: empty DSN")
	}
	cfg = cfg.applyDefaults()

	pcfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db: parse DSN: %w", err)
	}
	pcfg.MaxConns = cfg.MaxConns
	pcfg.MinConns = cfg.MinConns
	pcfg.MaxConnLifetime = cfg.MaxConnLifetime
	pcfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	pcfg.HealthCheckPeriod = cfg.HealthCheckPeriod
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	// PGAPPNAME goes on the connection so it shows up in
	// pg_stat_activity.application_name.
	pcfg.ConnConfig.RuntimeParams["application_name"] = cfg.AppName

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: initial ping: %w", err)
	}
	return &Pool{pool: pool, cfg: cfg}, nil
}

// Pgx returns the underlying *pgxpool.Pool.  Use this only for
// code that already imports pgx (sqlc-generated code in P3+);
// new consumers should add a method on Pool instead.
func (p *Pool) Pgx() *pgxpool.Pool { return p.pool }

// Config returns the resolved configuration (with defaults applied).
func (p *Pool) Config() Config { return p.cfg }

// Close shuts the pool down, canceling pending acquisitions and
// closing every idle connection.  Safe to call multiple times.
func (p *Pool) Close() {
	if p == nil || p.pool == nil {
		return
	}
	p.pool.Close()
}

// HealthResult is the outcome of a single Health check.
//
// Healthy is the high-level true/false signal the /healthz
// endpoint should report.  Latency captures the round trip so
// admin diagnostics can flag a degraded but reachable database.
type HealthResult struct {
	Healthy bool
	Latency time.Duration
	Err     error
}

// Health probes the database with a "SELECT 1" round trip,
// bounded by ctx.  The returned HealthResult always has a
// populated Latency, even on error, so callers can distinguish
// "slow but answered" from "unreachable".
//
// Health does NOT use the pool's Ping because Ping skips the
// query path entirely on some drivers; we want the test to
// exercise as much of the protocol as cheaply possible.
func (p *Pool) Health(ctx context.Context) HealthResult {
	start := time.Now()
	if p == nil || p.pool == nil {
		return HealthResult{Healthy: false, Latency: 0, Err: errors.New("db: nil pool")}
	}
	var one int
	err := p.pool.QueryRow(ctx, "SELECT 1").Scan(&one)
	latency := time.Since(start)
	if err != nil {
		return HealthResult{Healthy: false, Latency: latency, Err: err}
	}
	if one != 1 {
		return HealthResult{
			Healthy: false,
			Latency: latency,
			Err:     fmt.Errorf("db: SELECT 1 returned %d", one),
		}
	}
	return HealthResult{Healthy: true, Latency: latency}
}
