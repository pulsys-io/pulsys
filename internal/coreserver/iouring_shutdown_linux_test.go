// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux

// Reactor shutdown regression tests.
//
// The io_uring reactor used to leak its main goroutine across
// Server.Close():
//
//   - run() blocked in ring.enter(toSubmit, 1), waiting on a CQE
//     that only ever arrives when a client touches the listener.
//   - Close() only signalled the stop channel; the channel is
//     read at the TOP of the loop, not from inside enter(), so
//     a quiet reactor never observed the signal.
//   - Production effect: every restart left a per-listener
//     reactor goroutine pinning ring memory + fds for the rest
//     of the host's lifetime.
//   - Test effect: t.Cleanup hangs that masquerade as flakes.
//
// The fix arms an IORING_OP_READ on a per-reactor eventfd; Close()
// pokes the eventfd to complete the SQE and wake enter().  The
// tests below pin that contract — if a regression re-introduces
// the leak, these tests fail in <1s rather than hanging the
// suite.
package coreserver_test

import (
	"bufio"
	"bytes"
	"flag"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/coreserver"
)

// TestReactor_CloseDrainsCleanly_NoTraffic asserts that
// Server.Close() returns within a small window even when the
// reactor has been quietly waiting on the ring (no traffic since
// startup).  Pre-fix this would hang indefinitely.
func TestReactor_CloseDrainsCleanly_NoTraffic(t *testing.T) {
	srv, ln, cleanup := newIoUringSrv(t)
	defer cleanup()

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	// Give the reactor a beat to enter its main loop and arm
	// the wakeup READ SQE.  100ms is way over what's required;
	// we just need the goroutine to have parked in enter().
	time.Sleep(100 * time.Millisecond)

	before := goroutineCount()

	closeDone := make(chan struct{})
	go func() {
		srv.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		dumpGoroutines(t)
		t.Fatalf("Server.Close() hung > 5s with no traffic — eventfd wakeup regression?")
	}

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		dumpGoroutines(t)
		t.Fatalf("Serve() did not return within 5s after Close() — runReactors leak?")
	}

	_ = ln.Close()

	// Allow stragglers (Go runtime cleanup, the Close() goroutine
	// itself) to settle before counting.
	settleGoroutines(50 * time.Millisecond)

	after := goroutineCount()
	if delta := after - before; delta > 2 {
		dumpGoroutines(t)
		t.Fatalf("reactor leaked goroutines across Close(): before=%d after=%d (delta=%d)",
			before, after, delta)
	}
}

// TestReactor_CloseDrainsCleanly_AfterTraffic exercises the
// shutdown path AFTER the reactor has served real requests
// (warm hits + at least one detached fallback).  Asserts both
// run() and inFlight detached goroutines drain on Close().
func TestReactor_CloseDrainsCleanly_AfterTraffic(t *testing.T) {
	srv, ln, cleanup := newIoUringSrv(t)
	defer cleanup()

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	// Drive a few warm hits to exercise the io_uring fast path.
	host := srv.Cfg.DefaultHost
	for i := 0; i < 4; i++ {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		br := bufio.NewReader(conn)
		var sink [8192]byte
		_ = rawWarmGet(t, conn, br, "HTTP/1.1", host, "/models/bench/bench/resolve/main/4k.bin", sink[:])
		_ = conn.Close()
	}

	// Drive one non-warm request to push something onto the
	// detached fallback path.  PUT is non-cacheable, so the
	// reactor must hand off to runDetached.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("detached dial: %v", err)
	}
	// We don't care about the response; we care that the
	// request gets pushed through the in-flight WaitGroup so
	// Close() exercises waitInFlight.
	_, _ = conn.Write([]byte("PUT /not-cacheable HTTP/1.1\r\nHost: " + host + "\r\nContent-Length: 0\r\n\r\n"))
	_ = conn.Close()

	before := goroutineCount()

	closeDone := make(chan struct{})
	go func() {
		srv.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
	case <-time.After(15 * time.Second):
		dumpGoroutines(t)
		t.Fatalf("Server.Close() hung > 15s after traffic — inFlight WaitGroup never drained?")
	}

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		dumpGoroutines(t)
		t.Fatalf("Serve() did not return within 5s after Close()")
	}

	_ = ln.Close()
	settleGoroutines(150 * time.Millisecond)

	after := goroutineCount()
	// We allow a slightly higher slack here because runtime
	// netpoller bookkeeping after a burst of net.Dial calls
	// can briefly survive Close().  >5 is a real leak.
	if delta := after - before; delta > 5 {
		dumpGoroutines(t)
		t.Fatalf("reactor leaked goroutines across Close(): before=%d after=%d (delta=%d)",
			before, after, delta)
	}
}

// TestReactor_CloseIdempotent ensures multiple Close() calls
// don't panic on closed eventfd writes or double-closed chans.
func TestReactor_CloseIdempotent(t *testing.T) {
	srv, ln, cleanup := newIoUringSrv(t)
	defer cleanup()

	go func() { _ = srv.Serve(ln) }()
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 4; i++ {
		srv.Close()
	}
	_ = ln.Close()
}

// ---- helpers ----

func newIoUringSrv(t *testing.T) (*coreserver.Server, net.Listener, func()) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.ParseFlags(flag.NewFlagSet("x", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", filepath.Join(dir, "cache"),
		"-public-base-url", "http://core.test",
		"-tcp-cork=false",
		"-iouring=true",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 4096)
	store, err := cache.NewStore(cfg.CacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	path := "/models/bench/bench/resolve/main/4k.bin"
	keyHex := cache.KeyHex("GET", cfg.DefaultHost, path, "", "")
	if _, err := store.WriteFullFromStream(
		keyHex, 200, cfg.DefaultHost, path, "",
		`"etag"`, "application/octet-stream",
		bytes.NewReader(payload), int64(len(payload)),
	); err != nil {
		t.Fatal(err)
	}
	srv := &coreserver.Server{
		Cfg:     cfg,
		Store:   store,
		TCPCork: false,
		IoUring: true,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return srv, ln, func() {
		// Last-resort cleanup: we ignore errors because the
		// individual tests already Close() / ln.Close() in
		// happy paths.  This guards against early t.Fatal
		// leaving the reactor running.
		_ = ln.Close()
		srv.Close()
	}
}

func goroutineCount() int { return runtime.NumGoroutine() }

func settleGoroutines(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
}

// dumpGoroutines emits a full stack trace to the test log when a
// shutdown assertion trips.  The leaked frames make regressions
// trivially diagnosable.  We bound the dump size so we don't
// flood CI on a runaway suite.
func dumpGoroutines(t *testing.T) {
	t.Helper()
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	t.Logf("=== goroutine dump (%d bytes) ===\n%s", n, truncateForLog(string(buf[:n]), 16*1024))
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
