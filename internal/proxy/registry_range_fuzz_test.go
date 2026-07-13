// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

// p10.3 — range fuzz on the registry resolve path.
//
// The handler implements its own HTTP Range parsing
// (parseSimpleRange) because the stdlib's serveContent path doesn't
// give us hooks for content-addressed blob storage. That makes range
// correctness our problem; this test gives it a 1000-shot workout.

// TestRange_Fuzz_RegistryResolve fires N randomized range requests
// against a known 64 KiB body and asserts the returned bytes equal the
// requested slice. The randomisation covers bounded ranges (a-b),
// open-ended (N-), suffix (-N), zero-length tails, and the body-size
// boundary on both sides.
//
// Deterministic seed so failures bisect: every iteration prints its
// (start, end) on assertion failure.
func TestRange_Fuzz_RegistryResolve(t *testing.T) {
	env := newRegistryEnv(t, nil)
	const size = 64 * 1024
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i & 0xff)
	}
	env.seedRegistryRepo(t, "acme", "fuzz", map[string][]byte{"big.bin": body})

	rng := rand.New(rand.NewPCG(0xdeadbeef, 0xcafebabe))
	const iters = 1000
	for i := 0; i < iters; i++ {
		kind := rng.IntN(6)
		var hdr string
		var wantStart, wantEnd int64

		switch kind {
		case 0: // full open-ended N-
			n := rng.Int64N(size)
			hdr = fmt.Sprintf("bytes=%d-", n)
			wantStart, wantEnd = n, size-1
		case 1: // bounded
			start := rng.Int64N(size)
			end := start + rng.Int64N(size-start)
			hdr = fmt.Sprintf("bytes=%d-%d", start, end)
			wantStart, wantEnd = start, end
		case 2: // suffix
			n := rng.Int64N(size-1) + 1
			hdr = fmt.Sprintf("bytes=-%d", n)
			wantStart, wantEnd = size-n, size-1
		case 3: // one-byte
			n := rng.Int64N(size)
			hdr = fmt.Sprintf("bytes=%d-%d", n, n)
			wantStart, wantEnd = n, n
		case 4: // ends exactly at boundary
			start := rng.Int64N(size)
			hdr = fmt.Sprintf("bytes=%d-%d", start, size-1)
			wantStart, wantEnd = start, size-1
		case 5: // first chunk
			end := rng.Int64N(1024)
			hdr = fmt.Sprintf("bytes=0-%d", end)
			wantStart, wantEnd = 0, end
		}

		req, _ := http.NewRequest(http.MethodGet,
			env.stack.ProxyURL()+"/acme/fuzz/resolve/main/big.bin", nil)
		req.Header.Set("Range", hdr)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("iter=%d range=%q: %v", i, hdr, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("iter=%d range=%q status=%d want 206", i, hdr, resp.StatusCode)
		}
		want := body[wantStart : wantEnd+1]
		if !bytes.Equal(got, want) {
			t.Fatalf("iter=%d range=%q: got %d bytes, want %d bytes (slice [%d,%d])",
				i, hdr, len(got), len(want), wantStart, wantEnd)
		}
		wantCR := fmt.Sprintf("bytes %d-%d/%d", wantStart, wantEnd, size)
		if cr := resp.Header.Get("Content-Range"); cr != wantCR {
			t.Fatalf("iter=%d range=%q Content-Range=%q want %q", i, hdr, cr, wantCR)
		}
	}
}

// TestRange_Fuzz_InvalidRangesAlwaysSafe sends a battery of malformed
// or out-of-bounds ranges. The handler must EITHER return 416
// (parseSimpleRange rejected) OR fall through to a 200 full-body
// (the handler treats unsupported syntax as "no Range"). Crucially it
// must not panic, hang, or 500.
func TestRange_Fuzz_InvalidRangesAlwaysSafe(t *testing.T) {
	env := newRegistryEnv(t, nil)
	const size = 4096
	body := bytes.Repeat([]byte{0xAA}, size)
	env.seedRegistryRepo(t, "acme", "invalid", map[string][]byte{"b": body})

	cases := []string{
		"bytes=-",             // both empty
		"bytes=",              // nothing
		"",                    // empty (treated as no range -> 200)
		"bytes=4097-",         // start past end
		"bytes=0-4096",        // end exactly at size (off-by-one over)
		"bytes=100-50",        // inverted
		"bytes=-0",            // zero-length suffix
		"bytes=-9999999999",   // suffix bigger than body
		"bytes=abc-def",       // non-numeric
		"bytes=0-100,200-300", // multi-range, not supported
		"items=0-10",          // wrong unit
		"bytes 0-10",          // missing =
		"bytes=-",             // double dash empty
		"bytes=----",          // garbage
	}
	for _, hdr := range cases {
		req, _ := http.NewRequest(http.MethodGet,
			env.stack.ProxyURL()+"/acme/invalid/resolve/main/b", nil)
		if hdr != "" {
			req.Header.Set("Range", hdr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("range=%q: %v", hdr, err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusPartialContent, http.StatusOK, http.StatusRequestedRangeNotSatisfiable:
			// allowed
		default:
			t.Fatalf("range=%q produced status=%d (expected 200, 206, or 416)", hdr, resp.StatusCode)
		}
	}
}

// TestRange_MidStreamClientCancel proves that closing the client
// connection in the middle of a large body does not leak goroutines
// in the server. We measure the runtime's goroutine count before and
// after a flurry of canceled downloads.
func TestRange_MidStreamClientCancel(t *testing.T) {
	env := newRegistryEnv(t, nil)
	// 1 MiB body: enough to guarantee the io.CopyN sees the client
	// close mid-write at 10 KiB.
	const size = 1 << 20
	body := bytes.Repeat([]byte{0xCD}, size)
	env.seedRegistryRepo(t, "acme", "cancel", map[string][]byte{"big.bin": body})

	runtime.GC()
	before := runtime.NumGoroutine()

	const N = 32
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
				env.stack.ProxyURL()+"/acme/cancel/resolve/main/big.bin", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				cancel()
				return
			}
			// Read just a few KiB then cancel mid-stream.
			buf := make([]byte, 4*1024)
			_, _ = io.ReadFull(resp.Body, buf)
			cancel()
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()

	// Give the server a moment to unwind in-flight handlers.
	deadline := time.Now().Add(2 * time.Second)
	var after int
	for {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= before+8 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Tolerate a modest residual since http.Client and the test
	// listener carry their own pool routines; we're checking for an
	// unbounded leak, not zero growth.
	if growth := after - before; growth > 32 {
		t.Fatalf("HARD: goroutine leak after %d canceled streams: before=%d after=%d growth=%d",
			N, before, after, growth)
	}
}
