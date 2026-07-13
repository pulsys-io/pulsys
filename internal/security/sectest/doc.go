// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package sectest is the end-to-end security regression test
// surface for pulsys.
//
// # Why this package exists
//
// internal/coreserver/parser_*_test.go covers the HTTP/1.1 parser
// in isolation against synthetic inputs.  internal/security/
// authcontract covers the auth gate against every documented
// endpoint.  This package fills the remaining slot in the test
// pyramid: full-stack, raw-TCP regression tests against the
// integrated coreserver + handler stack for the OWASP WSTG
// categories that have no narrower home.
//
//   - httpparser_smuggling_test.go: HTTP request smuggling via the
//     real TCP listener (not just readRequest in isolation).  This
//     covers connection-lifecycle behavior the unit tests can't:
//     after a parse error we MUST close the socket so a smuggled
//     follow-on can't be reinterpreted.
//
//   - info_disclosure_test.go: /debug/pprof, /debug/vars, .env,
//     .git, /admin, /actuator, /server-status, /metrics from off-
//     box, and 404-shape stability across unmounted paths.
//
//   - path_traversal_test.go: percent-encoded, double-encoded, and
//     overlong-UTF-8 .. sequences in the /<org>/<repo>/resolve/...
//     path namespace.
//
//   - ssrf_test.go: X-Forwarded-Host, Host, and Location-header
//     rewrites that try to push the proxy to fetch loopback / link-
//     local / private addresses.
//
//   - response_splitting_test.go: CRLF injection into upstream
//     response headers that the proxy copies verbatim.
//
// # Why these are in a separate package
//
// authcontract asserts WHO can call WHAT.  sectest asserts WHAT
// the network protocol surface accepts at all, irrespective of
// auth.  Both surfaces matter and would have caught different
// classes of incident.
//
// # When to add tests here
//
//   - A new public-facing handler is added: add an info-disclosure
//     row that asserts the path returns 404 from anonymous.
//
//   - A new header is read from the request: add a response-splitting
//     row that fuzz-feeds CR/LF into the header value.
//
//   - A new upstream URL is constructed: add an SSRF row that
//     tries to point it at 169.254.169.254 / 127.0.0.1 / fd00::.
//
// # CI behavior
//
// All tests in this package run against testserver.New, which is
// fully self-contained (no external DBs or networks).  go test
// ./internal/security/sectest/... is therefore the same on a fresh
// clone as in CI.
package sectest
