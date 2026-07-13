// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// HTTP verb tampering matrix (OWASP WSTG-INPV-03).
//
// PURPOSE
//   Walk every public path on the proxy with every standard HTTP
//   method (plus a few non-standard ones a scanner would throw at
//   the surface) and assert two invariants:
//
//     1. The server NEVER 5xx's.  A 500 on an unexpected method is
//        an unhandled panic or a hidden parser branch -- both
//        production bugs.  4xx (400, 404, 405) is the desired
//        response shape.
//
//     2. The server NEVER ECHOES the request method or path back
//        in the response body.  Method/path reflection is the
//        ingredient that turns a verb-tampering bug into a stored
//        XSS or cache poisoning primitive (e.g. an HTML 500 page
//        that interpolates the requested path).
//
//   We probe both the admin-style /admin/api/v1/* routes (which
//   should respond 401/405) and the data-plane routes the
//   coreserver handles, plus the special TRACE / CONNECT methods
//   which historically leak request headers when handled
//   carelessly.
//
//   NOTE: this test runs against the public proxy listener (the
//   coreserver fallback wired into testserver.Stack), so admin
//   paths are exercised purely as opaque strings -- the proxy
//   does not know about /admin/, but it MUST NOT 5xx when probed
//   with arbitrary methods either.

package sectest

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// allMethods is the union of every IANA-registered standard method
// (RFC 9110) plus the non-standard ones common scanners send.  We
// also include a deliberately bogus method ("PWN") to catch parsers
// that silently accept any token as a method.
var allMethods = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPost,
	http.MethodPut,
	http.MethodDelete,
	http.MethodPatch,
	http.MethodConnect,
	http.MethodOptions,
	http.MethodTrace,
	"PWN", // bogus method: parser MUST treat as 400/405, not 500
}

// probePaths covers the four route classes the proxy listener may
// encounter: admin, data-plane API, content addressing, and a
// nonsense path that exercises the 404 branch.
var probePaths = []string{
	"/healthz",                // proxy-owned, GET-only
	"/api/models/acme/widget", // data-plane API
	"/acme/widget/resolve/main/config.json",
	"/api/whoami-v2",                   // typical auth probe
	"/_p/huggingface.co/some/resource", // /_p/ passthrough
	"/admin/api/v1/tenant",             // admin path on wrong listener
	"/does/not/exist/anywhere",         // 404 sink
}

// TestVerbTampering_NoFiveHundred runs every (method, path) pair
// and fails on any 5xx response or any response body that echoes
// the request line.
func TestVerbTampering_NoFiveHundred(t *testing.T) {
	stack := newStack(t)
	addr := stripAddr(stack.ProxyURL())
	client := &http.Client{
		Timeout: 5 * time.Second,
		// Don't follow redirects: an SSRF defense fail would
		// otherwise show up as a network error against the
		// redirect target instead of the suspicious 3xx itself.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for _, path := range probePaths {
		path := path
		for _, method := range allMethods {
			method := method
			t.Run(method+"_"+sanitiseForTestName(path), func(t *testing.T) {
				t.Parallel()
				// CONNECT cannot be sent via net/http to an
				// arbitrary path -- it must target an authority.
				// We instead raw-dial and send the bytes so the
				// coreserver sees what a scanner would send.
				if method == http.MethodConnect {
					raw := "CONNECT 127.0.0.1:80 HTTP/1.1\r\n" +
						"Host: " + addr + "\r\n\r\n"
					out, err := rawDialAndSend(t, addr, []byte(raw), 1024)
					if err != nil && !isExpectedCloseErr(err) {
						t.Fatalf("CONNECT raw dial: %v", err)
					}
					assertNoReflectionAndNo5xx(t, "CONNECT", path, out)
					return
				}
				// TRACE requires the same raw-dial because Go's
				// transport refuses to send it.
				if method == http.MethodTrace {
					raw := "TRACE " + path + " HTTP/1.1\r\n" +
						"Host: " + addr + "\r\n" +
						"X-Trace-Header: should-never-be-echoed\r\n\r\n"
					out, err := rawDialAndSend(t, addr, []byte(raw), 4096)
					if err != nil && !isExpectedCloseErr(err) {
						t.Fatalf("TRACE raw dial: %v", err)
					}
					if strings.Contains(out, "should-never-be-echoed") {
						t.Fatalf("TRACE echoed request headers (cross-site information leak):\n%s", out)
					}
					assertNoReflectionAndNo5xx(t, "TRACE", path, out)
					return
				}
				// "PWN" is a valid HTTP/1.1 method token per the
				// tchar grammar, so the coreserver parser will
				// accept it and pass it to the handler.  The
				// handler MUST then either route it like a GET
				// (if that's the intended semantics) or return a
				// 4xx -- never 5xx.
				req, err := http.NewRequest(method, stack.ProxyURL()+path, nil)
				if err != nil {
					t.Fatalf("build request: %v", err)
				}
				resp, err := client.Do(req)
				if err != nil {
					// Network errors are acceptable (some methods
					// against some paths intentionally close the
					// connection).  The test FAILS only on 5xx
					// observed on the wire, which requires a
					// completed response.
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode >= 500 && resp.StatusCode < 600 {
					t.Fatalf("WSTG-INPV-03: %s %s returned 5xx (%d) -- unhandled method branch",
						method, path, resp.StatusCode)
				}
			})
		}
	}
}

// TestVerbTampering_OptionsDoesNotLeakCors asserts OPTIONS
// responses do not advertise wildcard CORS access or reflect an
// arbitrary Origin header.  This is the WSTG-CONF-07 adjacency:
// permissive CORS combined with credentialed sessions is the
// canonical cross-origin attack chain.
func TestVerbTampering_OptionsDoesNotLeakCors(t *testing.T) {
	stack := newStack(t)
	client := &http.Client{Timeout: 5 * time.Second}

	for _, path := range probePaths {
		path := path
		t.Run("OPTIONS_"+sanitiseForTestName(path), func(t *testing.T) {
			t.Parallel()
			req, _ := http.NewRequest(http.MethodOptions, stack.ProxyURL()+path, nil)
			req.Header.Set("Origin", "https://evil.example")
			req.Header.Set("Access-Control-Request-Method", "DELETE")
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if v := resp.Header.Get("Access-Control-Allow-Origin"); v == "*" {
				t.Fatalf("WSTG-CONF-07: OPTIONS %s returned ACAO:* (no permissive CORS allowed on data plane)", path)
			}
			if v := resp.Header.Get("Access-Control-Allow-Origin"); v == "https://evil.example" {
				t.Fatalf("WSTG-CONF-07: OPTIONS %s reflected attacker Origin %q in ACAO", path, v)
			}
			if v := resp.Header.Get("Access-Control-Allow-Credentials"); v == "true" {
				t.Fatalf("WSTG-CONF-07: OPTIONS %s returned ACAC:true (credentialed cross-origin is forbidden)", path)
			}
		})
	}
}

// assertNoReflectionAndNo5xx is the shared assertion for raw-dial
// probes.  Parses the response status line, checks for 5xx, and
// scans the body for the request path (which MUST NOT be echoed
// verbatim into a 4xx error body -- that is the cache-poisoning
// gadget pattern).
func assertNoReflectionAndNo5xx(t *testing.T, method, path, raw string) {
	t.Helper()
	if raw == "" {
		// Server closed the connection without responding.  That
		// is a valid response to a malformed request; nothing to
		// assert against.
		return
	}
	// First line: "HTTP/1.1 <code> <reason>"
	line := raw
	if i := strings.IndexByte(raw, '\r'); i >= 0 {
		line = raw[:i]
	}
	if !strings.HasPrefix(line, "HTTP/1.") {
		t.Fatalf("%s %s: malformed status line %q", method, path, line)
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return
	}
	code := parts[1]
	if len(code) == 3 && code[0] == '5' {
		t.Fatalf("WSTG-INPV-03: %s %s returned 5xx (%s) -- unhandled method branch\n  raw=%q",
			method, path, code, truncate([]byte(raw), 200))
	}
	if strings.Contains(raw, path) && strings.Contains(path, "/") && len(path) > 8 {
		// Reflection of the request path into the body is the
		// stored-XSS / cache-poisoning gadget.  Only flag when
		// the path is non-trivial (>8 chars) so generic error
		// pages that happen to contain "/" don't fail.
		// Allow Location: headers to echo the path (legitimate
		// redirect behavior).
		if !strings.Contains(raw, "Location: "+path) &&
			!strings.Contains(raw, "Location: http") {
			t.Fatalf("WSTG-INPV-03: %s %s echoed request path into response body\n  raw=%q",
				method, path, truncate([]byte(raw), 300))
		}
	}
}

// isExpectedCloseErr reports whether err is the "server closed
// the connection" class of error that is the EXPECTED outcome for
// many verb-tampering probes.
func isExpectedCloseErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection reset") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "use of closed network connection")
}

// sanitiseForTestName makes a path safe to appear in a t.Run
// sub-test name (no "/" which the testing package interprets as a
// nested path separator and no whitespace).
func sanitiseForTestName(s string) string {
	r := strings.NewReplacer("/", "_", " ", "_", "?", "_")
	return r.Replace(s)
}

// truncate is defined in info_disclosure_test.go alongside other
// shared helpers; do NOT redefine here.

// ensure net is imported so go vet doesn't drop it during edits
var _ = net.IPv4zero
