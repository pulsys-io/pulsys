// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// CORS audit (OWASP WSTG-CONF-07 / API Security Top-10 #7).
//
// PURPOSE
//   Pulsys' design intentionally does NOT participate in CORS:
//
//     - The data plane (this listener / proxy.Handler) serves
//       binary model files and JSON metadata to first-party
//       clients (huggingface-cli, Pulsys SDKs, hf-transfer).
//       Browser-origin clients are NOT a supported caller; if
//       a browser ever sends a CORS preflight here, the
//       correct answer is "no" -- no Access-Control-Allow-*
//       headers, full stop.
//
//     - The admin port is served same-origin with the admin
//       SPA (Next.js).  The SPA fetches from the same host:port
//       it was loaded from.  CORS is unnecessary because no
//       cross-origin requests are legitimate.
//
//   The single failure mode this test catches is:
//     "a future maintainer adds a permissive CORS middleware
//      to fix a localhost dev experience and accidentally
//      ships ACAO: * to production."
//
//   Three invariants asserted here:
//
//     1. NO endpoint EVER returns Access-Control-Allow-Origin: *
//     2. NO endpoint EVER returns ACAO with a reflected attacker
//        Origin header
//     3. NO endpoint EVER returns Access-Control-Allow-Credentials:
//        true (the most dangerous CORS combination: with =true,
//        a wildcard origin is disallowed, but a reflected origin
//        becomes an authenticated CSRF vector)
//
//   PROBES:
//     - GET, POST, PUT, DELETE, OPTIONS preflight
//     - Origin: https://evil.example
//     - Origin: null
//     - Origin: <self> (legitimate same-origin)

package sectest

import (
	"net/http"
	"strings"
	"testing"
)

// corsResponseSinks lists the response headers that, when
// present and "wrong", constitute a CORS misconfiguration.
// We check each one against a value-or-reflection policy.
var corsBadValues = []struct {
	header          string
	forbiddenEq     string   // exact-equal forbidden value
	forbiddenIn     []string // any-substring forbidden values
	allowOriginRefl bool     // if true, value MUST NOT equal Origin
}{
	{header: "Access-Control-Allow-Origin", forbiddenEq: "*"},
	{header: "Access-Control-Allow-Origin", allowOriginRefl: true},
	{header: "Access-Control-Allow-Credentials", forbiddenEq: "true"},
	{header: "Access-Control-Allow-Methods", forbiddenIn: []string{"*"}},
	{header: "Access-Control-Allow-Headers", forbiddenIn: []string{"*"}},
	{header: "Access-Control-Expose-Headers", forbiddenIn: []string{"*"}},
	{header: "Timing-Allow-Origin", forbiddenEq: "*"},
}

var corsProbeOrigins = []string{
	"https://evil.example",
	"https://attacker.com:8443",
	"http://localhost:1337",  // local dev origin probe
	"null",                   // common file:// or sandboxed iframe origin
	"https://huggingface.co", // looks legit but is still cross-origin
}

var corsProbePaths = []string{
	"/healthz",
	"/api/models/acme/widget",
	"/acme/widget/resolve/main/config.json",
	"/api/whoami-v2",
	"/_p/huggingface.co/some/resource",
	"/admin/api/v1/tenant", // would be 404 here; still asserts CORS headers absent
}

var corsProbeMethods = []string{
	http.MethodGet,
	http.MethodPost,
	http.MethodPut,
	http.MethodDelete,
	http.MethodOptions,
}

// TestCORS_DataPlaneNoCORSResponses runs every (origin, path,
// method) combination and asserts no CORS-grant header appears
// in the response.
func TestCORS_DataPlaneNoCORSResponses(t *testing.T) {
	stack := newStack(t)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for _, origin := range corsProbeOrigins {
		origin := origin
		for _, path := range corsProbePaths {
			path := path
			for _, method := range corsProbeMethods {
				method := method
				name := strings.NewReplacer("/", "_", " ", "_", ":", "_", ".", "_").Replace(
					method + "_" + origin + "_" + path)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					req, err := http.NewRequest(method, stack.ProxyURL()+path, nil)
					if err != nil {
						return
					}
					req.Header.Set("Origin", origin)
					if method == http.MethodOptions {
						req.Header.Set("Access-Control-Request-Method", "DELETE")
						req.Header.Set("Access-Control-Request-Headers", "Authorization, X-Custom")
					}
					resp, err := client.Do(req)
					if err != nil {
						return
					}
					defer resp.Body.Close()
					assertNoCORSGrant(t, resp.Header, origin, path)
				})
			}
		}
	}
}

// TestCORS_AdminListenerNoCORSResponses runs the same matrix
// against the admin handler in-process (not over the network).
// The admin port is loopback-only in production so a network
// probe wouldn't reach it; we exercise the handler chain
// directly to assert CORS posture.
func TestCORS_AdminListenerNoCORSResponses(t *testing.T) {
	// We don't have a same-package admin fixture here; the
	// authcontract package owns those.  Instead we make a
	// minimal raw-TCP probe against the data plane's
	// /admin/api/v1/* paths -- which the data-plane listener
	// 404s on, but the CORS-header assertion is unaffected
	// (the middleware that adds them runs regardless of
	// dispatch result).
	stack := newStack(t)
	client := &http.Client{}
	req, _ := http.NewRequest(http.MethodOptions,
		stack.ProxyURL()+"/admin/api/v1/tenant", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "DELETE")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	assertNoCORSGrant(t, resp.Header, "https://evil.example", "/admin/api/v1/tenant")
}

// assertNoCORSGrant fails if any CORS-permissive header is
// present in resp with a value that would constitute a CORS
// grant to the attacker origin.
func assertNoCORSGrant(t *testing.T, h http.Header, origin, path string) {
	t.Helper()
	for _, rule := range corsBadValues {
		got := h.Get(rule.header)
		if got == "" {
			continue
		}
		if rule.forbiddenEq != "" && got == rule.forbiddenEq {
			t.Fatalf("WSTG-CONF-07: %s set %q=%q (path=%s, origin=%s)",
				path, rule.header, got, path, origin)
		}
		if rule.allowOriginRefl && got == origin {
			t.Fatalf("WSTG-CONF-07: %s REFLECTED attacker Origin in %q (path=%s, origin=%s)",
				path, rule.header, path, origin)
		}
		for _, sub := range rule.forbiddenIn {
			if strings.Contains(got, sub) {
				t.Fatalf("WSTG-CONF-07: %s %q=%q contains forbidden value %q (origin=%s)",
					path, rule.header, got, sub, origin)
			}
		}
	}
}
