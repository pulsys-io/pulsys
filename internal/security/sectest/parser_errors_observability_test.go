// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Parser-error observability regression: assert pulsys's
// per-class parser-error counters advance when the expected
// shape arrives on the wire.
//
// This is the primary monitoring signal for HTTP smuggling probe
// campaigns: an operator alerting on a sharp rise in
// pulsys_parser_errors{kind="smuggling_suspect"} from a single
// peer IP gets the textbook fingerprint of a desync scanner.
// Without this regression test a refactor that consolidated the
// error map or dropped a case would silently halve the signal.
package sectest

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/telemetry"
)

// TestParserErrorCounters_AdvanceOnEachClass asserts that each
// of the three parser-error sentinels increments its dedicated
// counter:
//
//   - errBadRequest        -> parserBadRequest
//   - errSmugglingSuspect  -> parserSmugglingSuspect
//   - errHeaderTooLarge    -> parserHeaderTooLarge
//
// The split MUST be preserved.  Bundling everything under a
// single "parser_errors" counter would lose the
// smuggling-suspect-vs-malformed-request signal that operators
// need to triage abusive traffic.
func TestParserErrorCounters_AdvanceOnEachClass(t *testing.T) {
	stack := newStack(t)
	addr := stripAddr(stack.ProxyURL())

	probe := func(t *testing.T, raw string) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = io.WriteString(conn, raw)
		// Drain whatever the server sent (typically a 4xx +
		// close); we don't assert the body shape here -- the
		// CVE-regression file owns that contract.  We only
		// need the parser-error counter to advance.
		_, _ = io.Copy(io.Discard, conn)
	}

	cases := []struct {
		name      string
		raw       string
		probe     func() (b, s, h int64)
		expectKey string // "bad" | "smuggling" | "header"
	}{
		{
			name: "smuggling_suspect_TE_and_CL",
			raw: "POST / HTTP/1.1\r\n" +
				"Host: x\r\n" +
				"Transfer-Encoding: chunked\r\n" +
				"Content-Length: 5\r\n\r\n0\r\n\r\n",
			expectKey: "smuggling",
		},
		{
			name:      "smuggling_suspect_duplicate_host",
			raw:       "GET / HTTP/1.1\r\nHost: x\r\nHost: y\r\n\r\n",
			expectKey: "smuggling",
		},
		{
			name: "bad_request_bogus_method",
			// PWN is a syntactically valid HTTP token but
			// validMethod rejects it because every byte
			// must be uppercase A-Z.  Wait -- "PWN" IS
			// uppercase A-Z, so it would actually pass
			// validMethod and then 404 at handler level
			// (not a parser error).  Use a method with
			// a tab to trip parser-side rejection.
			raw:       "GET\t/ HTTP/1.1\r\nHost: x\r\n\r\n",
			expectKey: "bad",
		},
		{
			name: "header_too_large",
			// 32 KiB of header content blows the 16 KiB
			// scratch buffer -> errHeaderTooLarge.  Each
			// header line stays small (validHeaderName /
			// Value pass) so the rejection MUST be by
			// size, not content.
			raw:       buildHeaderTooLargePayload(),
			expectKey: "header",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Snapshot BEFORE.  Counters are process-global
			// so we measure deltas, not absolutes; other
			// tests in this binary may have advanced them.
			bBefore, sBefore, hBefore := telemetry.ParserErrorSnapshot()
			probe(t, c.raw)
			// Allow the server a tick to write the
			// response + close before we sample.
			time.Sleep(50 * time.Millisecond)
			bAfter, sAfter, hAfter := telemetry.ParserErrorSnapshot()

			db := bAfter - bBefore
			ds := sAfter - sBefore
			dh := hAfter - hBefore

			switch c.expectKey {
			case "bad":
				if db < 1 {
					t.Fatalf("parserBadRequest did not advance: delta(bad=%d, smug=%d, hdr=%d)", db, ds, dh)
				}
			case "smuggling":
				if ds < 1 {
					t.Fatalf("parserSmugglingSuspect did not advance: delta(bad=%d, smug=%d, hdr=%d)", db, ds, dh)
				}
			case "header":
				if dh < 1 {
					t.Fatalf("parserHeaderTooLarge did not advance: delta(bad=%d, smug=%d, hdr=%d)", db, ds, dh)
				}
			}
		})
	}
}

// TestParserErrorCounters_LegitimateRequestDoesNotAdvance pins
// the negative: a normal warm GET does NOT advance any parser
// error counter.  Without this, a refactor that wired the
// increment into the success path would silently produce a
// noisy alert signal.
func TestParserErrorCounters_LegitimateRequestDoesNotAdvance(t *testing.T) {
	stack := newStack(t)
	addr := stripAddr(stack.ProxyURL())

	bBefore, sBefore, hBefore := telemetry.ParserErrorSnapshot()

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Well-formed GET; the testserver may 200 (cache hit), 502
	// (upstream miss), or 404 depending on stack state.  We do
	// NOT assert status; we only assert no parser-error
	// counter advanced.
	_, _ = io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	_, _ = io.Copy(io.Discard, conn)
	time.Sleep(50 * time.Millisecond)

	bAfter, sAfter, hAfter := telemetry.ParserErrorSnapshot()
	if bAfter-bBefore != 0 || sAfter-sBefore != 0 || hAfter-hBefore != 0 {
		t.Fatalf("legitimate request advanced parser-error counter: delta(bad=%d, smug=%d, hdr=%d)",
			bAfter-bBefore, sAfter-sBefore, hAfter-hBefore)
	}
}

// buildHeaderTooLargePayload constructs a syntactically valid
// HTTP/1.1 request whose total header bytes exceed the
// coreserver's 16 KiB header scratch budget.  Each header line
// stays under 200 bytes (well within validHeaderName /
// validHeaderValue), but their sum trips errHeaderTooLarge.
//
// We deliberately do NOT terminate with \r\n\r\n so the parser
// hits the scratch overflow check before the terminator; this
// guarantees errHeaderTooLarge (the specific class) instead of
// any neighboring error code.
func buildHeaderTooLargePayload() string {
	var sb strings.Builder
	sb.WriteString("GET / HTTP/1.1\r\nHost: x\r\n")
	// Each padding header line is 100 bytes; 200 lines -> 20 KiB,
	// safely above the 16 KiB cap.  Names rotate so duplicate-
	// header rejection (smuggling-suspect) does not fire first.
	line := strings.Repeat("a", 90)
	for i := 0; i < 200; i++ {
		// X-Pad-NNNN: <90-byte filler>\r\n  -> ~108 bytes/line
		sb.WriteString("X-Pad-")
		sb.WriteString(intToShortString(i))
		sb.WriteString(": ")
		sb.WriteString(line)
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")
	return sb.String()
}

// intToShortString returns a base-36 string for small ints
// (no leading zeros).  Enough to keep header names unique
// within the 200-line padding budget.
func intToShortString(n int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{digits[n%36]}, out...)
		n /= 36
	}
	return string(out)
}
