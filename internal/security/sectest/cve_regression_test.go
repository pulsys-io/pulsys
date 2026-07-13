// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// CVE-comparable regression pins.
//
// Each test in this file pins one invariant against a class of
// public CVE / disclosure from architecturally similar HTTP servers
// (Go net/http, fasthttp, OpenResty lua-nginx-module, nginx).  The
// goal is to make a future "small refactor" or "performance
// optimization" trip CI if it silently regresses on a known-bad
// shape, rather than discover the regression in a customer audit.
//
// The full CVE catalog and exploit-chain reasoning lives in
// docs/security.md.  Each test references the
// CVE / advisory it pins so a reader can locate the upstream
// analysis from the test itself.
//
// All tests run against the real coreserver via testserver.Stack,
// not against readRequest in isolation -- because the *interaction*
// of parser + connection lifecycle is what makes these shapes
// dangerous.
package sectest

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// Group 1: HEAD-with-body smuggling guard (CVE-2024-33452 class).
//
// OpenResty lua-nginx-module v0.10.26 and earlier treated a HEAD
// request's body as a *pipelined* follow-on request, allowing a
// smuggled inner request to bypass the front CDN/WAF.  Our warm
// path only accepts GET (server.go:tryServeWarm); HEAD falls
// through to the stdlib fallback, which drains correctly.  This
// test PINS that contract: if a future optimization adds HEAD to
// the warm path without draining, this test fails immediately.
// ---------------------------------------------------------------------

// TestCVE_2024_33452_HEAD_With_Body_DoesNotSmuggle sends one HEAD
// request with a Content-Length-declared body whose content is a
// well-formed `GET /admin` line.  A correct server consumes (or
// closes) such that the smuggled GET is NOT served on the same
// connection.  The acceptable outcomes are:
//
//  1. The server responds to the HEAD, drains the body, then waits
//     for a true next request -- and our second-request probe
//     receives a fresh response (not the admin response).
//  2. The server responds to the HEAD and closes the connection.
//
// The UNACCEPTABLE outcome is: the server processes the HEAD,
// returns to the parser, reads the body bytes as a new request
// line, and serves /admin to the smuggled request -- whose
// response would be delivered to whoever next reads on this
// socket (response queue poisoning).
func TestCVE_2024_33452_HEAD_With_Body_DoesNotSmuggle(t *testing.T) {
	t.Parallel()
	stack := newStack(t)
	addr := stripAddr(stack.ProxyURL())

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// HEAD with a 38-byte body containing a smuggled GET.  An
	// LB that respects CL forwards exactly these bytes; a backend
	// that misparses HEAD bodies will execute the inner GET.
	smuggled := "GET /admin/api/v1/tokens HTTP/1.1\r\nHost: pulsys.test\r\n\r\n"
	clVal := len(smuggled)
	payload := fmt.Sprintf(
		"HEAD /acme/widget/resolve/main/config.json HTTP/1.1\r\n"+
			"Host: pulsys.test\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: keep-alive\r\n"+
			"\r\n"+
			"%s",
		clVal, smuggled,
	)
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatalf("write smuggled: %v", err)
	}

	// First response: read until headers complete.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodHead})
	if err != nil {
		// A clean close on the HEAD is also an acceptable
		// outcome (server saw odd HEAD+body and closed).
		// EOF here means the server did NOT execute the
		// smuggled GET on this conn, which is the contract.
		if isCleanCloseErr(err) {
			return
		}
		t.Fatalf("first response: %v", err)
	}
	defer resp.Body.Close()

	// HEAD response.  We accept any status from the upstream
	// handler (200 if cached, 502/504 if upstream miss in mock
	// mode); the only forbidden outcome is the body of the
	// HEAD response containing the smuggled-GET's payload --
	// which would mean the server fused the two into one.
	body, _ := io.ReadAll(resp.Body)
	if len(body) > 0 && bytes.Contains(body, []byte("\"id\":")) && bytes.Contains(body, []byte("token")) {
		t.Fatalf("HEAD response body contains apparent token list -- smuggled GET /admin/api/v1/tokens may have been served: %s", body)
	}

	// Now try to read a SECOND response on the same socket.  If
	// the smuggled GET was executed on this conn the response
	// would arrive here (queue poisoning).  We allow:
	//   - clean EOF / timeout (server closed after HEAD -- fine)
	//   - any 4xx (server saw the body bytes as a request and
	//     refused -- also fine; the smuggled URL was not served)
	// We forbid: a 200/206 for /admin/api/v1/tokens.
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	resp2, err := http.ReadResponse(br, nil)
	if err != nil {
		// EOF / timeout -- ideal: server did not honor the
		// smuggled request.
		return
	}
	defer resp2.Body.Close()
	if resp2.StatusCode >= 200 && resp2.StatusCode < 300 {
		body2, _ := io.ReadAll(resp2.Body)
		t.Fatalf("second response on same conn was %d (smuggling primitive); body=%q", resp2.StatusCode, body2)
	}
}

// ---------------------------------------------------------------------
// Group 2: GET-with-Content-Length warm-path body draining
// (smuggling primitive analog).
//
// A GET request CAN have a Content-Length body (RFC 7230 permits,
// though strongly discouraged).  Our warm path serves the GET
// without consuming any body bytes, then loops to read the next
// request.  If body bytes look like a request line, they MUST
// either parse as garbage and the connection MUST close -- never
// be executed as a smuggled request.
// ---------------------------------------------------------------------

// TestSmuggling_GET_With_CL_Body_DoesNotExecute pins: when a GET
// arrives with Content-Length and follow-on body bytes that look
// like a smuggled "GET /admin", the server MUST not execute the
// smuggled GET.  Acceptable: the server closes the conn after the
// first response, OR the parser errors on the body bytes and
// closes.  Unacceptable: a 2xx response for /admin on this socket.
func TestSmuggling_GET_With_CL_Body_DoesNotExecute(t *testing.T) {
	t.Parallel()
	stack := newStack(t)
	addr := stripAddr(stack.ProxyURL())

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	smuggled := "GET /admin/api/v1/tokens HTTP/1.1\r\nHost: pulsys.test\r\n\r\n"
	payload := fmt.Sprintf(
		"GET /acme/widget/resolve/main/config.json HTTP/1.1\r\n"+
			"Host: pulsys.test\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: keep-alive\r\n"+
			"\r\n"+
			"%s",
		len(smuggled), smuggled,
	)
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	br := bufio.NewReader(conn)
	// First response: ignore status; we only care that no SECOND
	// 2xx response appears on this conn for the admin URL.
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		if isCleanCloseErr(err) {
			return
		}
		t.Fatalf("first response: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	resp2, err := http.ReadResponse(br, nil)
	if err != nil {
		// EOF / timeout / parser-error close.  Ideal outcome.
		return
	}
	defer resp2.Body.Close()
	if resp2.StatusCode >= 200 && resp2.StatusCode < 300 {
		body2, _ := io.ReadAll(resp2.Body)
		t.Fatalf("smuggled GET /admin/api/v1/tokens was served on the same conn: status=%d body=%q", resp2.StatusCode, body2)
	}
}

// ---------------------------------------------------------------------
// Group 3: CVE-2025-22871 -- bare LF in chunk-size line (Go stdlib).
//
// Our coreserver REJECTS all Transfer-Encoding != identity on the
// warm path, so we are immune to this CVE today.  This test pins
// the rejection so a future "add chunked support" refactor cannot
// ship the CVE shape unnoticed: the exact byte sequence from the
// upstream Go issue MUST be refused.
// ---------------------------------------------------------------------

// TestCVE_2025_22871_BareLF_In_Chunked_Rejected sends a POST with
// Transfer-Encoding: chunked and a chunk-size line terminated by
// bare LF instead of CRLF (the CVE shape).  Our parser MUST reject
// the request as smuggling-suspect and close the connection.
func TestCVE_2025_22871_BareLF_In_Chunked_Rejected(t *testing.T) {
	t.Parallel()
	stack := newStack(t)
	addr := stripAddr(stack.ProxyURL())

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	// "5\n" chunk-size with bare LF -- CVE-2025-22871 shape.
	// We expect the server to refuse the request at the framing
	// layer (chunked is rejected outright) and close the conn.
	payload := "POST /acme/widget/upload HTTP/1.1\r\n" +
		"Host: pulsys.test\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"\r\n" +
		"5\nhello\r\n0\r\n\r\n"
	if _, err := io.WriteString(conn, payload); err != nil {
		// A write error is also acceptable: the server may
		// close as soon as it sees TE: chunked.
		return
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		// Clean close is acceptable: server refused before
		// fully composing a response.
		if isCleanCloseErr(err) {
			return
		}
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 4 {
		t.Fatalf("expected 4xx, got %d", resp.StatusCode)
	}
	if !respHasConnectionClose(resp) {
		t.Fatalf("response missing Connection: close on smuggling-suspect rejection")
	}
}

// ---------------------------------------------------------------------
// Group 4: protocol-version edge cases.
//
// Many production smuggling primitives ride in on weird-looking
// request lines: HTTP/0.9, HTTP/2.0 in an HTTP/1.1 socket, missing
// version entirely, leading CRLF.  Our parser MUST refuse each of
// these.  This is currently covered by validProtocolVersion in
// the unit tests; pinned here from the public-facing protocol
// surface so any refactor that bypasses validation on the
// integrated path trips CI.
// ---------------------------------------------------------------------

func TestRequestLine_BadShapesRejected(t *testing.T) {
	t.Parallel()
	stack := newStack(t)
	addr := stripAddr(stack.ProxyURL())

	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "http_0_9_simple_request",
			raw:  "GET /\r\n",
		},
		{
			name: "http_2_0_advertised_on_http1_socket",
			raw:  "GET / HTTP/2.0\r\nHost: x\r\n\r\n",
		},
		{
			name: "http_1_9_unknown_minor",
			raw:  "GET / HTTP/1.9\r\nHost: x\r\n\r\n",
		},
		{
			name: "http_1_x_garbage",
			raw:  "GET / HTTP/1.X\r\nHost: x\r\n\r\n",
		},
		{
			name: "leading_crlf_then_request",
			// Some parsers tolerate a leading CRLF (RFC 7230
			// 3.5).  Even if we do, the request shape after
			// must still be valid; this case carries a bad
			// version to ensure rejection.
			raw: "\r\nGET / HTTP/1.X\r\nHost: x\r\n\r\n",
		},
		{
			name: "lowercase_method_get",
			// fasthttp-class disagreement: stdlib admits
			// "get"; we reject (validMethod requires uppercase).
			raw: "get / HTTP/1.1\r\nHost: x\r\n\r\n",
		},
		{
			name: "mixed_case_method_Get",
			raw:  "Get / HTTP/1.1\r\nHost: x\r\n\r\n",
		},
		{
			name: "absolute_form_target_forward_proxy",
			raw:  "GET http://evil.example/path HTTP/1.1\r\nHost: x\r\n\r\n",
		},
		{
			name: "authority_form_target",
			raw:  "GET evil.example:443 HTTP/1.1\r\nHost: x\r\n\r\n",
		},
		{
			name: "empty_request_target",
			raw:  "GET  HTTP/1.1\r\nHost: x\r\n\r\n",
		},
		{
			name: "tab_in_method",
			raw:  "GE\tT / HTTP/1.1\r\nHost: x\r\n\r\n",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.WriteString(conn, tc.raw); err != nil {
				return // server may close immediately; acceptable.
			}
			br := bufio.NewReader(conn)
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				if isCleanCloseErr(err) {
					return
				}
				t.Fatalf("read: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("bad request line %q accepted as %d: %q", tc.raw, resp.StatusCode, body)
			}
			// 4xx is expected.  Connection: close is the
			// preferred shape on a parser-error response so
			// the LB stops forwarding pipelined garbage.
			// We do not hard-require it on every case
			// (errBadRequest may reuse) but we DO require
			// no 5xx (a 500 is an unhandled panic).
			if resp.StatusCode/100 == 5 {
				t.Fatalf("bad request line %q produced 5xx %d -- parser panic class", tc.raw, resp.StatusCode)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------

// isCleanCloseErr reports whether err looks like a server-side
// connection close (EOF, RST, or a deadline hit while waiting for
// data the server is never going to send).  All three are
// acceptable outcomes for a smuggling-rejected request.
func isCleanCloseErr(err error) bool {
	if err == nil {
		return false
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "EOF") ||
		strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "deadline exceeded")
}

// respHasConnectionClose reports whether resp will close the
// underlying TCP conn after the body is drained, regardless of
// where the signal came from (response.Close, explicit header, or
// HTTP/1.0 default).
func respHasConnectionClose(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if resp.Close {
		return true
	}
	for _, v := range resp.Header.Values("Connection") {
		if strings.EqualFold(strings.TrimSpace(v), "close") {
			return true
		}
	}
	return false
}
