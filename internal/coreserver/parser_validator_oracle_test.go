// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Validator oracle conformance test.
//
// PURPOSE
//   Where parser_differential_test.go uses net/http.ReadRequest as a
//   end-to-end oracle (does THIS request, in total, get accepted?),
//   this test uses golang.org/x/net/http/httpguts as a FIELD-LEVEL
//   oracle (is THIS header name / value / method legal per RFC 7230?).
//   The httpguts package is the same one Go's net/http server uses
//   internally; it is the closest thing to a pure-functional RFC 7230
//   validator we can import.
//
// CONTRACT
//   For every header name / value / method byte sequence we feed
//   through coreserver's accept logic, if httpguts says "invalid"
//   then coreserver MUST also reject (a complete request containing
//   that token).  The converse is allowed: coreserver may reject
//   field values httpguts accepts, which makes us stricter than
//   stdlib and therefore safer.
//
// METHOD
//   Property-based: deterministic-seed random byte strings of varying
//   shapes (printable, control-heavy, NUL-heavy, high-bit) wrapped
//   into a minimal valid request frame.  Sample size large enough
//   (5000 iterations) to exercise every byte position with high
//   probability while keeping the test sub-second.
//
// FAILURE MODE
//   When a divergence is found the test logs the exact byte sequence
//   and prints both classifications.  Add the offending case to
//   parser_differential_test.go's canonicalCases as a regression
//   anchor so it's exercised on every CI run, not just probabilistically.

package coreserver_test

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/pulsys-io/pulsys/internal/coreserver"
	"golang.org/x/net/http/httpguts"
)

const validatorOracleSeed = 0x736d7567676c69 // "smuggli"

// TestParser_HeaderName_HttpgutsOracle generates synthetic header
// names and asserts: if httpguts rejects the name, a request that
// CONTAINS that header is also rejected by our parser.
//
// Test-construction caveat: we skip candidate names containing ':',
// '\r', or '\n' because those bytes are also the delimiters our
// parser splits on when locating the name/value boundary and the
// header line boundary respectively.  Embedded ':' would make our
// parser interpret a different substring as the "name" than the
// httpguts oracle does, producing false-positive divergence reports.
// The remaining 4/5 of iterations still exercise the same code path
// (validHeaderName); the differential / smuggling-corpus tests pin
// the explicit colon-in-name behavior.
func TestParser_HeaderName_HttpgutsOracle(t *testing.T) {
	t.Parallel()
	const iterations = 5000
	r := rand.New(rand.NewSource(validatorOracleSeed))

	for i := 0; i < iterations; i++ {
		name := randomToken(r, 1, 32, bytePalette(r))
		if strings.ContainsAny(name, ":\r\n") {
			continue // delimiter overlap; see test doc-comment.
		}
		gutsOK := httpguts.ValidHeaderFieldName(name)
		if gutsOK {
			continue
		}
		raw := fmt.Sprintf(
			"GET / HTTP/1.1\r\nHost: example.com\r\n%s: value\r\n\r\n",
			name,
		)
		_, _, err := coreserver.ReadRequestBytesForTest([]byte(raw))
		if err == nil {
			t.Fatalf("VALIDATOR DRIFT: coreserver accepted header name httpguts rejects\n  iter=%d\n  name=%q (len=%d)\n  raw=%q",
				i, name, len(name), raw)
		}
	}
}

// TestParser_HeaderValue_HttpgutsOracle generates synthetic header
// values and asserts the same invariant for the value position.
// We skip candidate values containing '\r' or '\n' because those
// bytes are the line-end delimiters our parser splits on; a value
// that contains them is structurally a multi-header sequence and
// the comparison with httpguts.ValidHeaderFieldValue would be
// apples-to-oranges.  The bare-CR / bare-LF rejection is pinned
// directly in parser_differential_test.go.
func TestParser_HeaderValue_HttpgutsOracle(t *testing.T) {
	t.Parallel()
	const iterations = 5000
	r := rand.New(rand.NewSource(validatorOracleSeed ^ 1))

	for i := 0; i < iterations; i++ {
		value := randomToken(r, 1, 64, bytePalette(r))
		if strings.ContainsAny(value, "\r\n") {
			continue
		}
		gutsOK := httpguts.ValidHeaderFieldValue(value)
		if gutsOK {
			continue
		}
		raw := fmt.Sprintf(
			"GET / HTTP/1.1\r\nHost: example.com\r\nX-Test: %s\r\n\r\n",
			value,
		)
		_, _, err := coreserver.ReadRequestBytesForTest([]byte(raw))
		if err == nil {
			t.Fatalf("VALIDATOR DRIFT: coreserver accepted header value httpguts rejects\n  iter=%d\n  value=%q (len=%d)\n  raw=%q",
				i, value, len(value), raw)
		}
	}
}

// TestParser_Method_TokenOracle exercises the method token using
// httpguts.IsTokenRune as the per-byte validator.  RFC 7230 §3.1.1
// defines method as a token, so a method whose ANY byte is
// non-tchar must be rejected.  We are stricter still (uppercase
// ASCII only), so the assertion direction here is: if any byte
// fails the tchar test, ours MUST reject.  Cases where the token
// grammar accepts but we reject (e.g. "get") are pinned in
// parser_differential_test.go.
func TestParser_Method_TokenOracle(t *testing.T) {
	t.Parallel()
	const iterations = 5000
	r := rand.New(rand.NewSource(validatorOracleSeed ^ 2))

	for i := 0; i < iterations; i++ {
		method := randomToken(r, 1, 12, bytePalette(r))
		if isTokenString(method) {
			continue // not asserting on this side
		}
		raw := fmt.Sprintf("%s / HTTP/1.1\r\nHost: example.com\r\n\r\n", method)
		_, _, err := coreserver.ReadRequestBytesForTest([]byte(raw))
		if err == nil {
			t.Fatalf("VALIDATOR DRIFT: coreserver accepted method with non-tchar bytes\n  iter=%d\n  method=%q (len=%d)\n  raw=%q",
				i, method, len(method), raw)
		}
	}
}

// isTokenString reports whether s is a non-empty RFC 7230 token,
// matching httpguts.IsTokenRune byte-for-byte for the ASCII subset
// that valid HTTP methods can live in.
func isTokenString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !httpguts.IsTokenRune(r) {
			return false
		}
	}
	return true
}

// randomToken returns a byte slice of length in [minLen, maxLen]
// drawn from palette.  Strings produced this way regularly include
// CR, LF, NUL, ':', whitespace, and high-bit bytes -- exactly the
// classes that exercise the validator's boundary cases.  Caller is
// responsible for skipping bytes that would re-frame the test
// request itself (delimiters); see the test bodies for details.
func randomToken(r *rand.Rand, minLen, maxLen int, palette string) string {
	n := minLen + r.Intn(maxLen-minLen+1)
	b := strings.Builder{}
	b.Grow(n)
	for i := 0; i < n; i++ {
		b.WriteByte(palette[r.Intn(len(palette))])
	}
	return b.String()
}

// bytePalette returns one of several byte palettes selected at
// random.  Each iteration draws from one palette so the test fairly
// exercises both "mostly valid" (printable) and "mostly hostile"
// (control-heavy) inputs.
func bytePalette(r *rand.Rand) string {
	switch r.Intn(4) {
	case 0:
		// Printable ASCII + the smuggling-critical specials.
		return "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_:.,!?#$%@*+= \t\r\n"
	case 1:
		// Heavy on control bytes.
		return "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f abc\x7f"
	case 2:
		// High-bit / obs-text territory.
		return "\x80\x81\xa0\xc3\xb1\xff abc"
	default:
		// Tchar grammar (RFC 7230 3.2.6) plus a few control bytes
		// sprinkled in -- targets the "almost valid header name"
		// boundary case.
		return "abcXYZ0123!#$%&'*+-.^_`|~\x00\x09\x20:"
	}
}
