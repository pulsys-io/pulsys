// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package importer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/admin/store"
	"github.com/pulsys-io/pulsys/internal/auth"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/testpg"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestCacheImportWorker_PersistsProgressInPostgres(t *testing.T) {
	pool := testpg.Acquire(t)
	adminSt := store.NewAdminStore(pool)
	authSt := authstore.NewPG(pool)
	ctx := context.Background()

	tid, err := authSt.EnsureTenant(ctx, "import-worker-pg", "Import Worker PG")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := authSt.CreateUserOIDC(ctx, auth.User{
		TenantID:    tid,
		Email:       "import-worker-pg@test.local",
		DisplayName: "Import Worker",
		Role:        auth.RoleAdmin,
		OIDCSub:     "import-worker-pg-sub",
	})
	if err != nil {
		t.Fatal(err)
	}

	dl := &fakeDownloader{events: []ImportProgress{
		{Phase: "downloading", BytesDone: 64, BytesTotal: 128},
		{Phase: "downloading", BytesDone: 128, BytesTotal: 128},
	}}
	workers := river.NewWorkers()
	worker := &CacheImportWorker{
		Downloader:    dl,
		ProgressEvery: time.Millisecond,
	}
	river.AddWorker(workers, worker)
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Workers: workers})
	if err != nil {
		t.Fatal(err)
	}
	worker.Client = client
	adminSt.SetRiverClient(client)

	job, err := adminSt.CreateImportJob(ctx, tid, uid, store.ImportJobTypeHFCacheImport, json.RawMessage(`{
		"repo_id":"Qwen/Qwen2.5-0.5B",
		"revision":"main",
		"repo_type":"models"
	}`))
	if err != nil {
		t.Fatal(err)
	}

	workerClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
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
		got, err := adminSt.GetImportJob(ctx, tid, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == store.ImportJobSucceeded {
			if got.Attempt < 1 {
				t.Fatalf("attempt=%d want >=1", got.Attempt)
			}
			var progress ImportProgress
			if err := json.Unmarshal(got.Progress, &progress); err != nil {
				t.Fatal(err)
			}
			if progress.Phase != "complete" || progress.BytesDone != 128 || progress.BytesTotal != 128 {
				t.Fatalf("progress=%+v", progress)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; status=%q progress=%s", got.Status, got.Progress)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type fakeDownloader struct {
	err      error
	seenSpec ImportSpec
	events   []ImportProgress
}

func (d *fakeDownloader) Download(_ context.Context, spec ImportSpec, progress func(ImportProgress)) error {
	d.seenSpec = spec
	for _, ev := range d.events {
		progress(ev)
	}
	return d.err
}

var _ Downloader = (*fakeDownloader)(nil)

// TestCacheImportWorker_PersistsProgressMidFlight verifies the eager
// JobUpdate path: progress must land in Postgres while Work() is still
// running, not only at completion.  Regression test for the bug where
// river.RecordOutput-only persistence left the admin UI stuck at
// progress={} until the job finished, making running jobs appear to
// jump from "starting" straight to "completed".
func TestCacheImportWorker_PersistsProgressMidFlight(t *testing.T) {
	pool := testpg.Acquire(t)
	adminSt := store.NewAdminStore(pool)
	authSt := authstore.NewPG(pool)
	ctx := context.Background()

	tid, err := authSt.EnsureTenant(ctx, "import-worker-midflight", "Import Worker Mid-Flight")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := authSt.CreateUserOIDC(ctx, auth.User{
		TenantID:    tid,
		Email:       "import-worker-midflight@test.local",
		DisplayName: "Import Worker Mid",
		Role:        auth.RoleAdmin,
		OIDCSub:     "import-worker-midflight-sub",
	})
	if err != nil {
		t.Fatal(err)
	}

	dl := &gatedDownloader{
		event:   ImportProgress{Phase: "downloading", BytesDone: 64, BytesTotal: 128},
		final:   ImportProgress{Phase: "downloading", BytesDone: 128, BytesTotal: 128},
		release: make(chan struct{}),
		emitted: make(chan struct{}, 1),
	}
	workers := river.NewWorkers()
	worker := &CacheImportWorker{
		Downloader:    dl,
		ProgressEvery: time.Millisecond,
	}
	river.AddWorker(workers, worker)
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Workers: workers})
	if err != nil {
		t.Fatal(err)
	}
	worker.Client = client
	adminSt.SetRiverClient(client)

	job, err := adminSt.CreateImportJob(ctx, tid, uid, store.ImportJobTypeHFCacheImport, json.RawMessage(`{
		"repo_id":"Qwen/Qwen2.5-0.5B",
		"revision":"main",
		"repo_type":"models"
	}`))
	if err != nil {
		t.Fatal(err)
	}

	workerClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
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

	// Wait for the downloader to emit the first progress event, then
	// confirm Postgres reflects it BEFORE we release the gate.  The
	// worker is parked inside Download at this point, so any progress
	// visible here can only have arrived via Client.JobUpdate.
	select {
	case <-dl.emitted:
	case <-time.After(5 * time.Second):
		t.Fatal("downloader never emitted first event")
	}

	midDeadline := time.Now().Add(5 * time.Second)
	for {
		got, err := adminSt.GetImportJob(ctx, tid, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		var p ImportProgress
		if len(got.Progress) > 0 {
			_ = json.Unmarshal(got.Progress, &p)
		}
		if got.Status == store.ImportJobRunning && p.BytesDone == 64 && p.BytesTotal == 128 {
			break
		}
		if got.Status == store.ImportJobSucceeded {
			t.Fatalf("job finished before mid-flight assertion; the worker should have been parked. progress=%+v", p)
		}
		if time.Now().After(midDeadline) {
			t.Fatalf("mid-flight progress never appeared in Postgres; status=%q progress=%s", got.Status, got.Progress)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Release the downloader; it emits the final event and returns.
	close(dl.release)

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := adminSt.GetImportJob(ctx, tid, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == store.ImportJobSucceeded {
			var p ImportProgress
			if err := json.Unmarshal(got.Progress, &p); err != nil {
				t.Fatal(err)
			}
			if p.Phase != "complete" || p.BytesDone != 128 || p.BytesTotal != 128 {
				t.Fatalf("terminal progress=%+v", p)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for completion; status=%q progress=%s", got.Status, got.Progress)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// gatedDownloader emits one progress event, signals the test via
// `emitted`, and then blocks on `release` so the test can observe the
// Postgres row mid-flight before letting Work() complete.
type gatedDownloader struct {
	event   ImportProgress
	final   ImportProgress
	release chan struct{}
	emitted chan struct{}
}

func (d *gatedDownloader) Download(ctx context.Context, _ ImportSpec, progress func(ImportProgress)) error {
	// Sleep past the worker's progress-every throttle (set to 1ms in
	// this test) so the first event is not coalesced with the
	// preceding "resolving" persist.  In production the throttle is
	// 2s and real downloads take seconds, so this isn't an issue
	// outside the test.
	time.Sleep(20 * time.Millisecond)
	progress(d.event)
	select {
	case d.emitted <- struct{}{}:
	default:
	}
	select {
	case <-d.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	progress(d.final)
	return nil
}

var _ Downloader = (*gatedDownloader)(nil)

func TestCacheImportWorker_CancelRunning(t *testing.T) {
	pool := testpg.Acquire(t)
	adminSt := store.NewAdminStore(pool)
	authSt := authstore.NewPG(pool)
	ctx := context.Background()

	tid, err := authSt.EnsureTenant(ctx, "import-worker-cancel", "Import Worker Cancel")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := authSt.CreateUserOIDC(ctx, auth.User{
		TenantID:    tid,
		Email:       "import-worker-cancel@test.local",
		DisplayName: "Import Worker Cancel",
		Role:        auth.RoleAdmin,
		OIDCSub:     "import-worker-cancel-sub",
	})
	if err != nil {
		t.Fatal(err)
	}

	dl := &gatedDownloader{
		event:   ImportProgress{Phase: "downloading", BytesDone: 64, BytesTotal: 128},
		release: make(chan struct{}),
		emitted: make(chan struct{}, 1),
	}
	workers := river.NewWorkers()
	worker := &CacheImportWorker{
		Downloader:    dl,
		ProgressEvery: time.Millisecond,
	}
	river.AddWorker(workers, worker)
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Workers: workers})
	if err != nil {
		t.Fatal(err)
	}
	worker.Client = client
	adminSt.SetRiverClient(client)

	job, err := adminSt.CreateImportJob(ctx, tid, uid, store.ImportJobTypeHFCacheImport, json.RawMessage(`{
		"repo_id":"Qwen/Qwen2.5-0.5B",
		"revision":"main",
		"repo_type":"models"
	}`))
	if err != nil {
		t.Fatal(err)
	}

	workerClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
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

	select {
	case <-dl.emitted:
	case <-time.After(5 * time.Second):
		t.Fatal("downloader never emitted first event")
	}

	runningDeadline := time.Now().Add(5 * time.Second)
	for {
		got, err := adminSt.GetImportJob(ctx, tid, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == store.ImportJobRunning {
			break
		}
		if time.Now().After(runningDeadline) {
			t.Fatalf("job never entered running; status=%q", got.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := adminSt.CancelImportJob(ctx, tid, job.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := adminSt.GetImportJob(ctx, tid, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == store.ImportJobCanceled {
			// Canceled rows never expose a user-facing error block; the
			// status badge alone communicates the state (see import_jobs.go
			// and TestForceCancelImportJob_* which assert the same).
			if got.Error != nil {
				t.Fatalf("error=%q, want nil (canceled is not a failure)", *got.Error)
			}
			return
		}
		if got.Status == store.ImportJobSucceeded {
			t.Fatal("canceled job completed as succeeded")
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for cancel; status=%q error=%v", got.Status, got.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
