// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// CacheImportWorker warms the local proxy cache by downloading through a
// loopback proxy.Handler endpoint (no Pulsys PAT on the data plane).
type CacheImportWorker struct {
	river.WorkerDefaults[CacheImportArgs]
	Downloader    Downloader
	JobTimeout    time.Duration
	ProgressEvery time.Duration
	// Client is injected post-construction by NewRiverBundle so we can
	// call JobUpdate from inside Work. river.RecordOutput buffers the
	// value in the work context and only flushes to Postgres when Work
	// returns; without an eager update path the admin UI would see the
	// job sit at progress={} until completion. The field is optional --
	// tests that don't wire a client fall back to RecordOutput, which
	// is still correct for terminal state assertions.
	Client *river.Client[pgx.Tx]
}

const defaultImportJobTimeout = 24 * time.Hour

// Timeout overrides River's 1-minute default; cache imports can run for hours.
func (w *CacheImportWorker) Timeout(*river.Job[CacheImportArgs]) time.Duration {
	if w.JobTimeout > 0 {
		return w.JobTimeout
	}
	return defaultImportJobTimeout
}

func (w *CacheImportWorker) Work(ctx context.Context, job *river.Job[CacheImportArgs]) error {
	spec, err := parseSpecFromArgs(job.Args)
	if err != nil {
		return err
	}

	start := time.Now()
	lastPersist := time.Time{}
	var latest ImportProgress
	persist := func(p ImportProgress, force bool) {
		p.UpdatedAt = time.Now().UTC()
		if p.DownloadBPS == 0 && p.BytesDone > 0 {
			elapsed := time.Since(start).Seconds()
			if elapsed > 0 {
				p.DownloadBPS = float64(p.BytesDone) / elapsed
			}
		}
		latest = p
		if !force && time.Since(lastPersist) < w.progressEvery() {
			return
		}
		lastPersist = time.Now()
		if w.Client != nil {
			_, _ = w.Client.JobUpdate(ctx, job.ID, &river.JobUpdateParams{Output: p})
			return
		}
		_ = river.RecordOutput(ctx, p)
	}

	persist(ImportProgress{Phase: "resolving", Message: "resolving repository metadata"}, true)
	err = w.Downloader.Download(ctx, spec, func(p ImportProgress) {
		if ctx.Err() != nil {
			return
		}
		if p.Phase == "" {
			p.Phase = "downloading"
		}
		persist(p, false)
	})
	if err != nil {
		if isImportCanceled(ctx, err) {
			return river.JobCancel(importCancelErr(ctx, err))
		}
		if isQuotaHTTPError(err) {
			return HumanizeImportError(fmt.Errorf("cache storage quota exceeded; purge unused models on /models or raise -cache-max-bytes"))
		}
		return HumanizeImportError(err)
	}
	if isImportCanceled(ctx, nil) {
		return river.JobCancel(importCancelErr(ctx, nil))
	}
	if latest.Phase == "" {
		latest.Phase = "complete"
	}
	latest.Phase = "complete"
	latest.Message = "import complete"
	latest.UpdatedAt = time.Now().UTC()
	_ = river.RecordOutput(ctx, latest)
	return nil
}

func (w *CacheImportWorker) progressEvery() time.Duration {
	if w.ProgressEvery > 0 {
		return w.ProgressEvery
	}
	return 2 * time.Second
}

func parseSpecFromArgs(args CacheImportArgs) (ImportSpec, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return ImportSpec{}, err
	}
	return parseSpec(raw)
}

type httpStatusError interface {
	Status() int
}

// isImportCanceled reports whether the job context was canceled (admin
// cancel or shutdown). River only transitions a running job to canceled
// when Work returns a non-nil error; returning nil after cancel completes
// the job as succeeded.
func isImportCanceled(ctx context.Context, err error) bool {
	if err != nil && errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(context.Cause(ctx), river.ErrJobCancelledRemotely) {
		return true
	}
	return errors.Is(ctx.Err(), context.Canceled)
}

func importCancelErr(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); errors.Is(cause, river.ErrJobCancelledRemotely) {
		return cause
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

func isQuotaHTTPError(err error) bool {
	if err == nil {
		return false
	}
	var statusErr httpStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.Status() == http.StatusInsufficientStorage
}
