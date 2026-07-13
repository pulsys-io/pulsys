// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/logx"
	"github.com/pulsys-io/pulsys/internal/proxy"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

// fakeUpstream implements upstream.Client.  All counters are incremented
// synchronously inside Do(), so tests/benchmarks reading them after the HTTP
// client has finished a request observe race-free totals.
type fakeUpstream struct {
	resp     atomic.Pointer[fakeResp]
	fetches  atomic.Int64 // number of Do() calls
	bytesOut atomic.Int64 // sum of body bytes returned to caller
}

type fakeResp struct {
	status       int
	body         []byte
	contentType  string
	etag         string
	contentLen   int64  // -1 to force chunked reply (no Content-Length header)
	contentRange string // optional, used for 206
}

func (f *fakeUpstream) set(r *fakeResp) { f.resp.Store(r) }

func (f *fakeUpstream) Do(_ context.Context, _, _, _, _ string, _ http.Header, _ []byte) (*upstream.Response, error) {
	r := f.resp.Load()
	if r == nil {
		return nil, errors.New("fakeUpstream: no canned response set")
	}
	f.fetches.Add(1)
	f.bytesOut.Add(int64(len(r.body)))
	h := http.Header{}
	if r.contentType != "" {
		h.Set("Content-Type", r.contentType)
	}
	if r.etag != "" {
		h.Set("ETag", r.etag)
	}
	cl := r.contentLen
	switch {
	case cl == 0:
		cl = int64(len(r.body))
		h.Set("Content-Length", strconv.FormatInt(cl, 10))
	case cl > 0:
		h.Set("Content-Length", strconv.FormatInt(cl, 10))
	default: // -1: chunked
		h.Del("Content-Length")
	}
	if r.contentRange != "" {
		h.Set("Content-Range", r.contentRange)
	}
	return &upstream.Response{
		Status:        r.status,
		Header:        h,
		ContentLength: cl,
		Body:          io.NopCloser(bytes.NewReader(r.body)),
	}, nil
}

// newProxyServer wires a real proxy.Handler over an httptest.Server with the
// supplied fake upstream and returns an http.Client that talks to it.
//
// The handler is the same proxy.Handler that runs in production behind
// internal/coreserver; tests exercise the slow-path (net/http) entry point
// here so that they cover the fallback path coreserver delegates to.
func newProxyServer(tb testing.TB, fake upstream.Client) (*http.Client, string, func()) {
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
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		MaxConnsPerHost:     32,
		DisableCompression:  true,
		ForceAttemptHTTP2:   false,
	}
	client := &http.Client{Transport: tr}
	return client, srv.URL, func() {
		tr.CloseIdleConnections()
		srv.Close()
	}
}

// drainGet performs a GET against the proxy under test, fully reads the body,
// and returns it.  The supplied path is appended to the test server URL so the
// Host header is the test server's loopback host (proxy.Handler does not
// require any specific Host -- it routes by URL path).
func drainGet(tb testing.TB, c *http.Client, base, path string, hdrs map[string]string) (int, []byte) {
	tb.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		tb.Fatal(err)
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		tb.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		tb.Fatal(err)
	}
	return resp.StatusCode, b
}

// TestWarmArtifactGetZeroUpstream is the hard-requirement assertion: a
// second GET for the same artifact path must NOT trigger any upstream
// Do() calls and must NOT pull any upstream body bytes.
func TestWarmArtifactGetZeroUpstream(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), 64*1024)
	fake := &fakeUpstream{}
	fake.set(&fakeResp{
		status: 200, body: payload,
		contentType: "application/octet-stream",
		etag:        `"abc"`,
	})

	client, base, stop := newProxyServer(t, fake)
	defer stop()
	path := "/openai-community/gpt2/resolve/main/big.bin"

	// Cold
	status, got := drainGet(t, client, base, path, nil)
	if status != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("cold status=%d body_len=%d", status, len(got))
	}
	if fake.fetches.Load() != 1 {
		t.Fatalf("cold expected 1 upstream fetch, got %d", fake.fetches.Load())
	}
	if fake.bytesOut.Load() != int64(len(payload)) {
		t.Fatalf("cold expected %d upstream bytes, got %d", len(payload), fake.bytesOut.Load())
	}

	// Warm
	fetchesAfterCold := fake.fetches.Load()
	bytesAfterCold := fake.bytesOut.Load()

	status, got = drainGet(t, client, base, path, nil)
	if status != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("warm status=%d body_len=%d", status, len(got))
	}
	if delta := fake.fetches.Load() - fetchesAfterCold; delta != 0 {
		t.Fatalf("HARD: warm artifact upstream fetches = %d, want 0", delta)
	}
	if delta := fake.bytesOut.Load() - bytesAfterCold; delta != 0 {
		t.Fatalf("HARD: warm artifact upstream body bytes = %d, want 0", delta)
	}
}

// TestWarmRangeHitsCache: warm Range request fully covered by a cached
// 200 must serve from disk with 0 upstream bytes.
func TestWarmRangeHitsCache(t *testing.T) {
	payload := bytes.Repeat([]byte("b"), 32*1024)
	fake := &fakeUpstream{}
	fake.set(&fakeResp{status: 200, body: payload, contentType: "application/octet-stream"})

	client, base, stop := newProxyServer(t, fake)
	defer stop()
	path := "/some/repo/resolve/main/file.bin"

	if _, _ = drainGet(t, client, base, path, nil); fake.fetches.Load() != 1 {
		t.Fatalf("cold expected 1 fetch")
	}

	fetchesBefore := fake.fetches.Load()
	bytesBefore := fake.bytesOut.Load()

	status, slice := drainGet(t, client, base, path, map[string]string{"Range": "bytes=100-199"})
	if status != http.StatusPartialContent {
		t.Fatalf("range expected 206, got %d", status)
	}
	if !bytes.Equal(slice, payload[100:200]) {
		t.Fatalf("range body mismatch: got %d bytes", len(slice))
	}
	if got := fake.fetches.Load() - fetchesBefore; got != 0 {
		t.Fatalf("HARD: warm range upstream fetches = %d, want 0", got)
	}
	if got := fake.bytesOut.Load() - bytesBefore; got != 0 {
		t.Fatalf("HARD: warm range upstream bytes = %d, want 0", got)
	}
}

// TestChunkedArtifactPersists exercises the unknown-Content-Length /
// chunked 200 path: every byte must end up on disk and the warm GET
// must serve from disk (0 upstream bytes).
func TestChunkedArtifactPersists(t *testing.T) {
	payload := bytes.Repeat([]byte("c"), 33*1024)
	fake := &fakeUpstream{}
	fake.set(&fakeResp{
		status: 200, body: payload, contentType: "application/octet-stream",
		contentLen: -1, // chunked
	})

	client, base, stop := newProxyServer(t, fake)
	defer stop()
	path := "/repo/resolve/main/chunked.bin"

	status, got := drainGet(t, client, base, path, nil)
	if status != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("chunked cold mismatch: status=%d len=%d", status, len(got))
	}

	fetchesBefore := fake.fetches.Load()
	bytesBefore := fake.bytesOut.Load()

	status, got = drainGet(t, client, base, path, nil)
	if status != 200 || !bytes.Equal(got, payload) {
		t.Fatalf("chunked warm mismatch: status=%d len=%d", status, len(got))
	}
	if got := fake.fetches.Load() - fetchesBefore; got != 0 {
		t.Fatalf("HARD: chunked warm fetches = %d, want 0", got)
	}
	if got := fake.bytesOut.Load() - bytesBefore; got != 0 {
		t.Fatalf("HARD: chunked warm bytes = %d, want 0", got)
	}
}

// TestRange206Persists: 206 with known total + Range hit serves from disk.
func TestRange206Persists(t *testing.T) {
	payload := bytes.Repeat([]byte("r"), 8*1024)
	fake := &fakeUpstream{}
	first := payload[:1024]
	fake.set(&fakeResp{
		status: 206, body: first,
		contentType:  "application/octet-stream",
		contentLen:   1024,
		contentRange: "bytes 0-1023/8192",
	})
	client, base, stop := newProxyServer(t, fake)
	defer stop()
	path := "/r/resolve/main/big"

	status, got := drainGet(t, client, base, path, map[string]string{"Range": "bytes=0-1023"})
	if status != http.StatusPartialContent || !bytes.Equal(got, first) {
		t.Fatalf("206 cold: status=%d len=%d", status, len(got))
	}

	fetchesBefore := fake.fetches.Load()
	bytesBefore := fake.bytesOut.Load()

	status, got = drainGet(t, client, base, path, map[string]string{"Range": "bytes=0-1023"})
	if status != http.StatusPartialContent || !bytes.Equal(got, first) {
		t.Fatalf("206 warm: status=%d len=%d", status, len(got))
	}
	if got := fake.fetches.Load() - fetchesBefore; got != 0 {
		t.Fatalf("HARD: warm 206 fetches = %d, want 0", got)
	}
	if got := fake.bytesOut.Load() - bytesBefore; got != 0 {
		t.Fatalf("HARD: warm 206 bytes = %d, want 0", got)
	}
}

// ---- Benchmarks (use Go's built-in framework + b.SetBytes / b.ReportMetric) ----

func benchOnce(b *testing.B, size int, mode string) {
	b.Helper()
	payload := bytes.Repeat([]byte("z"), size)
	fake := &fakeUpstream{}
	fake.set(&fakeResp{status: 200, body: payload, contentType: "application/octet-stream"})
	client, base, stop := newProxyServer(b, fake)
	defer stop()

	urlBase := "/bench/repo/resolve/main/"
	if mode == "warm" {
		_, body := drainGet(b, client, base, urlBase+"warm-"+b.Name(), nil)
		if len(body) != size {
			b.Fatalf("warm setup body len mismatch: %d", len(body))
		}
	}

	fetchBase := fake.fetches.Load()
	bytesBase := fake.bytesOut.Load()

	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var p string
		if mode == "warm" {
			p = urlBase + "warm-" + b.Name()
		} else {
			p = urlBase + "cold-" + b.Name() + "-" + strconv.Itoa(i)
		}
		_, body := drainGet(b, client, base, p, nil)
		if len(body) != size {
			b.Fatalf("body len %d != %d", len(body), size)
		}
	}
	b.StopTimer()

	fetches := fake.fetches.Load() - fetchBase
	upBytes := fake.bytesOut.Load() - bytesBase
	b.ReportMetric(float64(upBytes)/float64(b.N), "upstream_bytes/op")
	b.ReportMetric(float64(fetches)/float64(b.N), "upstream_fetches/op")
}

// BenchmarkArtifactGetCold: every iteration is a fresh key (cache miss)
// -> upstream_bytes/op should equal payload size.
func BenchmarkArtifactGetCold_256KiB(b *testing.B) { benchOnce(b, 256*1024, "cold") }
func BenchmarkArtifactGetCold_4MiB(b *testing.B)   { benchOnce(b, 4*1024*1024, "cold") }

// BenchmarkArtifactGetWarm: same key reused -> upstream_bytes/op MUST
// be 0.  This is the headline "0 B per operation" benchmark.
func BenchmarkArtifactGetWarm_256KiB(b *testing.B) { benchOnce(b, 256*1024, "warm") }
func BenchmarkArtifactGetWarm_4MiB(b *testing.B)   { benchOnce(b, 4*1024*1024, "warm") }

// BenchmarkArtifactGetWarmParallel exercises lock contention on a
// single warm key.
func BenchmarkArtifactGetWarmParallel_256KiB(b *testing.B) {
	const size = 256 * 1024
	payload := bytes.Repeat([]byte("p"), size)
	fake := &fakeUpstream{}
	fake.set(&fakeResp{status: 200, body: payload, contentType: "application/octet-stream"})
	client, base, stop := newProxyServer(b, fake)
	defer stop()
	path := "/bench/parallel/resolve/main/file"
	if _, body := drainGet(b, client, base, path, nil); len(body) != size {
		b.Fatalf("setup")
	}
	fetchBase := fake.fetches.Load()
	bytesBase := fake.bytesOut.Load()
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, body := drainGet(b, client, base, path, nil)
			if len(body) != size {
				b.Fatalf("body len")
			}
		}
	})
	b.StopTimer()
	fetches := fake.fetches.Load() - fetchBase
	upBytes := fake.bytesOut.Load() - bytesBase
	b.ReportMetric(float64(upBytes)/float64(b.N), "upstream_bytes/op")
	b.ReportMetric(float64(fetches)/float64(b.N), "upstream_fetches/op")
}
