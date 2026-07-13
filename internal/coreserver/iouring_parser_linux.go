// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux

// Incremental HTTP/1.1 request parser used by the io_uring reactor.
//
// The bufio.Reader-based readRequest() in server.go assumes blocking
// io.Reader semantics (it calls ReadSlice('\n') which can block on
// more bytes).  The reactor cannot do that — it must consume
// already-received bytes from a fixed accumulator buffer and report
// "need more data" rather than block.
//
// parseRequestFromBuf is the buffer-mode equivalent.  It parses
// exactly one request out of buf, populating req with slices that
// alias buf.  The returned consumed count is the number of bytes the
// caller should drop from the front of buf before the next call.
//
// Wire format must stay byte-identical with readRequest().  Both
// share asciiEqualFold / trimSpace / parseInt64 (defined in
// server.go) and produce a Request the rest of the codebase can
// consume interchangeably.
package coreserver

import (
	"bytes"
	"errors"
)

// errNeedMoreData is a sentinel returned by parseRequestFromBuf when
// the buffer does not yet contain a complete header block.
var errNeedMoreData = errors.New("coreserver: io_uring parser needs more bytes")

// parseRequestFromBuf scans buf for one complete HTTP/1.1 request.
// On success it populates req with []byte slices that alias buf and
// returns the number of bytes consumed (= start of any leftover bytes
// that follow the "\r\n\r\n" terminator).  On a malformed request it
// returns 0 and a non-nil error.  When the buffer is short, it
// returns 0, errNeedMoreData.
//
// The security model is identical to readRequest in server.go: any
// classification disagreement between this parser and a front-end
// reverse proxy creates a smuggling primitive.  See that function's
// doc-comment for the policy.  Both parsers MUST stay in lock-step
// on accept/reject; the parser_differential and smuggling_corpus
// tests exercise readRequest, and the iouring_linux_test fans the
// same fixtures through parseRequestFromBuf.
func parseRequestFromBuf(buf []byte, req *Request) (int, error) {
	end := bytes.Index(buf, crlfcrlf)
	if end < 0 {
		return 0, errNeedMoreData
	}
	consumed := end + 4     // past "\r\n\r\n"
	hdr := buf[:consumed-2] // exclude the trailing "\r\n"; first
	// blank line is the terminator and readRequest expected hdr to
	// include up through (but not strictly past) the final CRLF.
	req.Raw = buf[:consumed]

	// Parse request line.
	lineEnd := bytes.Index(hdr, crlf)
	if lineEnd < 0 {
		return 0, errBadRequest
	}
	if err := parseRequestLine(hdr[:lineEnd], req); err != nil {
		return 0, err
	}
	hdr = hdr[lineEnd+2:]

	req.KeepAlive = req.HTTP11

	var (
		seenCL   bool
		seenTE   bool
		seenHost bool
	)

	for len(hdr) > 0 {
		lineEnd := bytes.Index(hdr, crlf)
		if lineEnd < 0 {
			return 0, errBadRequest
		}
		if lineEnd == 0 {
			break
		}
		line := hdr[:lineEnd]
		hdr = hdr[lineEnd+2:]

		if line[0] == ' ' || line[0] == '\t' {
			return 0, errSmugglingSuspect
		}

		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return 0, errBadRequest
		}
		name := line[:colon]
		if !validHeaderName(name) {
			return 0, errBadRequest
		}
		value := trimSpace(line[colon+1:])
		if !validHeaderValue(value) {
			return 0, errSmugglingSuspect
		}
		switch {
		case asciiEqualFold(name, hostHdr):
			if seenHost {
				return 0, errSmugglingSuspect
			}
			seenHost = true
			req.Host = value
		case asciiEqualFold(name, rangeHdr):
			req.Range = value
		case asciiEqualFold(name, authHdr):
			req.Auth = value
		case asciiEqualFold(name, clenHdr):
			if seenCL {
				return 0, errSmugglingSuspect
			}
			seenCL = true
			cl, err := parseDecimalInt64(value)
			if err != nil {
				return 0, errSmugglingSuspect
			}
			req.ContentLen = cl
		case asciiEqualFold(name, teHdr):
			seenTE = true
			if !asciiEqualFold(value, identBytes) {
				return 0, errSmugglingSuspect
			}
		case asciiEqualFold(name, connHdr):
			if asciiContainsTokenFold(value, closeBytes) {
				req.KeepAlive = false
			} else if !req.HTTP11 {
				req.KeepAlive = true
			}
		}
	}
	if seenTE && seenCL {
		return 0, errSmugglingSuspect
	}
	if !seenHost {
		return 0, errBadRequest
	}
	return consumed, nil
}
