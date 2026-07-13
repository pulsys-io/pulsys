// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package importer

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// RiverConfig configures the shared River client used by the admin API and
// optional import worker.
type RiverConfig struct {
	Pool            *pgxpool.Pool
	LoopbackBaseURL string
	HFToken         string
	MaxWorkers      int
	WorkerEnabled   bool
	// JobTimeout caps a single import worker run. Zero uses 24h (River's
	// default is 1m, which aborts large model warms with deadline exceeded).
	JobTimeout time.Duration
}

// RiverBundle holds a started River client and its worker registration.
type RiverBundle struct {
	Client *river.Client[pgx.Tx]
}

// NewRiverBundle migrates River schema (idempotent), registers the cache-import
// worker, and optionally starts job processing.
func NewRiverBundle(ctx context.Context, cfg RiverConfig) (*RiverBundle, error) {
	if cfg.Pool == nil {
		return nil, fmt.Errorf("importer: nil pool")
	}
	if strings.TrimSpace(cfg.LoopbackBaseURL) == "" {
		return nil, fmt.Errorf("importer: loopback base URL required")
	}

	driver := riverpgxv5.New(cfg.Pool)
	workers := river.NewWorkers()
	maxFetchWorkers := cfg.MaxWorkers
	if maxFetchWorkers <= 0 {
		maxFetchWorkers = runtime.GOMAXPROCS(0) * 2
		if maxFetchWorkers < 1 {
			maxFetchWorkers = 1
		}
	}
	// Hold a pointer to the worker so we can inject the client after
	// river.NewClient builds it.  River's registration pattern
	// requires workers to exist before the client is constructed,
	// but the worker also needs a client reference so it can call
	// JobUpdate from inside Work for eager progress persistence.
	worker := &CacheImportWorker{
		JobTimeout: cfg.JobTimeout,
		Downloader: HFCacheDownloader{
			Endpoint: cfg.LoopbackBaseURL,
			Token:    strings.TrimSpace(cfg.HFToken),
			Workers:  maxFetchWorkers,
		},
	}
	river.AddWorker(workers, worker)

	// River v0.38+ rejects QueueConfig{MaxWorkers: 0}, so we either
	// configure a real worker pool (when this process runs the
	// worker) or build an insert-only client (when the worker is
	// disabled).  The former calls Start; the latter omits Queues
	// and never calls Start.  See:
	// https://riverqueue.com/docs/insert-only-clients
	clientCfg := &river.Config{Workers: workers}
	if cfg.WorkerEnabled {
		clientCfg.Queues = map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		}
	}
	client, err := river.NewClient(driver, clientCfg)
	if err != nil {
		return nil, err
	}
	worker.Client = client
	if cfg.WorkerEnabled {
		if err := client.Start(ctx); err != nil {
			return nil, err
		}
	}
	return &RiverBundle{Client: client}, nil
}

// Stop shuts down the River client.
func (b *RiverBundle) Stop(ctx context.Context) error {
	if b == nil || b.Client == nil {
		return nil
	}
	return b.Client.Stop(ctx)
}
