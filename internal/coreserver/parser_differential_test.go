// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Differential parser conformance test.
//
// PURPOSE
//   Every byte sequence we feed to the hand-rolled coreserver parser is
//   ALSO fed to net/http.ReadRequest.  If our parser accepts what stdlib
//   rejects, that is a smuggling primitive: a front-end (Cloudflare, nginx,
//   AWS ALB, or anything else built on a reference parser) will refuse the
//   request while our backend interprets it as valid, allowing an attacker
//   to desync the connection.  This test makes such divergences fail the
//   build.
//
// CONTRACT
//   Two independent invariants, asserted on every case:
//
//   (A) No-Looser-Than-Stdlib:
//       If stdlib rejects, ours MUST reject.  This is the security
//       invariant: stdlib's hardened parser refuses; if ours admits,
//       we are creating a smuggling primitive (the front-end /
//       reverse proxy refuses while we let it through).  Failure
//       here means a desync vector is live.
//
//   (B) We-Reject-What-We-Promised:
//       Cases listed in mustReject are inputs WE classify as
//       security-relevant regardless of what stdlib does (Go's
//       stdlib is notoriously lenient on, e.g., obsolete line
//       folding and lowercase methods, which downstream parsers
//       reject).  Ours MUST reject these.
//
//   (C) For accepted baseline cases (not in mustReject and stdlib
//       accepts), assert observable fields match: method,
//       request-target, host, content-length, keep-alive token.
//       Drift here is a parser-correctness bug for the warm-hit key
//       derivation.
//
// SCOPE
//   The hand-curated table covers the surface area we actually care
//   about: framing (CL/TE), header validity, request-line shape, and
//   protocol-version exposure.  parser_smuggling_corpus_test.go fans
//   the same oracle out over the llhttp + PortSwigger fixture corpora
//   so the catalog grows with the public state of the art.
//
// REFERENCES
//   RFC 7230 (the active HTTP/1.1 message grammar), James Kettle's
//   "HTTP Desync Attacks" research, the llhttp request/ test fixtures
//   under internal/coreserver/vendor/llhttp/test/request/.

package coreserver_test

import (
	"bufio"
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/pulsys-io/pulsys/internal/coreserver"
)

// differentialCase is one row in the conformance table.
//
// raw is the literal byte sequence as it would arrive on the wire,
// CRLF-terminated.  String literal `\r\n` escapes keep it readable.
// rationale explains why a divergence here matters; logged on
// failure so the bisect can read intent without re-deriving it.
type differentialCase struct {
	name      string
	raw       string
	rationale string
}

// canonicalCases enumerate the inputs that drive the conformance
// check.  Keep them grouped by category so a regression in (say)
// header validation produces a contiguous run of failures rather
// than a sparse pattern.
var canonicalCases = []differentialCase{
	// ----- Baseline well-formed requests (both MUST accept) ----------------
	{
		name:      "baseline_get",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n",
		rationale: "Most-basic request; if this diverges, the test harness is broken.",
	},
	{
		name:      "baseline_get_keepalive",
		raw:       "GET /a/b/c HTTP/1.1\r\nHost: example.com\r\nConnection: keep-alive\r\n\r\n",
		rationale: "HTTP/1.1 implies keep-alive; explicit header should still parse cleanly.",
	},
	{
		name:      "baseline_get_close",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n",
		rationale: "Connection: close changes keepalive but must not affect classification.",
	},
	{
		name:      "baseline_post_with_cl",
		raw:       "POST /x HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\n\r\nhello",
		rationale: "Standard POST with single Content-Length and a body.",
	},
	{
		name:      "baseline_head",
		raw:       "HEAD /resource HTTP/1.1\r\nHost: example.com\r\n\r\n",
		rationale: "HEAD with no body; trivially well-formed.",
	},
	{
		name:      "baseline_lowercase_header_names",
		raw:       "GET / HTTP/1.1\r\nhost: example.com\r\nrange: bytes=0-100\r\n\r\n",
		rationale: "Header names are case-insensitive; lowercase MUST be accepted.",
	},
	{
		name:      "baseline_with_authorization",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer pulsys_dead\r\n\r\n",
		rationale: "Authorization carries the credential the gate evaluates; must round-trip intact.",
	},

	// ----- Framing conflicts (security-critical, both SHOULD reject) -------
	{
		name:      "te_chunked_and_cl",
		raw:       "POST /x HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\nContent-Length: 5\r\n\r\n0\r\n\r\n",
		rationale: "RFC 7230 5.3.3: server MUST refuse TE+CL together; classic CL.TE / TE.CL desync.",
	},
	{
		name:      "duplicate_content_length_same_value",
		raw:       "POST /x HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\nContent-Length: 5\r\n\r\nhello",
		rationale: "Duplicate CL even with matching values is a smuggling vector (RFC 7230 3.3.2).",
	},
	{
		name:      "duplicate_content_length_diff_value",
		raw:       "POST /x HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\nContent-Length: 9\r\n\r\nhellohelp",
		rationale: "Classic CL.CL: which value does the proxy honor? Both should reject.",
	},
	{
		name:      "content_length_negative",
		raw:       "POST /x HTTP/1.1\r\nHost: example.com\r\nContent-Length: -1\r\n\r\n",
		rationale: "Negative CL is undefined; must reject (RFC 7230 3.3.2).",
	},
	{
		name:      "content_length_non_numeric",
		raw:       "POST /x HTTP/1.1\r\nHost: example.com\r\nContent-Length: abc\r\n\r\n",
		rationale: "Non-numeric CL: stdlib rejects; we must too.",
	},
	{
		name:      "content_length_with_plus_sign",
		raw:       "POST /x HTTP/1.1\r\nHost: example.com\r\nContent-Length: +5\r\n\r\nhello",
		rationale: "Leading-sign CL: stdlib rejects to avoid octal/hex parsing surprises.",
	},

	// ----- Header field validity (RFC 7230 3.2) ----------------------------
	{
		name:      "header_name_with_space",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\nX Bad : value\r\n\r\n",
		rationale: "Whitespace inside header name is illegal (tchar grammar).",
	},
	{
		name:      "header_name_with_colon_only",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\n: novalue\r\n\r\n",
		rationale: "Empty header name; both should reject.",
	},
	{
		name:      "header_value_with_bare_cr",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\nX-Bad: foo\rbar\r\n\r\n",
		rationale: "Bare CR in header value enables response splitting / smuggling.",
	},
	{
		name:      "header_value_with_nul",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\nX-Bad: foo\x00bar\r\n\r\n",
		rationale: "NUL byte in header values is rejected by RFC 7230 (and most production parsers).",
	},
	{
		name:      "header_name_with_nul",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\nX-\x00Bad: foo\r\n\r\n",
		rationale: "NUL byte in header name is rejected by tchar grammar.",
	},
	{
		name:      "obsolete_line_folding",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\nX-Wrap: first\r\n second\r\n\r\n",
		rationale: "RFC 7230 deprecates obs-fold; intermediaries SHOULD reject in request messages.",
	},

	// ----- Request line shape ---------------------------------------------
	{
		name:      "missing_request_target",
		raw:       "GET  HTTP/1.1\r\nHost: example.com\r\n\r\n",
		rationale: "Two spaces -> empty request-target; both must reject.",
	},
	{
		name:      "missing_protocol_version",
		raw:       "GET /\r\nHost: example.com\r\n\r\n",
		rationale: "Request line without version; both must reject.",
	},
	{
		name:      "garbage_protocol_version",
		raw:       "GET / LOLCAT/1.0\r\nHost: example.com\r\n\r\n",
		rationale: "Unknown protocol token; both must reject.",
	},
	{
		name:      "http_zero_nine_downgrade",
		raw:       "GET / HTTP/0.9\r\nHost: example.com\r\n\r\n",
		rationale: "HTTP/0.9 has no headers; refusing prevents downgrade-based confusion.",
	},
	{
		name:      "http_one_nine_invalid",
		raw:       "GET / HTTP/1.9\r\nHost: example.com\r\n\r\n",
		rationale: "Invalid HTTP/1.x minor version; both should reject.",
	},
	{
		name:      "http_two_zero_in_one_one_framing",
		raw:       "GET / HTTP/2.0\r\nHost: example.com\r\n\r\n",
		rationale: "HTTP/2 has different framing entirely; an HTTP/1 parser must refuse this.",
	},
	{
		name:      "lowercase_method_not_token",
		raw:       "get / HTTP/1.1\r\nHost: example.com\r\n\r\n",
		rationale: "Methods are case-sensitive tokens; lowercase MUST be rejected.",
	},
	{
		name:      "method_with_invalid_char",
		raw:       "GE\x00T / HTTP/1.1\r\nHost: example.com\r\n\r\n",
		rationale: "NUL inside method violates the token grammar.",
	},

	// ----- Absolute-URI request line (we are not a forward proxy) ---------
	{
		name:      "absolute_uri_request_target",
		raw:       "GET http://internal:8080/admin HTTP/1.1\r\nHost: example.com\r\n\r\n",
		rationale: "Absolute-form request-target is only for forward proxies; pulsys is not one.",
	},
	{
		name:      "authority_form_request_target",
		raw:       "CONNECT internal:8080 HTTP/1.1\r\nHost: example.com\r\n\r\n",
		rationale: "CONNECT authority-form is only meaningful to TLS tunneling proxies; reject.",
	},

	// ----- Multiple Host headers (RFC 7230 5.4) ---------------------------
	{
		name:      "multiple_host_headers",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\nHost: evil.com\r\n\r\n",
		rationale: "Duplicate Host enables routing-based smuggling; both should reject.",
	},
	{
		name:      "missing_host_header",
		raw:       "GET / HTTP/1.1\r\n\r\n",
		rationale: "RFC 7230 5.4: HTTP/1.1 requests MUST have a Host header.",
	},

	// ----- Pipelined / body-bearing edge cases ----------------------------
	{
		name:      "get_with_content_length_zero",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\n\r\n",
		rationale: "GET with CL:0 is unusual but well-formed; both should accept.",
	},
	{
		name:      "get_with_content_length_nonzero",
		raw:       "GET / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\n\r\nhello",
		rationale: "GET with body is permitted by RFC but rare; both should accept (semantics, not framing).",
	},
}

// mustReject lists case names that OUR parser MUST reject as a
// security policy decision.  Stdlib may be lenient on some of these
// (Go's net/http historically accepts obsolete line folding and
// missing Host headers; nginx and most production parsers refuse).
// We choose stricter than stdlib because a downstream parser that
// rejects what we accept creates the same desync vector as the
// reverse.
var mustReject = map[string]bool{
	"te_chunked_and_cl":                   true,
	"duplicate_content_length_same_value": true,
	"duplicate_content_length_diff_value": true,
	"content_length_negative":             true,
	"content_length_non_numeric":          true,
	"content_length_with_plus_sign":       true,
	"header_name_with_space":              true,
	"header_name_with_colon_only":         true,
	"header_value_with_bare_cr":           true,
	"header_value_with_nul":               true,
	"header_name_with_nul":                true,
	"obsolete_line_folding":               true,
	"missing_request_target":              true,
	"missing_protocol_version":            true,
	"garbage_protocol_version":            true,
	"http_zero_nine_downgrade":            true,
	"http_one_nine_invalid":               true,
	"http_two_zero_in_one_one_framing":    true,
	"lowercase_method_not_token":          true,
	"method_with_invalid_char":            true,
	"absolute_uri_request_target":         true,
	"authority_form_request_target":       true,
	"multiple_host_headers":               true,
	"missing_host_header":                 true,
}

// TestParserDifferentialAgainstStdlib runs each canonical case
// through both parsers and enforces the three invariants documented
// in the file header.
func TestParserDifferentialAgainstStdlib(t *testing.T) {
	for _, c := range canonicalCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			oursReq, _, oursErr := coreserver.ReadRequestBytesForTest([]byte(c.raw))
			stdReq, stdErr := http.ReadRequest(bufio.NewReader(bytes.NewReader([]byte(c.raw))))

			oursAccept := oursErr == nil
			stdAccept := stdErr == nil

			// Invariant (A): No-Looser-Than-Stdlib.  If stdlib refuses
			// and we admit, a downstream parser desync is live.
			if !stdAccept && oursAccept {
				t.Fatalf("SECURITY (looser-than-stdlib): coreserver accepted a request stdlib rejects\n  case: %s\n  rationale: %s\n  ours parsed: %s\n  stdlib err: %v\n  raw=%q",
					c.name, c.rationale, summary(oursReq), stdErr, c.raw)
			}

			// Invariant (B): We-Reject-What-We-Promised.
			if mustReject[c.name] {
				if oursAccept {
					t.Fatalf("POLICY: coreserver MUST reject this input regardless of stdlib's leniency\n  case: %s\n  rationale: %s\n  ours parsed: %s\n  stdlib accept=%v stdlib err=%v\n  raw=%q",
						c.name, c.rationale, summary(oursReq), stdAccept, stdErr, c.raw)
				}
				return
			}

			// Invariant (C): for non-must-reject cases that stdlib
			// accepts, ours should also accept and the observable
			// fields should match.  If ours rejects here we don't
			// have a security failure (stricter is safer) but we DO
			// want a visible log so future bisects know which
			// baseline we hardened.
			if !stdAccept {
				return // both rejected; nothing more to check
			}
			if !oursAccept {
				t.Logf("NOTE: coreserver stricter than stdlib on baseline case\n  case: %s\n  rationale: %s\n  ours err: %v\n  stdlib parsed method=%s target=%s host=%s\n  raw=%q",
					c.name, c.rationale, oursErr, stdReq.Method, stdReq.RequestURI, stdReq.Host, c.raw)
				return
			}

			// Both accepted; check observable fields agree.
			diffFields(t, c, oursReq, stdReq)
		})
	}
}

func diffFields(t *testing.T, c differentialCase, ours *coreserver.Request, std *http.Request) {
	t.Helper()
	if string(ours.Method) != std.Method {
		t.Errorf("method mismatch: ours=%q std=%q", ours.Method, std.Method)
	}
	if string(ours.RequestURI) != std.RequestURI {
		t.Errorf("request-target mismatch: ours=%q std=%q", ours.RequestURI, std.RequestURI)
	}
	// Host: ours pulls it from the header; std exposes it on .Host.
	if string(ours.Host) != std.Host {
		t.Errorf("host mismatch: ours=%q std=%q", ours.Host, std.Host)
	}
	if ours.ContentLen != std.ContentLength {
		t.Errorf("content-length mismatch: ours=%d std=%d", ours.ContentLen, std.ContentLength)
	}
	// Keep-alive: stdlib's r.Close == true means "close after response";
	// ours has KeepAlive (inverted).
	stdKeep := !std.Close
	if ours.KeepAlive != stdKeep {
		t.Errorf("keepalive mismatch: ours=%v std=%v", ours.KeepAlive, stdKeep)
	}
}

// summary returns a compact representation of a Request useful for
// failure messages.  Avoids printing the (potentially binary) Raw
// field which would clutter the test log.
func summary(r *coreserver.Request) string {
	if r == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString("Request{method=")
	b.Write(r.Method)
	b.WriteString(" target=")
	b.Write(r.RequestURI)
	b.WriteString(" host=")
	b.Write(r.Host)
	b.WriteString("}")
	return b.String()
}

// TestParserClassifications_AreStable is a guard that the table is
// internally consistent: every entry in mustReject corresponds to a
// real case in canonicalCases.  Catches typos in the map keys.
func TestParserClassifications_AreStable(t *testing.T) {
	names := make(map[string]bool, len(canonicalCases))
	for _, c := range canonicalCases {
		if names[c.name] {
			t.Fatalf("duplicate case name: %s", c.name)
		}
		names[c.name] = true
	}
	for k := range mustReject {
		if !names[k] {
			t.Errorf("mustReject references unknown case: %s", k)
		}
	}
}

// Below: keep one tiny canary that exercises the sentinel-error
// exports, so any rename in coreserver/server.go that drops the
// re-exports surfaces here rather than as an opaque test compile
// failure elsewhere.
func TestParserSentinelsExposed(t *testing.T) {
	if coreserver.ErrBadRequestForTest == nil {
		t.Fatal("ErrBadRequestForTest not exported")
	}
	if coreserver.ErrHeaderTooLargeForTest == nil {
		t.Fatal("ErrHeaderTooLargeForTest not exported")
	}
	if !errors.Is(coreserver.ErrBadRequestForTest, coreserver.ErrBadRequestForTest) {
		t.Fatal("sentinel does not match itself; sanity guard failed")
	}
}
