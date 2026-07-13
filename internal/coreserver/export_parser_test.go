// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Test-only exports for the HTTP/1.1 parser.
//
// The parser is a private package symbol because it has zero
// production callers outside coreserver.serveConn / the io_uring
// reactor.  But the security test corpus needs to feed handcrafted
// byte sequences directly at it, so we expose a stable shim that is
// only compiled into _test binaries.
//
// Naming: keep ReadRequestForTest distinct from the upstream
// readRequest so any accidental production call shows up immediately
// in code review.

package coreserver

import (
	"bufio"
	"bytes"
)

// ReadRequestForTest parses one HTTP/1.1 request from br using the
// production readRequest path.  Returns the populated Request on
// success or an error on parse failure / short read.  The returned
// Request aliases an internally-allocated scratch buffer that lives
// for the lifetime of the value; tests must copy out any fields they
// keep across calls.
func ReadRequestForTest(br *bufio.Reader) (*Request, error) {
	scratch := make([]byte, headerScratchSize)
	req := &Request{}
	if err := readRequest(br, scratch, req); err != nil {
		return nil, err
	}
	return req, nil
}

// ReadRequestBytesForTest is a convenience wrapper that wraps b in a
// bufio.Reader before calling ReadRequestForTest.  Returns the parse
// result and the number of bytes the parser left UNREAD in the input
// (useful for asserting the parser stops at the byte immediately
// following the end-of-headers marker rather than over-consuming).
func ReadRequestBytesForTest(b []byte) (*Request, int, error) {
	src := bytes.NewReader(b)
	br := bufio.NewReader(src)
	req, err := ReadRequestForTest(br)
	if err != nil {
		return nil, 0, err
	}
	leftover := br.Buffered() + src.Len()
	return req, leftover, nil
}

// HeaderScratchSizeForTest exposes the per-request scratch budget so
// tests can construct boundary payloads (header line > headerScratchSize).
const HeaderScratchSizeForTest = headerScratchSize

// ErrHeaderTooLargeForTest exposes the sentinel so tests can do
// errors.Is comparisons without re-defining the message.
var ErrHeaderTooLargeForTest = errHeaderTooLarge

// ErrBadRequestForTest exposes the sentinel so tests can do
// errors.Is comparisons without re-defining the message.
var ErrBadRequestForTest = errBadRequest
