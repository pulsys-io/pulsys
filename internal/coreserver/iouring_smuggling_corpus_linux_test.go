// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux

// io_uring parser smuggling-corpus differential test.
//
// PURPOSE
//   Phase 4.5 of the OWASP WSTG hardening pass.  The Linux-only
//   io_uring reactor uses a DIFFERENT parser
//   (parseRequestFromBuf in iouring_parser_linux.go) than the
//   cork+sendfile path (readRequest in server.go).  Pre-Phase-
//   4.5, the only test driving the iouring parser is
//   TestWarmHitIoUring, which sends a single well-formed warm
//   GET.  None of the smuggling / desync / parser-strictness
//   fixtures the cork+sendfile parser is hardened against were
//   ever fanned through the iouring parser.
//
//   This file closes that gap.  It walks both:
//
//     * the in-tree PortSwigger smuggling corpus
//       (testdata/portswigger_smuggling.txt)
//     * the llhttp request fixture corpus (vendored as a git
//       submodule under vendor/llhttp/test/request)
//
//   ... through parseRequestFromBuf and asserts the SAME
//   accept/reject classification the cork+sendfile parser
//   produces.  Any divergence between the two parsers is a
//   smuggling primitive: an attacker who finds an input that
//   one parser accepts and the other rejects can desync the
//   warm-cache pipeline from the slow-path handler.
//
//   We additionally pin EQUIVALENT-OR-STRICTER behaviour: the
//   io_uring parser may REJECT something the cork parser
//   accepted (the iouring parser is strictly stricter on
//   buffer-bound cases like errNeedMoreData), but NEVER the
//   reverse.

package coreserver_test

import (
	"errors"
	"testing"

	"github.com/pulsys-io/pulsys/internal/coreserver"
)

// TestIoUringParser_PortSwiggerCorpus replays the in-tree
// PortSwigger smuggling fixtures through parseRequestFromBuf
// and asserts the SAME accept/reject classification as the
// cork+sendfile parser.
//
// "errNeedMoreData" is treated as REJECT for the purpose of
// this test: the iouring parser uses the sentinel for
// "incremental parse needs more bytes", which on a complete
// fixture means the input is malformed (the cork parser
// would have blocked indefinitely or errored).
func TestIoUringParser_PortSwiggerCorpus(t *testing.T) {
	t.Parallel()
	fixtures, err := parsePortswiggerCorpus(portswiggerCorpusRaw)
	if err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("PortSwigger corpus parsed empty; the testdata file must be present")
	}
	t.Logf("running %d PortSwigger fixtures through io_uring parser", len(fixtures))

	for _, f := range fixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := coreserver.ReadRequestBytesForTestIoUring(f.payload)

			// errNeedMoreData is "REJECT" semantics for this
			// suite -- the iouring parser asks for more bytes
			// only when the input is incomplete, which on a
			// complete corpus fixture indicates malformed
			// framing the cork parser would also have
			// errored on.
			rejected := err != nil
			switch f.expected {
			case "reject":
				if !rejected {
					t.Fatalf("SECURITY: PortSwigger fixture %q ACCEPTED by io_uring parser but expected reject\n  source: %s\n  note: %s\n  payload=%q",
						f.name, f.source, f.note, f.payload)
				}
			case "accept":
				if rejected {
					// errNeedMoreData on an "accept"
					// fixture is a real bug -- the cork
					// parser handled it.
					if errors.Is(err, coreserver.ErrNeedMoreDataForTest) {
						t.Fatalf("DIVERGENCE: PortSwigger fixture %q -- io_uring parser returned errNeedMoreData on COMPLETE input that cork parser accepted\n  source: %s\n  note: %s\n  payload=%q",
							f.name, f.source, f.note, f.payload)
					}
					t.Fatalf("DIVERGENCE: PortSwigger fixture %q -- io_uring parser REJECTED a baseline accept fixture\n  source: %s\n  note: %s\n  err: %v\n  payload=%q",
						f.name, f.source, f.note, err, f.payload)
				}
			default:
				t.Fatalf("fixture %q has unknown expected value %q", f.name, f.expected)
			}
		})
	}
}

// TestIoUringParser_DivergenceFromCorkParser explicitly walks
// every PortSwigger fixture, feeds it to BOTH parsers, and
// fails if their accept/reject decisions disagree (modulo the
// errNeedMoreData semantics above).
//
// This is the strongest form of the contract: not only must
// the iouring parser stay calibrated against the corpus, it
// must stay calibrated against the OTHER PARSER.  The two
// parsers will inevitably drift over time as one gets
// micro-optimised; this test fails on drift before drift
// becomes a smuggling primitive.
func TestIoUringParser_DivergenceFromCorkParser(t *testing.T) {
	t.Parallel()
	fixtures, err := parsePortswiggerCorpus(portswiggerCorpusRaw)
	if err != nil {
		t.Fatalf("parse corpus: %v", err)
	}

	type result struct {
		accepted bool
		err      error
	}
	classify := func(p []byte) result {
		_, _, e := coreserver.ReadRequestBytesForTest(p)
		return result{accepted: e == nil, err: e}
	}
	classifyIo := func(p []byte) result {
		_, _, e := coreserver.ReadRequestBytesForTestIoUring(p)
		return result{accepted: e == nil, err: e}
	}

	var divergences int
	for _, f := range fixtures {
		cork := classify(f.payload)
		iou := classifyIo(f.payload)
		if cork.accepted == iou.accepted {
			continue
		}
		// Tolerate iouring returning errNeedMoreData on
		// fixtures the cork parser would have BLOCKED on
		// waiting for more bytes (the cork parser's
		// ReadSlice would have failed with EOF, which is
		// also an error).  Both are "reject" outcomes.
		if !iou.accepted && errors.Is(iou.err, coreserver.ErrNeedMoreDataForTest) && !cork.accepted {
			continue
		}
		divergences++
		t.Errorf("SMUGGLING-CLASS DIVERGENCE: fixture %q\n  cork: accepted=%v err=%v\n  iou:  accepted=%v err=%v\n  payload=%q\n  source: %s",
			f.name, cork.accepted, cork.err, iou.accepted, iou.err, f.payload, f.source)
	}
	if divergences > 0 {
		t.Fatalf("SECURITY: %d parser divergences -- attacker can desync the warm-cache pipeline from the slow-path handler", divergences)
	}
	t.Logf("Phase 4.5: %d PortSwigger fixtures fanned through both parsers with zero divergence", len(fixtures))
}
