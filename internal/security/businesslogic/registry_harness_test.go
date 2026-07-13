// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package businesslogic

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/blobstore"
	"github.com/pulsys-io/pulsys/internal/registry"
	"github.com/pulsys-io/pulsys/internal/testpg"
)

// registryHarness wires a Postgres-backed registry.Store + a
// local blobstore + one seeded repo + one seeded blob.  This
// is the minimum surface every commit-side BUSL test needs:
// the path-validation + size-cross-check tests differ only in
// the CommitInput they hand to CommitTx.
//
// Each call constructs a fresh isolated database via testpg
// so tests can run in parallel.
type registryHarness struct {
	t        *testing.T
	ctx      context.Context
	store    *registry.Store
	bs       blobstore.Store
	tenantID string
	repoID   string
	blobOID  string
	blobSize int64
}

func newRegistryHarness(t *testing.T) *registryHarness {
	t.Helper()
	pool := testpg.Acquire(t)
	s := registry.New(pool)
	bs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blobstore: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	// Seed one tenant.  The registry tests use ('default')
	// directly via INSERT; we follow that pattern to stay
	// minimal-dependency.
	var tenantID string
	if err := pool.QueryRow(ctx, `
INSERT INTO tenants (name, display_name) VALUES ('busl-registry', 'BUSL registry')
RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	repo, err := s.CreateRepo(ctx, tenantID, "models", "acme", "busl", false, "")
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	// Seed one blob so commit tests can reference a real oid
	// without each test re-doing the dance.  The bytes are
	// content-addressed; the stored size is recorded for the
	// LFS pointer size-mismatch test.
	body := []byte("BUSL test blob content -- v1")
	stat, err := bs.Put(ctx, bytes.NewReader(body), blobstore.PutOptions{})
	if err != nil {
		t.Fatalf("blobstore put: %v", err)
	}
	if err := s.UpsertBlob(ctx, stat.OID, stat.Size, stat.StorageURL); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}

	return &registryHarness{
		t:        t,
		ctx:      ctx,
		store:    s,
		bs:       bs,
		tenantID: tenantID,
		repoID:   repo.ID,
		blobOID:  stat.OID,
		blobSize: stat.Size,
	}
}
