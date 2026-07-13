// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy_test

import (
	"bytes"
	"flag"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/logx"
	"github.com/pulsys-io/pulsys/internal/proxy"
	"github.com/pulsys-io/pulsys/internal/telemetry"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

// newProxyServerBudget is newProxyServer with an explicit inflight-acquire
// budget and direct access to the underlying Store, so the test can plant a
// synthetic in-flight holder to simulate contention (e.g. a concurrent
// import or whole-file download holding the range).
func newProxyServerBudget(tb testing.TB, fake upstream.Client, budget time.Duration) (*http.Client, string, *cache.Store, string, func()) {
	tb.Helper()
	dir := tb.TempDir()
	cfg, err := config.ParseFlags(flag.NewFlagSet("x", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", filepath.Join(dir, "cache"),
		"-public-base-url", "http://test.local",
		"-inflight-acquire-timeout", budget.String(),
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
	tr := &http.Transport{MaxIdleConns: 16, MaxIdleConnsPerHost: 16, DisableCompression: true}
	client := &http.Client{Transport: tr}
	return client, srv.URL, store, cfg.DefaultHost, func() {
		tr.CloseIdleConnections()
		srv.Close()
	}
}

// TestInflightContendedPassthrough asserts that when an artifact GET cannot
// claim its byte range within the budget (because another whole-file fetch
// is in flight), the public ingress falls through to a non-caching
// pass-through fetch — serving the client promptly instead of blocking past
// its header read timeout — and that normal caching resumes once the
// contention clears.
func TestInflightContendedPassthrough(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 48*1024)
	fake := &fakeUpstream{}
	fake.set(&fakeResp{status: 200, body: payload, contentType: "application/octet-stream", etag: `"zz"`})

	const budget = 200 * time.Millisecond
	client, base, store, host, stop := newProxyServerBudget(t, fake, budget)
	defer stop()

	path := "/some-org/some-model/resolve/main/weights.bin"
	key := cache.KeyHex(http.MethodGet, host, path, "", "")

	// Plant a synthetic in-flight holder over the whole file, exactly as a
	// concurrent no-Range GET / import chunk fetch would.
	holder := store.AcquireRange(key, 0, math.MaxInt64)

	beforePT := telemetry.InflightContendedPassthroughSnapshot()
	beforeFetch := fake.fetches.Load()

	start := time.Now()
	status, got := drainGet(t, client, base, path, nil)
	elapsed := time.Since(start)

	if status != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("contended GET status=%d body_len=%d (want 200, %d)", status, len(got), len(payload))
	}
	if delta := telemetry.InflightContendedPassthroughSnapshot() - beforePT; delta != 1 {
		t.Fatalf("contended passthrough counter delta=%d, want 1 (key mismatch or did not pass through)", delta)
	}
	if delta := fake.fetches.Load() - beforeFetch; delta != 1 {
		t.Fatalf("passthrough upstream fetches delta=%d, want 1", delta)
	}
	// Sanity: it returned roughly at the budget, not instantly (proves it
	// actually waited the budget) and not far past it (proves it did not
	// block on the holder).
	if elapsed > budget+2*time.Second {
		t.Fatalf("contended GET took %v, far past budget %v (blocked on holder?)", elapsed, budget)
	}

	// The passthrough must NOT have populated the cache.  Release the
	// holder; the next GET is a genuine cold miss that fetches + caches,
	// and the one after is a warm hit with zero upstream fetches.
	holder()

	fetchesBeforeCold := fake.fetches.Load()
	if status, got = drainGet(t, client, base, path, nil); status != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("post-release cold GET status=%d len=%d", status, len(got))
	}
	if delta := fake.fetches.Load() - fetchesBeforeCold; delta != 1 {
		t.Fatalf("post-release cold fetches delta=%d, want 1 (passthrough wrongly cached?)", delta)
	}

	fetchesBeforeWarm := fake.fetches.Load()
	if status, got = drainGet(t, client, base, path, nil); status != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("warm GET status=%d len=%d", status, len(got))
	}
	if delta := fake.fetches.Load() - fetchesBeforeWarm; delta != 0 {
		t.Fatalf("HARD: warm GET after contention cleared made %d upstream fetches, want 0", delta)
	}
}

// TestInflightUncontendedNoPassthrough is the control: with no overlapping
// holder, an artifact GET takes the normal caching path and never increments
// the contended-passthrough counter.
func TestInflightUncontendedNoPassthrough(t *testing.T) {
	payload := bytes.Repeat([]byte("q"), 16*1024)
	fake := &fakeUpstream{}
	fake.set(&fakeResp{status: 200, body: payload, contentType: "application/octet-stream"})

	client, base, _, _, stop := newProxyServerBudget(t, fake, 200*time.Millisecond)
	defer stop()

	path := "/org/model/resolve/main/clean.bin"
	beforePT := telemetry.InflightContendedPassthroughSnapshot()

	if status, got := drainGet(t, client, base, path, nil); status != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("cold GET status=%d len=%d", status, len(got))
	}
	// Warm hit proves it cached normally.
	fetchesBefore := fake.fetches.Load()
	if status, _ := drainGet(t, client, base, path, nil); status != 200 {
		t.Fatalf("warm GET status=%d", status)
	}
	if delta := fake.fetches.Load() - fetchesBefore; delta != 0 {
		t.Fatalf("HARD: warm GET made %d upstream fetches, want 0", delta)
	}
	if delta := telemetry.InflightContendedPassthroughSnapshot() - beforePT; delta != 0 {
		t.Fatalf("uncontended path incremented passthrough counter by %d, want 0", delta)
	}
}
