// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package authcontract is the security-contract test surface for every
// HTTP endpoint Pulsys exposes.
//
// # Why this package exists
//
// The 2026-05-21 incident -- a revoked PAT continued to authorize
// downloads through pulsys -- traced to two interlocking defects
// (the data plane had no auth gate at all, and the admin middleware
// silently swallowed "invalid"-containing errors).  Neither defect
// would have shipped if there had been a single test that enumerated
// every endpoint and asserted the rejection / admission behavior for
// every credential class.
//
// This package is that test.  It is intentionally:
//
//   - Declarative.  Every endpoint's auth requirement is a row in a
//     table, not a custom test function.  Adding a new endpoint forces
//     the table to grow; missing entries trip the completeness check.
//
//   - End-to-end.  The admin matrix mounts the real admin.NewHandler
//     over a freshly migrated Postgres database.  The data plane
//     matrix mounts the real proxy stack via testserver.New with the
//     production PATGate installed.  No mocks of the auth layer.
//
//   - Exhaustive on credentials.  Each endpoint is probed with every
//     credential class -- anonymous, bogus PAT, revoked PAT, expired
//     PAT, scoped PATs (read / write / admin:*), and (for the admin
//     surface) sessions for every role plus a revoked session.
//
//   - Auth-only.  The matrix asserts "401 vs 403 vs admitted" only.
//     Endpoint body / content correctness lives in the owning
//     package's tests.  This separation keeps the matrix small and
//     stable: a new query parameter on a handler should not require
//     touching the auth contract.
//
// The Postgres-backed tests Skip() when testpg.Acquire has no admin
// DSN, so `go test ./...` remains portable.  CI configures
// PULSYS_TEST_PG_DSN (or PULSYS_TEST_TC_IMAGE) so every PR runs the
// full matrix.
package authcontract
