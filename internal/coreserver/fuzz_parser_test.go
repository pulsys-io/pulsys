// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Coverage-guided differential fuzz against net/http.ReadRequest.
//
// PURPOSE
//   The hand-curated differential table and the vendored corpora
//   exercise inputs we knew to ask about.  This fuzzer asks the
//   coverage instrumentation to explore inputs we didn't think of:
//   any byte sequence that produces NEW code coverage in coreserver
//   gets retained as a corpus entry and the differential oracle
//   re-runs against it on every subsequent CI invocation.
//
// ORACLE
//   The same "no-looser-than-stdlib" invariant from
//   parser_differential_test.go: feed the same bytes to
//   coreserver.readRequest and net/http.ReadRequest.  If stdlib
//   refuses and we admit, the fuzz target reports failure -- and
//   the input is permanently committed to
//   testdata/fuzz/FuzzReadRequest/<hash> so the regression is
//   exercised by every future `go test` run, not just `-fuzz`.
//
// PANIC INVARIANT
//   Neither parser may panic on any input.  A panic in our parser
//   is a hard failure regardless of stdlib's behavior.  A panic in
//   stdlib indicates an upstream Go bug worth reporting; we treat
//   it as a recovered no-op so the fuzzer keeps exploring rather
//   than getting stuck on the same crash.
//
// HOW TO RUN
//   Quick smoke test:   go test -run FuzzReadRequest ./internal/coreserver/
//   Active fuzz cycle:  go test -fuzz=FuzzReadRequest -fuzztime=60s ./internal/coreserver/
//   Long nightly:       go test -fuzz=FuzzReadRequest -fuzztime=10m ./internal/coreserver/

package coreserver_test

import (
	"bufio"
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/pulsys-io/pulsys/internal/coreserver"
)

// seedCorpus collects every byte sequence we know to be
// security-interesting.  Adding entries here both seeds the
// fuzzer's mutation walk and ensures every entry is replayed on
// every `go test` run, so smoke-test runs catch regressions in
// known cases before the fuzzer even starts.
var seedCorpus = func() [][]byte {
	seeds := make([][]byte, 0, len(canonicalCases))
	for _, c := range canonicalCases {
		seeds = append(seeds, []byte(c.raw))
	}
	// PortSwigger and llhttp corpora are loaded from disk in the
	// dedicated tests; the fuzzer doesn't need them as named seeds
	// because mutations of the canonical set will cover the same
	// shapes within ~seconds of fuzz time.
	return seeds
}()

func FuzzReadRequest(f *testing.F) {
	for _, seed := range seedCorpus {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Guard against the parser panicking on any input.  A
		// panic is a hard test failure regardless of what stdlib
		// does: the production serveConn relies on parser
		// non-panic behavior for the connection lifecycle.
		oursReq, oursErr := safeReadRequest(t, data)
		_, stdErr := safeReadStdlib(t, data)

		// Differential oracle: stdlib rejection MUST imply our
		// rejection.  Any acceptance-mismatch in this direction
		// is a security primitive.
		if stdErr != nil && oursErr == nil {
			// Suppress one well-known FALSE POSITIVE: stdlib
			// rejects requests with `Connection: close` if the
			// body framing is missing, which we (intentionally)
			// don't enforce because we don't read bodies on the
			// warm path.  Recognize that signature and skip.
			if errors.Is(stdErr, errStdlibConnCloseFraming(stdErr)) {
				return
			}
			t.Fatalf("DIFFERENTIAL FAILURE: ours accepts what stdlib rejects\n  stdlib err: %v\n  ours parsed method=%q target=%q host=%q cl=%d\n  raw=%q",
				stdErr, oursReq.Method, oursReq.RequestURI, oursReq.Host, oursReq.ContentLen, data)
		}
	})
}

// safeReadRequest wraps coreserver.ReadRequestBytesForTest in a
// recover() so a panic becomes a test failure with the offending
// input attached, rather than terminating the fuzz worker.
func safeReadRequest(t *testing.T, data []byte) (req *coreserver.Request, err error) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC in coreserver.readRequest: %v\n  raw=%q", r, data)
		}
	}()
	req, _, err = coreserver.ReadRequestBytesForTest(data)
	return req, err
}

// safeReadStdlib wraps net/http.ReadRequest in a recover().  A
// panic here would indicate a Go stdlib bug; we don't fail the
// test on it (would block fuzz progress on every iteration) but we
// do log it so it surfaces on a triage pass.
func safeReadStdlib(t *testing.T, data []byte) (req *http.Request, err error) {
	defer func() {
		if r := recover(); r != nil {
			t.Logf("PANIC in net/http.ReadRequest (upstream issue, not fatal): %v\n  raw=%q", r, data)
			err = errors.New("stdlib panic")
		}
	}()
	req, err = http.ReadRequest(bufio.NewReader(bytes.NewReader(data)))
	return req, err
}

// errStdlibConnCloseFraming is a placeholder for a more precise
// stdlib-error matcher we may add as the fuzz surface up faces
// specific known false positives.  Currently always returns nil so
// the suppression is a no-op; documented as a hook point so the
// triage path for a real false positive is shorter.
func errStdlibConnCloseFraming(_ error) error { return nil }
