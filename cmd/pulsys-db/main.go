// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// pulsys-db is the operational CLI for the Pulsys Postgres
// schema.  It is the single tool the AMI bootstrap, the dev
// docker-compose flow, and a human operator on RDS share for
// running migrations and inspecting database state.
//
// Subcommands:
//
//	pulsys-db migrate up       apply every pending migration
//	pulsys-db migrate down     revert every applied migration
//	pulsys-db migrate version  print current version (+ dirty flag)
//	pulsys-db health           ping the database, print latency
//	pulsys-db user grant-owner break-glass owner promotion (audited)
//
// Conventions:
//
//	-dsn         postgres://user:pass@host:port/db?sslmode=...
//	             defaults to $PULSYS_DB_DSN
//	-timeout     overall command timeout (default 30s)
//
// Exit codes:
//
//	0   success
//	1   configuration error (bad DSN, bad subcommand)
//	2   database error (migration failed, ping failed, ...)
//
// The CLI deliberately keeps no per-subcommand flags beyond what
// is listed above; advanced operations (force, goto a specific
// version, drop without prompts) are intentionally NOT exposed
// so a misclick during incident response cannot brick the
// schema.  Anyone who needs those reaches for `migrate` directly.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/pulsys-io/pulsys/internal/db"
)

const usage = `pulsys-db: operational CLI for Pulsys Postgres state

usage:
  pulsys-db [-dsn DSN] [-timeout DURATION] <subcommand> [args]

flags:
  -dsn        Postgres DSN (default: $PULSYS_DB_DSN)
  -timeout    overall command timeout (default: 30s)

subcommands:
  migrate up        apply every pending migration
  migrate down      revert every applied migration
  migrate version   print current version and dirty flag
  health            ping the database, print latency
  tenant ensure     create or fetch a tenant by slug
  oidc configure    configure external OIDC IdP for a tenant
  user grant-owner  break-glass: promote a user to owner (audited)
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	fs := flag.NewFlagSet("pulsys-db", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	dsn := fs.String("dsn", os.Getenv("PULSYS_DB_DSN"), "Postgres DSN (default $PULSYS_DB_DSN)")
	timeout := fs.Duration("timeout", 30*time.Second, "overall command timeout")

	if err := fs.Parse(argv); err != nil {
		return 1
	}
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return 1
	}

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "pulsys-db: -dsn or $PULSYS_DB_DSN is required")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	switch args[0] {
	case "migrate":
		if len(args) < 2 {
			fs.Usage()
			return 1
		}
		switch args[1] {
		case "up":
			return doMigrate(*dsn, db.MigrateUp, "up")
		case "down":
			return doMigrate(*dsn, db.MigrateDown, "down")
		case "version":
			return doMigrateVersion(*dsn)
		default:
			fmt.Fprintf(os.Stderr, "pulsys-db: unknown migrate subcommand %q\n", args[1])
			return 1
		}
	case "health":
		return doHealth(ctx, *dsn)
	case "tenant":
		return runTenant(args, *dsn, ctx)
	case "oidc":
		return runOIDC(args, *dsn, ctx)
	case "user":
		return runUser(args, *dsn, ctx)
	default:
		fmt.Fprintf(os.Stderr, "pulsys-db: unknown subcommand %q\n", args[0])
		return 1
	}
}

func doMigrate(dsn string, dir db.MigrationDirection, label string) int {
	fmt.Printf("pulsys-db: migrate %s starting\n", label)
	if err := db.Migrate(dsn, dir); err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: migrate %s failed: %v\n", label, err)
		return 2
	}
	if dir == db.MigrateUp {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := db.MigrateRiver(ctx, dsn); err != nil {
			fmt.Fprintf(os.Stderr, "pulsys-db: river migrate up failed: %v\n", err)
			return 2
		}
	}
	v, dirty, err := db.MigrationVersion(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: migrate %s ok, version check failed: %v\n", label, err)
		return 2
	}
	fmt.Printf("pulsys-db: migrate %s ok (version=%d dirty=%v)\n", label, v, dirty)
	return 0
}

func doMigrateVersion(dsn string) int {
	v, dirty, err := db.MigrationVersion(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: version: %v\n", err)
		return 2
	}
	fmt.Printf("version=%d dirty=%v\n", v, dirty)
	if dirty {
		return 2
	}
	return 0
}

func doHealth(ctx context.Context, dsn string) int {
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AppName: "pulsys-db"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: health: %v\n", err)
		return 2
	}
	defer pool.Close()
	res := pool.Health(ctx)
	if !res.Healthy {
		if res.Err != nil && !errors.Is(res.Err, context.DeadlineExceeded) {
			fmt.Fprintf(os.Stderr, "pulsys-db: health: %v\n", res.Err)
		} else if res.Err != nil {
			fmt.Fprintf(os.Stderr, "pulsys-db: health: deadline exceeded after %s\n", res.Latency)
		}
		return 2
	}
	fmt.Printf("healthy latency=%s\n", res.Latency)
	return 0
}
