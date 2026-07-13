// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package testpg gives Postgres-backed Go tests cheap per-test
// database isolation, the way Django and Rails do it but using
// Postgres's native template-database mechanism rather than wrapping
// every test in a rolled-back transaction.
//
// Why not transaction-per-test?
//
//	The Pulsys registry layer runs real serialisable BEGIN/COMMIT
//	cycles inside CommitTx. A transaction-per-test harness (like
//	go-txdb) would have to emulate every BEGIN as a SAVEPOINT, and
//	pgx's BeginTx options (IsoLevel, AccessMode) do not survive that
//	translation cleanly. Template cloning gives us a fresh DB per
//	test with real transaction semantics and no driver gymnastics.
//
// How it works:
//
//  1. First call in the process: connect to an admin DSN, run
//     `CREATE DATABASE pulsys_tmpl_<sha>` keyed by the sha256 of all
//     embedded migration files. The sha-naming gives automatic cache
//     invalidation when a migration changes. Run migrations into the
//     template. Mark it `IS_TEMPLATE` so Postgres allows cheap
//     cloning.
//
//  2. Per Acquire(t) call: `CREATE DATABASE pulsys_<random> TEMPLATE
//     <tmpl>` — Postgres clones the template at the FS layer in
//     typically <50ms. Return a *pgxpool.Pool scoped to that DB.
//
//  3. t.Cleanup closes the pool and DROPs the cloned database.
//
// Where the admin DSN comes from:
//
//   - PULSYS_TEST_PG_DSN (preferred): explicit DSN, must be able to
//     CREATEDB. CI sets this; locally any postgres running as
//     superuser works.
//
//   - PULSYS_TEST_TC_IMAGE: e.g. "postgres:16-alpine". When set, the
//     helper starts a testcontainers-go Postgres container, captures
//     its DSN, and tears it down at the end of the test process.
//
//   - Neither set: tests Skip(), so `go test ./...` stays green on
//     machines without Postgres or Docker.
package testpg
