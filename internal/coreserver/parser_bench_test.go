// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Parser hot-path benchmark.
//
// PURPOSE
//   The stricter parser fixes added in the OWASP WSTG hardening
//   pass (TE/CL refusal, dup-CL, bare-CR, header-value validity,
//   request-target validity) MUST not regress warm-hit TTFB.  This
//   benchmark measures the per-request parse cost in isolation
//   (without the server loop or sendfile path) so a regression
//   shows up as a clear delta rather than buried in end-to-end noise.
//
// HOW TO USE
//   Establish a baseline before parser edits:
//
//     git stash
//     go test -bench=BenchmarkReadRequestWarm -benchmem -benchtime=10s ./internal/coreserver/ > /tmp/before.txt
//     git stash pop
//     go test -bench=BenchmarkReadRequestWarm -benchmem -benchtime=10s ./internal/coreserver/ > /tmp/after.txt
//     benchstat /tmp/before.txt /tmp/after.txt
//
//   Expectation: ns/op delta below 5% on the canonical warm-hit
//   header block.  If the delta is larger, hoist the TE/CL switch
//   into a per-header byte-0 dispatch and re-measure.
//
//   TestWarmHitAllocFloor already proves the allocation floor
//   stays at the documented 2-3 allocs/op for the FULL warm-hit
//   path (parse + auth gate + sendfile); this micro-bench
//   complements it by isolating the parser cost.

package coreserver_test

import (
	"bytes"
	"testing"

	"github.com/pulsys-io/pulsys/internal/coreserver"
)

// canonicalWarmHeader is the prototypical warm-hit request shape
// the hf-cli / huggingface_hub library issues.  Keep this in sync
// with the production header layout; if a real client adds more
// headers the bench should grow with it so the measurement stays
// representative.
var canonicalWarmHeader = []byte(
	"GET /openai-community/gpt2/resolve/main/config.json HTTP/1.1\r\n" +
		"Host: huggingface.co\r\n" +
		"User-Agent: huggingface_hub/0.27.0; python/3.11.0\r\n" +
		"Authorization: Bearer pulsys_dead000000000000000000000000000000000000\r\n" +
		"Accept-Encoding: gzip, deflate, zstd\r\n" +
		"Accept: */*\r\n" +
		"Connection: keep-alive\r\n" +
		"\r\n",
)

// BenchmarkReadRequestWarm measures the steady-state cost of parsing
// one canonical warm-hit request.  No I/O, no server loop, no
// network: just bytes.Reader -> bufio.Reader -> readRequest ->
// validation.  Per-iteration cost should track the production warm
// path's parse step within measurement noise.
func BenchmarkReadRequestWarm(b *testing.B) {
	b.SetBytes(int64(len(canonicalWarmHeader)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// We construct a fresh wrapper per iteration so the test
		// measures the steady-state shape of an incoming request
		// (the bufio reader is fresh per conn in production, too,
		// just pooled).  The bytes.Reader allocation is the same
		// fixed cost as production's net.Conn read; what we care
		// about is the delta the parser changes introduce.
		req, _, err := coreserver.ReadRequestBytesForTest(canonicalWarmHeader)
		if err != nil {
			b.Fatalf("warm-hit parse failed: %v", err)
		}
		// Sanity gate so an accidentally lenient parser
		// (e.g. always-accept) doesn't game the bench.
		if !bytes.Equal(req.Method, []byte("GET")) {
			b.Fatalf("expected GET, got %q", req.Method)
		}
	}
}

// BenchmarkReadRequestHostOnly is the absolute minimum-cost parse:
// a Host header is the only required header.  Useful as the lower
// bound on parser cost; anything bigger reflects validation work.
func BenchmarkReadRequestHostOnly(b *testing.B) {
	payload := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := coreserver.ReadRequestBytesForTest(payload); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReadRequestRejectTE measures the cost of REJECTING a TE
// request.  Early-return paths should be cheap; if this bench is
// substantially slower than the accept path the validation loop is
// over-inspecting the header block before deciding.
func BenchmarkReadRequestRejectTE(b *testing.B) {
	payload := []byte("POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n")
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := coreserver.ReadRequestBytesForTest(payload)
		if err == nil {
			b.Fatal("expected reject")
		}
	}
}
