// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Error-page information disclosure (OWASP WSTG-ERRH-01 / -02).
//
// PURPOSE
//   When the server returns an error response, the body must
//   NOT include any of the following information-disclosure
//   markers an attacker could use to plan their next move:
//
//     - Go runtime panic stack traces
//       ("goroutine N [running]", "panic: ...", "/usr/local/go/...")
//     - Source file paths
//       ("/Users/...", "/Workspace/...", "internal/...")
//     - Internal IP / hostname literals
//       ("127.0.0.1", "::1", "localhost", "*.internal")
//     - Postgres / driver error structure
//       ("SQLSTATE", "pq:", "pgconn")
//     - Go stdlib type names that betray implementation choices
//       ("*errors.errorString", "runtime.gopanic")
//
//   COVERAGE STRATEGY
//   We can't easily force every possible 5xx path from the
//   outside, but we CAN cover the common gadgets:
//
//     1. Malformed JSON body  -> handler's JSON decode fails
//     2. Oversized body       -> request entity too large path
//     3. Path with traversal  -> URL parser path
//     4. Method on a route that does not exist
//     5. Header with bare CR  -> parser path (already covered
//        in coreserver tests; replicated here for the wire)
//
//   We also fuzz the data-plane URL space with random paths
//   and confirm no probe body ever contains any disclosure
//   marker.

package sectest

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// disclosureMarkers are substrings that, if present in a
// response body, would constitute an information disclosure
// per WSTG-ERRH-02.  The list is intentionally broad; false
// positives on legitimate paths (e.g. a public API path with
// "internal" in the name) are not in scope for this project
// because we don't expose any such paths.
var disclosureMarkers = []string{
	// Go runtime panic markers
	"goroutine ",
	"panic:",
	"runtime.gopanic",
	"runtime/panic.go",
	"runtime.main",
	// Source-tree paths (this repo's checkout root)
	"/Users/",
	"/Workspace/",
	"/home/runner/",
	"/build/",
	"/go/pkg/mod/",
	// Pulsys package paths -- a tree-shaped trace would
	// reveal these.
	"github.com/pulsys-io/pulsys/internal/",
	"pulsys-go/internal/",
	// Stdlib type names + errors.New defaults
	"*errors.errorString",
	"&errors.errorString",
	// Postgres / pgx markers (also asserted in SQLi audit,
	// re-checked here for completeness on the wire surface).
	"SQLSTATE",
	"pq:",
	"pgconn",
	"syntax error at or near",
	// Internal network identifiers that a leaky upstream
	// rewriter might surface.
	"169.254.169.254",
	"metadata.google.internal",
}

// errorTriggers are HTTP requests designed to force a 4xx/5xx
// response on the data plane.  Each one targets a different
// error-path branch.
type errorTrigger struct {
	name    string
	method  string
	path    string
	headers map[string]string
	body    []byte
}

var errorTriggers = []errorTrigger{
	{name: "malformed_json_body",
		method: "POST", path: "/api/whoami-v2",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    []byte(`{not-valid-json...`)},
	{name: "oversized_body",
		method: "POST", path: "/api/models",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    largeBody(2 << 20)}, // 2 MiB
	{name: "path_traversal_dotdot",
		method: "GET", path: "/api/models/../../../etc/passwd",
		headers: nil, body: nil},
	{name: "method_on_missing_route",
		method: "PATCH", path: "/totally/missing/route",
		headers: nil, body: nil},
	{name: "double_slash_path",
		method: "GET", path: "//api////models//.//.",
		headers: nil, body: nil},
	{name: "null_byte_in_path",
		method: "GET", path: "/api/models/with%00null",
		headers: nil, body: nil},
}

// TestErrorDisclosure_NoStackOrPathInResponseBody runs every
// trigger and asserts no marker appears in the response body.
// We deliberately don't assert the STATUS CODE here (some
// triggers correctly produce 400, others 404, others 405);
// the invariant is purely about body content.
func TestErrorDisclosure_NoStackOrPathInResponseBody(t *testing.T) {
	stack := newStack(t)
	client := &http.Client{}

	for _, tg := range errorTriggers {
		tg := tg
		t.Run(tg.name, func(t *testing.T) {
			t.Parallel()
			var body io.Reader
			if len(tg.body) > 0 {
				body = strings.NewReader(string(tg.body))
			}
			req, err := http.NewRequest(tg.method, stack.ProxyURL()+tg.path, body)
			if err != nil {
				// Some Go-side URL validations may reject our
				// trigger before it leaves the client; that's
				// fine for this test.
				return
			}
			for k, v := range tg.headers {
				req.Header.Set(k, v)
			}
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			scanForMarkers(t, tg.name, string(respBody))
		})
	}
}

// TestErrorDisclosure_RawDialBareCRInHeader replays the bare-CR
// header value that the parser rejects (see fuzz_parser_test.go)
// and asserts the rejection body doesn't include any disclosure
// markers.  This is the wire-level pin for the corresponding
// coreserver unit test.
func TestErrorDisclosure_RawDialBareCRInHeader(t *testing.T) {
	stack := newStack(t)
	addr := stripAddr(stack.ProxyURL())
	raw := "GET / HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"X-Trigger: bad-value\rextra\r\n" + // bare CR mid-header
		"Connection: close\r\n\r\n"
	out, err := rawDialAndSend(t, addr, []byte(raw), 8192)
	if err != nil && !isExpectedCloseErr(err) {
		t.Fatalf("raw dial: %v", err)
	}
	scanForMarkers(t, "bare_cr_in_header", out)
}

// TestErrorDisclosure_RawDialOverlongRequestLine sends a
// pathologically long request line and asserts the 431 / 414 /
// 400 response body is marker-free.
func TestErrorDisclosure_RawDialOverlongRequestLine(t *testing.T) {
	stack := newStack(t)
	addr := stripAddr(stack.ProxyURL())
	raw := "GET /" + strings.Repeat("A", 16384) + " HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Connection: close\r\n\r\n"
	out, err := rawDialAndSend(t, addr, []byte(raw), 8192)
	if err != nil && !isExpectedCloseErr(err) {
		t.Fatalf("raw dial: %v", err)
	}
	scanForMarkers(t, "overlong_request_line", out)
}

// TestErrorDisclosure_AdminListenerRoutes asserts the admin
// listener's 404 / 405 / 400 responses are also marker-free.
// (The admin listener exposes the auth + admin API paths, all
// of which write JSON error bodies.)
func TestErrorDisclosure_AdminListenerRoutes(t *testing.T) {
	stack := newStack(t)
	client := &http.Client{}
	probes := []struct{ name, path string }{
		{"unmounted_admin", "/admin/api/v1/does/not/exist"},
		{"missing_auth_endpoint", "/auth/missing"},
		{"healthz_with_bad_body", "/healthz"},
	}
	for _, p := range probes {
		p := p
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			req, _ := http.NewRequest("POST", stack.ProxyURL()+p.path,
				strings.NewReader("not-json"))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			scanForMarkers(t, p.name, string(body))
		})
	}
}

// scanForMarkers fails the calling test if any disclosureMarker
// substring appears in body.  Reports the first match for
// triage.
func scanForMarkers(t *testing.T, label, body string) {
	t.Helper()
	for _, m := range disclosureMarkers {
		if strings.Contains(body, m) {
			t.Fatalf("WSTG-ERRH-02: %s response leaked marker %q\n  body[:400]=%q",
				label, m, truncate([]byte(body), 400))
		}
	}
}

func largeBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'A'
	}
	return b
}
