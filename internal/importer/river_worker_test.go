// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package importer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

func TestCacheImportWorkerTimeout(t *testing.T) {
	w := &CacheImportWorker{}
	if got := w.Timeout(&river.Job[CacheImportArgs]{}); got != defaultImportJobTimeout {
		t.Fatalf("default timeout = %v, want %v", got, defaultImportJobTimeout)
	}
	w.JobTimeout = 2 * time.Hour
	if got := w.Timeout(&river.Job[CacheImportArgs]{}); got != 2*time.Hour {
		t.Fatalf("custom timeout = %v, want 2h", got)
	}
}

type cancelOnCtxDownloader struct{}

func (cancelOnCtxDownloader) Download(ctx context.Context, _ ImportSpec, _ func(ImportProgress)) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestCacheImportWorker_ReturnsJobCancelOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := &CacheImportWorker{Downloader: cancelOnCtxDownloader{}}
	err := w.Work(ctx, &river.Job[CacheImportArgs]{
		Args: CacheImportArgs{RepoID: "org/model", Revision: "main", RepoType: "models"},
	})
	var cancelErr *river.JobCancelError
	if !errors.As(err, &cancelErr) {
		t.Fatalf("Work() err = %v, want JobCancelError", err)
	}
}
