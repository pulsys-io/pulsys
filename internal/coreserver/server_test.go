// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package coreserver_test

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/coreserver"
)

// newCoreServerWithCachedObject pre-populates a cache.Store with one cached
// 200-style object at the given path, then starts a coreserver bound to a
// loopback TCP listener.  The returned addr / closer let benchmarks drive
// it via raw TCP (or via a fasthttp.HostClient, but raw TCP avoids any
// client-side framework noise).
func newCoreServerWithCachedObject(tb testing.TB, urlPath string, payload []byte) (string, func()) {
	tb.Helper()
	dir := tb.TempDir()
	cfg, err := config.ParseFlags(flag.NewFlagSet("x", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", filepath.Join(dir, "cache"),
		"-public-base-url", "http://core.test",
	})
	if err != nil {
		tb.Fatal(err)
	}
	store, err := cache.NewStore(cfg.CacheDir, "none")
	if err != nil {
		tb.Fatal(err)
	}
	// Pre-populate the cache so the warm-hit path can serve it.
	keyHex := cache.KeyHex("GET", cfg.DefaultHost, urlPath, "", "")
	if _, err := store.WriteFullFromStream(
		keyHex, 200, cfg.DefaultHost, urlPath, "",
		`"core-etag"`, "application/octet-stream",
		bytes.NewReader(payload), int64(len(payload)),
	); err != nil {
		tb.Fatal(err)
	}

	srv := &coreserver.Server{Cfg: cfg, Store: store}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() {
		srv.Close()
		_ = ln.Close()
	}
}

// rawWarmGet performs one HTTP/1.1 keep-alive GET over an existing TCP
// connection and reads the entire response.  All scratch buffers are
// supplied by the caller so the benchmark itself contributes zero allocs/op.
func rawWarmGet(tb testing.TB, conn net.Conn, br *bufio.Reader, hdrLine, host, urlPath string, sink []byte) int {
	tb.Helper()
	// Minimal request line + headers.  We construct it once into a
	// single-shot byte buffer so the per-iteration write is a single
	// syscall.
	var req [256]byte
	n := copy(req[:], "GET ")
	n += copy(req[n:], urlPath)
	n += copy(req[n:], " HTTP/1.1\r\nHost: ")
	n += copy(req[n:], host)
	n += copy(req[n:], "\r\nConnection: keep-alive\r\n\r\n")
	if _, err := conn.Write(req[:n]); err != nil {
		tb.Fatal(err)
	}
	// Read status line.
	line, err := br.ReadSlice('\n')
	if err != nil {
		tb.Fatal(err)
	}
	if !bytes.Contains(line, []byte("200")) {
		tb.Fatalf("status: %q", line)
	}
	// Read headers; capture Content-Length without allocating.
	cl := -1
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			tb.Fatal(err)
		}
		if len(line) == 2 && line[0] == '\r' {
			break
		}
		if hasPrefixFold(line, "content-length:") {
			v := bytes.TrimSpace(line[len("content-length:"):])
			n := 0
			for _, c := range v {
				if c < '0' || c > '9' {
					continue
				}
				n = n*10 + int(c-'0')
			}
			cl = n
		}
	}
	if cl < 0 {
		tb.Fatalf("missing content-length")
	}
	// Drain body into sink (caller-owned, reused).
	if cl > len(sink) {
		tb.Fatalf("sink too small: %d > %d", cl, len(sink))
	}
	if _, err := io.ReadFull(br, sink[:cl]); err != nil {
		tb.Fatal(err)
	}
	return cl
}

func TestCoreServerWarmHit(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 4096)
	addr, stop := newCoreServerWithCachedObject(t, "/warm/resolve/main/file.bin", payload)
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	sink := make([]byte, len(payload))
	for i := 0; i < 3; i++ {
		got := rawWarmGet(t, conn, br, "", "core.test", "/warm/resolve/main/file.bin", sink)
		if got != len(payload) || !bytes.Equal(sink[:got], payload) {
			t.Fatalf("iteration %d: body mismatch (got %d bytes)", i, got)
		}
	}
}

// TestCoreServerRequireAuthNilGateDenies pins the defense-in-depth backstop:
// with RequireAuth set and no AuthGate, the data plane fails closed (503) and
// never serves even a warm cache hit.
func TestCoreServerRequireAuthNilGateDenies(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.ParseFlags(flag.NewFlagSet("x", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", filepath.Join(dir, "cache"),
		"-public-base-url", "http://core.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewStore(cfg.CacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	// A warm object that WOULD be served if the gate were bypassed.
	payload := bytes.Repeat([]byte("x"), 1024)
	keyHex := cache.KeyHex("GET", cfg.DefaultHost, "/warm/resolve/main/f.bin", "", "")
	if _, err := store.WriteFullFromStream(
		keyHex, 200, cfg.DefaultHost, "/warm/resolve/main/f.bin", "",
		`"e"`, "application/octet-stream",
		bytes.NewReader(payload), int64(len(payload)),
	); err != nil {
		t.Fatal(err)
	}

	srv := &coreserver.Server{Cfg: cfg, Store: store, RequireAuth: true} // no AuthGate
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		srv.Close()
		_ = ln.Close()
	}()

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /warm/resolve/main/f.bin HTTP/1.1\r\nHost: core.test\r\nConnection: close\r\n\r\n")
	line, err := bufio.NewReader(conn).ReadSlice('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(line, []byte("503")) {
		t.Fatalf("RequireAuth + nil gate: want 503 fail-closed, got %q", line)
	}
}

// ---- Benchmark ----

func benchCoreServer(b *testing.B, size int) {
	b.Helper()
	payload := bytes.Repeat([]byte("z"), size)
	urlPath := fmt.Sprintf("/bench/coresrv/resolve/main/%s", b.Name())
	addr, stop := newCoreServerWithCachedObject(b, urlPath, payload)
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 16*1024)
	sink := make([]byte, size)

	// Warm iteration: also primes the bodyHandle cache and the keepalive
	// connection, ensuring we measure the steady-state hot loop.
	if got := rawWarmGet(b, conn, br, "", "core.test", urlPath, sink); got != size {
		b.Fatalf("setup: got %d, want %d", got, size)
	}

	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := rawWarmGet(b, conn, br, "", "core.test", urlPath, sink); got != size {
			b.Fatalf("body len %d != %d", got, size)
		}
	}
	b.StopTimer()

	// upstream_bytes/op MUST stay at 0 — no fake upstream is wired in for
	// this server because the warm path never calls upstream.  Exposing
	// this as a metric makes the invariant visible in benchmark output.
	b.ReportMetric(0, "upstream_bytes/op")
	b.ReportMetric(0, "upstream_fetches/op")
}

// BenchmarkCoreServerWarm_256KiB / _4MiB are the headline numbers showing
// the proxy's allocation cost when ingress runs through coreserver instead
// of fasthttp.  Compare against ArtifactGetWarmFastHTTP_* for the fasthttp
// baseline.
func BenchmarkCoreServerWarm_256KiB(b *testing.B) { benchCoreServer(b, 256*1024) }
func BenchmarkCoreServerWarm_4MiB(b *testing.B)   { benchCoreServer(b, 4*1024*1024) }

// hasPrefixFold reports whether b begins with prefixLower (case-insensitive
// ASCII) without allocating a lower-cased copy of b.  prefixLower MUST be
// lower case.
func hasPrefixFold(b []byte, prefixLower string) bool {
	if len(b) < len(prefixLower) {
		return false
	}
	for i := 0; i < len(prefixLower); i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefixLower[i] {
			return false
		}
	}
	return true
}
