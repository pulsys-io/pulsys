// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pulsys-io/pulsys/internal/admin/store"
	"github.com/pulsys-io/pulsys/internal/auth"
	"github.com/pulsys-io/pulsys/internal/db"
	"github.com/pulsys-io/pulsys/internal/importer"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func setupImportRiverClient(t *testing.T, pool *pgxpool.Pool) *river.Client[pgx.Tx] {
	t.Helper()
	ctx := context.Background()
	if err := db.MigrateRiverPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Insert-only River client: see internal/admin/api/handler_test.go
	// for the rationale.  Workers are still registered so River can
	// validate kinds at insert time, but the client is never Started.
	workers := river.NewWorkers()
	river.AddWorker(workers, &importer.CacheImportWorker{
		Downloader:    &importerWorkerFake{},
		ProgressEvery: time.Millisecond,
	})
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Workers: workers})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type importerWorkerFake struct{}

func (importerWorkerFake) Download(_ context.Context, spec importer.ImportSpec, progress func(importer.ImportProgress)) error {
	progress(importer.ImportProgress{Phase: "downloading", BytesDone: 32, BytesTotal: 32})
	return nil
}

func TestImportJobs_CreateListGet(t *testing.T) {
	adminSt, authSt, cleanup := testAdminStore(t)
	defer cleanup()
	ctx := context.Background()
	adminSt.SetRiverClient(setupImportRiverClient(t, adminSt.Pool))

	tid, uid := createImportJobTenant(t, ctx, authSt, "import-jobs-create")
	payload := json.RawMessage(`{"repo_id":"Qwen/Qwen2.5-0.5B","revision":"main"}`)

	created, err := adminSt.CreateImportJob(ctx, tid, uid, store.ImportJobTypeHFCacheImport, payload)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("empty job id")
	}
	if created.Status != store.ImportJobQueued {
		t.Fatalf("status=%q want queued", created.Status)
	}
	if created.Attempt != 0 {
		t.Fatalf("attempt=%d want 0", created.Attempt)
	}

	got, err := adminSt.GetImportJob(ctx, tid, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID {
		t.Fatalf("get id=%q want %q", got.ID, created.ID)
	}

	list, err := adminSt.ListImportJobs(ctx, tid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list=%+v", list)
	}
}

// TestImportJobs_ForceCancel_OrphanedRunning reproduces the
// production bug where a job sits at `running` in the DB but has
// no live worker -- e.g. the proxy was restarted while the job
// was downloading, or the worker crashed mid-attempt.
//
// Repro shape:
//
//  1. Insert a job through CreateImportJob (River row exists,
//     state=available).
//  2. Manually UPDATE river_job.state='running' to simulate the
//     orphan. No worker is started.
//  3. Verify CancelImportJob succeeds (mirrors the 204 the user
//     reported in the field) but the state stays running -- River
//     only signals; there's no worker to honor the cancel.
//  4. Verify ForceCancelImportJob flips the row to canceled
//     without needing the worker to come back to life.
//  5. Verify Delete now succeeds (the UI's "Force remove" affordance
//     unblocks the operator).
//
// Regression guard: before the ForceCancelImportJob escape hatch
// existed, a row in this shape was unfixable from the admin UI.
func TestImportJobs_ForceCancel_OrphanedRunning(t *testing.T) {
	adminSt, authSt, cleanup := testAdminStore(t)
	defer cleanup()
	ctx := context.Background()
	adminSt.SetRiverClient(setupImportRiverClient(t, adminSt.Pool))

	tid, uid := createImportJobTenant(t, ctx, authSt, "import-orphan-running")
	created, err := adminSt.CreateImportJob(ctx, tid, uid,
		store.ImportJobTypeHFCacheImport,
		json.RawMessage(`{"repo_id":"Qwen/Qwen2.5-0.5B"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the orphan: flip state to running directly, no worker
	// to claim or finalize the row.
	if _, err := adminSt.Pool.Exec(ctx, `
		UPDATE river_job
		SET state = 'running'::river_job_state, attempted_at = now(), attempt = 1
		WHERE id = $1
	`, created.ID); err != nil {
		t.Fatal(err)
	}

	got, err := adminSt.GetImportJob(ctx, tid, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.ImportJobRunning {
		t.Fatalf("setup: status=%q want running", got.Status)
	}

	// Regular Cancel returns nil (204 at the HTTP layer) but the
	// row stays running because there's no worker subscribed.
	if err := adminSt.CancelImportJob(ctx, tid, created.ID); err != nil {
		t.Fatalf("Cancel returned err=%v, want nil (matches the 204 the UI sees)", err)
	}
	stillRunning, err := adminSt.GetImportJob(ctx, tid, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillRunning.Status != store.ImportJobRunning {
		t.Fatalf("after Cancel: status=%q want still running (orphan repro)", stillRunning.Status)
	}
	if stillRunning.CancelRequestedAt == nil {
		t.Fatal("after Cancel: CancelRequestedAt should be populated so UI can render Canceling…")
	}

	// Delete must refuse: running rows are protected by River's
	// "no delete running" safety. This is the dead-end the user hit.
	if err := adminSt.DeleteImportJob(ctx, tid, created.ID); err == nil {
		t.Fatal("Delete on running row should refuse, got nil")
	}

	// Force-cancel: the escape hatch. Flips state without needing
	// the worker to do anything.
	if err := adminSt.ForceCancelImportJob(ctx, tid, created.ID); err != nil {
		t.Fatal(err)
	}
	final, err := adminSt.GetImportJob(ctx, tid, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != store.ImportJobCanceled {
		t.Fatalf("after ForceCancel: status=%q want canceled", final.Status)
	}
	if final.CompletedAt == nil {
		t.Fatal("after ForceCancel: CompletedAt should be set")
	}
	// Canceled rows never expose a user-facing error block; the
	// status badge alone communicates the state.
	if final.Error != nil {
		t.Fatalf("after ForceCancel: error=%q should be nil (canceled is not a failure)", *final.Error)
	}

	// Delete now succeeds: the operator can finish cleanup.
	if err := adminSt.DeleteImportJob(ctx, tid, created.ID); err != nil {
		t.Fatalf("Delete after ForceCancel err=%v", err)
	}
	if _, err := adminSt.GetImportJob(ctx, tid, created.ID); err == nil {
		t.Fatal("Get after Delete should return not-found")
	}
}

// TestImportJobs_ForceCancel_Idempotent verifies a double-click on
// Force remove (or a CI retry) does not error and does not rewrite
// the original finalized_at. Mirrors the contract for CancelImportJob.
func TestImportJobs_ForceCancel_Idempotent(t *testing.T) {
	adminSt, authSt, cleanup := testAdminStore(t)
	defer cleanup()
	ctx := context.Background()
	adminSt.SetRiverClient(setupImportRiverClient(t, adminSt.Pool))

	tid, uid := createImportJobTenant(t, ctx, authSt, "import-force-idempotent")
	created, err := adminSt.CreateImportJob(ctx, tid, uid,
		store.ImportJobTypeHFCacheImport,
		json.RawMessage(`{"repo_id":"Qwen/Qwen2.5-0.5B"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := adminSt.ForceCancelImportJob(ctx, tid, created.ID); err != nil {
		t.Fatal(err)
	}
	first, err := adminSt.GetImportJob(ctx, tid, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.CompletedAt == nil {
		t.Fatal("first ForceCancel: expected CompletedAt to be set")
	}
	firstFinalised := *first.CompletedAt

	if err := adminSt.ForceCancelImportJob(ctx, tid, created.ID); err != nil {
		t.Fatalf("second ForceCancel err=%v, want idempotent nil", err)
	}
	second, err := adminSt.GetImportJob(ctx, tid, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.CompletedAt == nil || !second.CompletedAt.Equal(firstFinalised) {
		t.Fatalf("second ForceCancel: finalized_at moved (first=%v second=%v); idempotency broken",
			firstFinalised, second.CompletedAt)
	}
}

// TestImportJobs_ForceCancel_CrossTenant guards the tenant-scoping
// rationale documented on ForceCancelImportJob: River's job ids are
// global integers, so the store must reject force-cancel for ids it
// finds in another tenant. Same shape as the cross-tenant guard on
// regular Cancel and Delete.
func TestImportJobs_ForceCancel_CrossTenant(t *testing.T) {
	adminSt, authSt, cleanup := testAdminStore(t)
	defer cleanup()
	ctx := context.Background()
	adminSt.SetRiverClient(setupImportRiverClient(t, adminSt.Pool))

	tidA, uidA := createImportJobTenant(t, ctx, authSt, "import-force-tenant-a")
	tidB, _ := createImportJobTenant(t, ctx, authSt, "import-force-tenant-b")

	createdA, err := adminSt.CreateImportJob(ctx, tidA, uidA,
		store.ImportJobTypeHFCacheImport,
		json.RawMessage(`{"repo_id":"Qwen/Qwen2.5-0.5B"}`))
	if err != nil {
		t.Fatal(err)
	}

	err = adminSt.ForceCancelImportJob(ctx, tidB, createdA.ID)
	if err == nil {
		t.Fatal("cross-tenant ForceCancel returned nil; want not-found")
	}
	if !errors.Is(err, store.ErrImportJobNotFound) {
		t.Fatalf("err=%v, want ErrImportJobNotFound", err)
	}

	// Tenant A can still cancel their own row (regression: store
	// must not have side-effected the row during the rejected
	// cross-tenant call).
	if err := adminSt.ForceCancelImportJob(ctx, tidA, createdA.ID); err != nil {
		t.Fatal(err)
	}
}

func TestImportJobs_RiverWorkerCompletes(t *testing.T) {
	adminSt, authSt, cleanup := testAdminStore(t)
	defer cleanup()
	ctx := context.Background()
	client := setupImportRiverClient(t, adminSt.Pool)
	adminSt.SetRiverClient(client)

	tid, uid := createImportJobTenant(t, ctx, authSt, "import-jobs-worker")
	created, err := adminSt.CreateImportJob(ctx, tid, uid, store.ImportJobTypeHFCacheImport, json.RawMessage(`{"repo_id":"gpt2"}`))
	if err != nil {
		t.Fatal(err)
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, &importer.CacheImportWorker{
		Downloader:    &importerWorkerFake{},
		ProgressEvery: time.Millisecond,
	})
	workerClient, err := river.NewClient(riverpgxv5.New(adminSt.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers: workers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := workerClient.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = workerClient.Stop(stopCtx)
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := adminSt.GetImportJob(ctx, tid, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == store.ImportJobSucceeded {
			var progress importer.ImportProgress
			if err := json.Unmarshal(got.Progress, &progress); err != nil {
				t.Fatal(err)
			}
			if progress.Phase != "complete" || progress.BytesDone != 32 {
				t.Fatalf("progress=%+v", progress)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for import; last status=%q progress=%s", got.Status, got.Progress)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func createImportJobTenant(t *testing.T, ctx context.Context, authSt interface {
	EnsureTenant(context.Context, string, string) (string, error)
	CreateUserOIDC(context.Context, auth.User) (string, error)
}, slug string) (tenantID, userID string) {
	t.Helper()
	tid, err := authSt.EnsureTenant(ctx, slug, slug)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := authSt.CreateUserOIDC(ctx, auth.User{
		TenantID:    tid,
		Email:       slug + "@test.local",
		DisplayName: "Import Admin",
		Role:        auth.RoleAdmin,
		OIDCSub:     slug + "-sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	return tid, uid
}
