// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package registry is the Postgres-backed data layer for the
// Pulsys self-hosted model registry. It owns the rows in
//
//	repos
//	commits
//	branches
//	blobs
//	file_revisions
//	mirrors
//
// and the invariants between them. HTTP handlers in
// internal/proxy/registry.go and internal/proxy/upload.go depend on
// this package; they never run SQL directly.
//
// Atomicity:
//
//   - CommitTx is the single write entry point: it appends a commit,
//     inserts file_revisions, upserts blob rows (with refcount += 1),
//     and advances the target branch in one Postgres transaction.
//     A crash mid-commit leaves the registry consistent.
//
//   - Blob bytes are written via internal/blobstore BEFORE the
//     transaction. If the transaction fails the blob bytes are
//     orphans, GC-able later by `refcount = 0`. We never have an
//     `file_revisions` row pointing at non-existent bytes.
//
// Tenancy:
//
//   - All public methods take a tenant context. The package sets
//     `pulsys.tenant_id` per transaction so the RLS policies declared
//     in 0004_registry.up.sql apply. Tests run as superuser (bypasses
//     RLS) but verify the WHERE-clauses explicitly.
package registry
