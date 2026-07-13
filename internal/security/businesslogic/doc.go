// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package businesslogic is the OWASP WSTG-BUSL regression
// surface for pulsys.
//
// # Why this package exists
//
// internal/security/authcontract pins WHO can call WHAT.
// internal/security/sectest pins what the network protocol
// surface accepts.  This package fills the third slot: the
// state-machine + invariant rules that sit ABOVE both --
// "two concurrent writers must not silently overwrite each
// other", "a revoked credential must stop working in the same
// HTTP request that revoked it", "a client cannot upload a
// 1 TiB payload through a 100 GiB endpoint".
//
// These are the WSTG-BUSL-01..09 categories: business-logic
// flaws that pass every parser + auth test but break the
// product's value contract with the operator.  Each test
// here was added in response to a specific Phase 5 production
// fix; cross-reference the Plan: trailer in the corresponding
// commit for the change set being pinned.
//
// # Test taxonomy
//
//   - lfs_upload_size_test.go           BUSL-09  oversize PUT
//   - pat_revocation_dataplane_test.go  ATHN-04  revoke cache
//   - token_scope_allowlist_test.go     BUSL-01  unknown scope
//   - token_ttl_validation_test.go      BUSL-01  silly TTL
//   - settings_cas_test.go              BUSL-03  CAS race
//   - token_revoke_idempotent_test.go   BUSL-07  retry audit
//   - commit_lfs_size_mismatch_test.go  BUSL-09  size lie
//   - commit_filepath_validation_test.go BUSL-01 path tricks
//
// # Why these are in a separate package
//
// The authcontract matrix is a one-shot Postgres-backed
// fixture; sectest is raw-TCP black-box.  This package is
// neither -- the tests here drive admin handlers and registry
// commits in-process with real Postgres state and assert
// post-conditions on that state.  Lifting them out keeps
// the authcontract matrix focused on auth deniability without
// growing a business-logic chapter, and keeps the sectest
// fixture small enough to read in one screen.
//
// # CI behavior
//
// Every test in this package calls testpg.Acquire(t) which
// skips with a clear message if PULSYS_TEST_DB_DSN is unset.
// The docker security-tests image always sets it; local runs
// without a Postgres remain green by skipping.
package businesslogic
