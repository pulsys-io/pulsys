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
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/coreserver"
)

// These tests pin the sliding write-deadline behaviour: a client that
// keeps draining bytes must be allowed to finish a transfer that lasts
// far longer than the absolute WriteTimeout, while a client that stalls
// for the whole WriteTimeout window must still be cut.  They exercise
// both serving paths reachable from the classic (non-io_uring) server:
//
//   - warm: tryServeWarm sendfile + the size-gated write-deadline pump
//     (Mechanism B).
//   - cold: the fallback respWriter sliding SetWriteDeadline (Mechanism A).
//
// The io_uring reactor SO_SNDTIMEO path (Mechanism C) is covered by the
// in-package linux-only test in iouring_sndtimeo_linux_test.go.

// zeroReader yields an endless stream of (uninitialised) bytes; content
// is irrelevant for these throughput/timeout tests, only length matters.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return len(p), nil }

type slowServerOpts struct {
	writeTimeout time.Duration
	warmPath     string // if non-empty, pre-populate this path with warmSize bytes
	warmSize     int64
	fallbackSize int // bytes the fallback handler streams for any request
}

// startSlowServer boots a classic coreserver with a configurable
// WriteTimeout, an optional pre-cached warm object, and a fallback
// handler that streams fallbackSize bytes with an explicit
// Content-Length (so the respWriter content-length / ReadFrom path is
// exercised).
func startSlowServer(tb testing.TB, opts slowServerOpts) (addr, host string, stop func()) {
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
	if opts.warmPath != "" {
		keyHex := cache.KeyHex("GET", cfg.DefaultHost, opts.warmPath, "", "")
		if _, err := store.WriteFullFromStream(
			keyHex, 200, cfg.DefaultHost, opts.warmPath, "",
			`"slow-etag"`, "application/octet-stream",
			io.LimitReader(zeroReader{}, opts.warmSize), opts.warmSize,
		); err != nil {
			tb.Fatal(err)
		}
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(opts.fallbackSize))
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, io.LimitReader(zeroReader{}, int64(opts.fallbackSize)))
	})

	srv := &coreserver.Server{
		Cfg:          cfg,
		Store:        store,
		Fallback:     coreserver.HandlerFallback(h),
		WriteTimeout: opts.writeTimeout,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), cfg.DefaultHost, func() {
		srv.Close()
		_ = ln.Close()
	}
}

// sendGet writes a single Connection: close GET request.
func sendGet(tb testing.TB, conn net.Conn, host, path string) {
	tb.Helper()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, host); err != nil {
		tb.Fatal(err)
	}
}

// readHead consumes the status line + header block and returns the
// declared Content-Length (or -1 if absent / chunked).
func readHead(tb testing.TB, br *bufio.Reader) (status int, contentLen int) {
	tb.Helper()
	line, err := br.ReadSlice('\n')
	if err != nil {
		tb.Fatalf("read status: %v", err)
	}
	// HTTP/1.1 NNN ...
	fields := bytes.SplitN(line, []byte(" "), 3)
	if len(fields) < 2 {
		tb.Fatalf("bad status line: %q", line)
	}
	status, _ = strconv.Atoi(string(fields[1]))
	contentLen = -1
	for {
		hl, err := br.ReadSlice('\n')
		if err != nil {
			tb.Fatalf("read header: %v", err)
		}
		if len(hl) == 2 && hl[0] == '\r' {
			break
		}
		if hasPrefixFold(hl, "content-length:") {
			v := bytes.TrimSpace(hl[len("content-length:"):])
			if n, err := strconv.Atoi(string(v)); err == nil {
				contentLen = n
			}
		}
	}
	return status, contentLen
}

// readBodyProgressively drains total bytes in fixed chunks, sleeping gap
// between each read so the overall transfer outlasts WriteTimeout while
// each individual idle window stays well under it.  Returns the byte
// count read and any error.
func readBodyProgressively(br *bufio.Reader, total, chunk int, gap time.Duration) (int, error) {
	buf := make([]byte, chunk)
	got := 0
	for got < total {
		time.Sleep(gap)
		want := chunk
		if total-got < want {
			want = total - got
		}
		n, err := io.ReadFull(br, buf[:want])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

// TestSlidingDeadline_Warm_ProgressingClientCompletes proves a large warm
// (sendfile) transfer to a slow-but-steady client finishes even though it
// takes several multiples of the WriteTimeout.
func TestSlidingDeadline_Warm_ProgressingClientCompletes(t *testing.T) {
	if testing.Short() {
		t.Skip("slow timing test")
	}
	const warmSize = 40 << 20 // > warmDeadlinePumpThreshold (32 MiB) so the pump engages
	path := "/slow/warm/resolve/main/big.bin"
	addr, host, stop := startSlowServer(t, slowServerOpts{
		writeTimeout: 2 * time.Second,
		warmPath:     path,
		warmSize:     warmSize,
	})
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReaderSize(conn, 64*1024)

	sendGet(t, conn, host, path)
	status, cl := readHead(t, br)
	if status != 200 || cl != warmSize {
		t.Fatalf("warm head: status=%d cl=%d want 200/%d", status, cl, warmSize)
	}

	// 20 chunks * 150ms ≈ 3s of read time, well past the 2s WriteTimeout,
	// but every idle gap (150ms) is far under it.
	got, err := readBodyProgressively(br, cl, 2<<20, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("progressing warm read cut short at %d/%d bytes: %v", got, cl, err)
	}
	if got != warmSize {
		t.Fatalf("warm body short: got %d want %d", got, warmSize)
	}
}

// TestSlidingDeadline_Cold_ProgressingClientCompletes proves the same for
// the fallback (respWriter) streaming path.
func TestSlidingDeadline_Cold_ProgressingClientCompletes(t *testing.T) {
	if testing.Short() {
		t.Skip("slow timing test")
	}
	const bodySize = 8 << 20
	addr, host, stop := startSlowServer(t, slowServerOpts{
		writeTimeout: 2 * time.Second,
		fallbackSize: bodySize,
	})
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReaderSize(conn, 64*1024)

	sendGet(t, conn, host, "/cold/blob")
	status, cl := readHead(t, br)
	if status != 200 || cl != bodySize {
		t.Fatalf("cold head: status=%d cl=%d want 200/%d", status, cl, bodySize)
	}

	// 16 chunks * 150ms ≈ 2.4s read time > 2s WriteTimeout.
	got, err := readBodyProgressively(br, cl, 512<<10, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("progressing cold read cut short at %d/%d bytes: %v", got, cl, err)
	}
	if got != bodySize {
		t.Fatalf("cold body short: got %d want %d", got, bodySize)
	}
}

// readBodyStalled does NOT read for stallFor (longer than WriteTimeout),
// letting the server's send block and trip the idle deadline, then drains
// whatever is left until EOF/reset.  Returns the total body bytes read.
func readBodyStalled(br *bufio.Reader, conn net.Conn, stallFor time.Duration) int {
	time.Sleep(stallFor)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1<<20)
	got := 0
	for {
		n, err := br.Read(buf)
		got += n
		if err != nil {
			return got
		}
	}
}

// TestSlidingDeadline_Warm_StalledClientCut proves a warm transfer to a
// client that stops reading is cut after ~WriteTimeout rather than
// allowed to run forever (or pin the server).
func TestSlidingDeadline_Warm_StalledClientCut(t *testing.T) {
	if testing.Short() {
		t.Skip("slow timing test")
	}
	const warmSize = 40 << 20 // larger than any plausible socket buffer so the send blocks
	path := "/slow/warm/resolve/main/stall.bin"
	addr, host, stop := startSlowServer(t, slowServerOpts{
		writeTimeout: 800 * time.Millisecond,
		warmPath:     path,
		warmSize:     warmSize,
	})
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 64*1024)

	sendGet(t, conn, host, path)
	status, cl := readHead(t, br)
	if status != 200 || cl != warmSize {
		t.Fatalf("warm head: status=%d cl=%d want 200/%d", status, cl, warmSize)
	}

	got := readBodyStalled(br, conn, 2500*time.Millisecond)
	if got >= warmSize {
		t.Fatalf("stalled warm client got full body (%d bytes); deadline was not enforced", got)
	}
}

// TestSlidingDeadline_Cold_StalledClientCut proves the same for the
// fallback streaming path.
func TestSlidingDeadline_Cold_StalledClientCut(t *testing.T) {
	if testing.Short() {
		t.Skip("slow timing test")
	}
	const bodySize = 40 << 20
	addr, host, stop := startSlowServer(t, slowServerOpts{
		writeTimeout: 800 * time.Millisecond,
		fallbackSize: bodySize,
	})
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 64*1024)

	sendGet(t, conn, host, "/cold/blob")
	status, cl := readHead(t, br)
	if status != 200 || cl != bodySize {
		t.Fatalf("cold head: status=%d cl=%d want 200/%d", status, cl, bodySize)
	}

	got := readBodyStalled(br, conn, 2500*time.Millisecond)
	if got >= bodySize {
		t.Fatalf("stalled cold client got full body (%d bytes); deadline was not enforced", got)
	}
}
