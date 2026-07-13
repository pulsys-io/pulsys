// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux

// Test-only Linux export for the io_uring-mode parser.
//
// PURPOSE
//   internal/coreserver has TWO HTTP/1.1 parsers that MUST stay
//   byte-identical in their accept/reject decisions:
//
//     1. readRequest(br *bufio.Reader, ...)   -- exported as
//        ReadRequestForTest in export_parser_test.go.  Used by
//        the cork+sendfile path (server.go).  Cross-platform.
//
//     2. parseRequestFromBuf(buf []byte, ...) -- the io_uring
//        reactor's incremental, non-blocking parser.  Linux-
//        only (build-tagged via iouring_parser_linux.go).  Has
//        ZERO direct tests pre-Phase-4.5; the source comment
//        even acknowledges this gap and points future work at
//        "fan the smuggling corpus through it".
//
//   This file is Phase 4.5's plug for that gap.  By exposing a
//   ReadRequestBytesForTestIoUring wrapper with the SAME shape
//   as ReadRequestBytesForTest, the smuggling/parser corpora
//   in parser_smuggling_corpus_test.go can be invoked against
//   BOTH parsers with no test-side changes.  The corresponding
//   Linux-only test file (iouring_smuggling_corpus_linux_test.go)
//   walks the same fixtures through this entry point.

package coreserver

// ReadRequestBytesForTestIoUring parses one HTTP/1.1 request
// from b via the io_uring-mode parseRequestFromBuf and returns
// the populated *Request, the count of unconsumed leftover
// bytes (= len(b) - consumed), and any parse error.
//
// Linux-only because parseRequestFromBuf only exists on Linux.
func ReadRequestBytesForTestIoUring(b []byte) (*Request, int, error) {
	req := &Request{}
	n, err := parseRequestFromBuf(b, req)
	if err != nil {
		return nil, len(b), err
	}
	leftover := len(b) - n
	return req, leftover, nil
}

// ErrNeedMoreDataForTest exposes the io_uring parser's "short
// buffer" sentinel so corpus tests can recognise (and skip)
// the cases where a fixture is intentionally truncated to test
// the incremental-parse code path rather than a smuggling
// classification.
var ErrNeedMoreDataForTest = errNeedMoreData
