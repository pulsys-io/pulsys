// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/logx"
	"github.com/pulsys-io/pulsys/internal/proxy"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

// rangeUpstream is a fake upstream that:
//   - returns 206 slices honoring the incoming Range header
//   - sleeps for the configured `delay` per request to simulate
//     network RTT
//   - tracks the maximum number of concurrent in-flight Do() calls so
//     tests can assert true parallelism (race-free via atomic gauge)
//
// Without the AcquireRange fix the proxy holds Lock(keyHex) for the
// full stream duration, so concurrent disjoint range requests are
// serialized at the proxy and inFlight peaks at 1.
type rangeUpstream struct {
	body        []byte
	delay       time.Duration
	totalCL     int64
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
	fetches     atomic.Int64
}

func (r *rangeUpstream) Do(ctx context.Context, _, _, _, _ string, hdr http.Header, _ []byte) (*upstream.Response, error) {
	r.fetches.Add(1)
	cur := r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	for {
		old := r.maxInFlight.Load()
		if cur <= old || r.maxInFlight.CompareAndSwap(old, cur) {
			break
		}
	}

	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	rangeStr := hdr.Get("Range")
	start, end, ok := cache.ParseSingleRange(rangeStr, int64(len(r.body)))
	if !ok {
		return nil, errors.New("rangeUpstream: missing/bad Range header")
	}
	if end > int64(len(r.body)) {
		end = int64(len(r.body))
	}
	slice := r.body[start:end]
	h := http.Header{}
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(int64(len(slice)), 10))
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, r.totalCL))
	return &upstream.Response{
		Status:        http.StatusPartialContent,
		Header:        h,
		ContentLength: int64(len(slice)),
		Body:          io.NopCloser(bytes.NewReader(slice)),
	}, nil
}

// newRangeProxyServer wires a real proxy.Handler over httptest.NewServer
// backed by a rangeUpstream and returns an http.Client tuned for
// concurrency.
func newRangeProxyServer(tb testing.TB, fake upstream.Client) (*http.Client, string, func()) {
	tb.Helper()
	dir := tb.TempDir()
	cfg, err := config.ParseFlags(flag.NewFlagSet("x", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", filepath.Join(dir, "cache"),
		"-public-base-url", "http://test.local",
	})
	if err != nil {
		tb.Fatal(err)
	}
	store, err := cache.NewStore(cfg.CacheDir, "none")
	if err != nil {
		tb.Fatal(err)
	}
	h := proxy.NewHandler(cfg, store, fake, logx.New("error"))
	srv := httptest.NewServer(h)
	tr := &http.Transport{
		DisableCompression:  true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		MaxConnsPerHost:     32,
	}
	client := &http.Client{Transport: tr}
	return client, srv.URL, func() {
		tr.CloseIdleConnections()
		srv.Close()
	}
}

// TestParallelDisjointRangeFetches proves that hf-cli-style parallel
// range fetches for the same cache key are no longer serialized by the
// proxy.  The fake upstream sleeps `delay` per request and tracks the
// peak concurrent in-flight Do() calls; with proper range-level
// coordination, all 8 requests execute concurrently and
// maxInFlight == 8.
//
// HARD requirement: maxInFlight MUST be > 1.  We assert >= 4 (loose
// bound to absorb scheduler jitter under -race / CI).  Wall time is
// also checked: it must be << N * delay.
func TestParallelDisjointRangeFetches(t *testing.T) {
	const (
		chunkSize = 64 * 1024
		nChunks   = 8
		total     = chunkSize * nChunks
	)
	payload := make([]byte, total)
	for i := range payload {
		payload[i] = byte(i & 0xff)
	}

	fake := &rangeUpstream{
		body:    payload,
		delay:   80 * time.Millisecond,
		totalCL: int64(total),
	}
	client, base, stop := newRangeProxyServer(t, fake)
	defer stop()

	url := base + "/some/repo/resolve/main/big.bin"

	var wg sync.WaitGroup
	errCh := make(chan error, nChunks)
	results := make([][]byte, nChunks)

	t0 := time.Now()
	for i := 0; i < nChunks; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			rangeHdr := fmt.Sprintf("bytes=%d-%d", i*chunkSize, (i+1)*chunkSize-1)
			req, _ := http.NewRequest(http.MethodGet, url, nil)
			req.Header.Set("Range", rangeHdr)
			resp, err := client.Do(req)
			if err != nil {
				errCh <- fmt.Errorf("chunk %d: %w", i, err)
				return
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				errCh <- fmt.Errorf("chunk %d read: %w", i, err)
				return
			}
			if resp.StatusCode != http.StatusPartialContent {
				errCh <- fmt.Errorf("chunk %d: status=%d", i, resp.StatusCode)
				return
			}
			results[i] = b
		}()
	}
	wg.Wait()
	wallTime := time.Since(t0)
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	for i := 0; i < nChunks; i++ {
		want := payload[i*chunkSize : (i+1)*chunkSize]
		if !bytes.Equal(results[i], want) {
			t.Fatalf("chunk %d body mismatch (got %d bytes)", i, len(results[i]))
		}
	}

	if got := fake.maxInFlight.Load(); got < 4 {
		t.Fatalf("HARD: parallel range fetches were serialized - maxInFlight=%d, want >= 4", got)
	}
	if upper := 3 * fake.delay; wallTime > upper {
		t.Fatalf("HARD: wall time %s > %s - fetches not running in parallel", wallTime, upper)
	}
	t.Logf("parallel range fetches: maxInFlight=%d/%d, wallTime=%s (one delay=%s)",
		fake.maxInFlight.Load(), nChunks, wallTime, fake.delay)
}

// TestDuplicateRangeFetchesDeduplicated proves that two concurrent
// requests for the *same* range still serialize (so the second observes
// a populated cache and serves from disk with 0 upstream bytes).
func TestDuplicateRangeFetchesDeduplicated(t *testing.T) {
	const total = 64 * 1024
	payload := bytes.Repeat([]byte("z"), total)

	fake := &rangeUpstream{
		body:    payload,
		delay:   60 * time.Millisecond,
		totalCL: int64(total),
	}
	client, base, stop := newRangeProxyServer(t, fake)
	defer stop()

	url := base + "/r/resolve/main/big"
	rangeHdr := fmt.Sprintf("bytes=0-%d", total-1)

	const N = 4
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, url, nil)
			req.Header.Set("Range", rangeHdr)
			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("dup range: %v", err)
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !bytes.Equal(b, payload) {
				t.Errorf("dup range body mismatch")
			}
		}()
	}
	wg.Wait()

	if got := fake.fetches.Load(); got != 1 {
		t.Fatalf("HARD: expected 1 upstream fetch (singleflight), got %d", got)
	}
}
