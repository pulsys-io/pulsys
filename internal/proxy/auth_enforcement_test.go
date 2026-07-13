// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/pulsys-io/pulsys/internal/coreserver"
	"github.com/pulsys-io/pulsys/internal/hfhub/fixtures"
	"github.com/pulsys-io/pulsys/internal/hfhub/mockhub"
	"github.com/pulsys-io/pulsys/internal/testserver"
)

// validPAT is the one bearer value the fakeGate below admits.  It's a
// well-formed pulsys_ token but never appears in any real datastore;
// the gate is the only authority that recognizes it for tests.
const validPAT = "pulsys_deadbeef_thisoneisaccepted"

// fakeGate is an inline coreserver.AuthGate with the same shape as the
// production auth.PATGate (see internal/auth/proxygate.go), pared down
// to one in-memory token.  The integration tests below verify that
// *any* gate plugged into Server.AuthGate is honored by every code
// path -- warm hits, slow-path fallback, /healthz bypass.  Unit tests
// for the real PATGate (DB lookup, TTL cache, revocation) live in
// internal/auth.
func fakeGate() coreserver.AuthGate {
	return coreserver.AuthGateFunc(func(_ context.Context, auth, path []byte) (int, string) {
		// /healthz is unauthenticated by design (load balancers).
		if bytes.Equal(path, []byte("/healthz")) {
			return 0, ""
		}
		const prefix = "Bearer "
		if len(auth) < len(prefix) || !bytes.EqualFold(auth[:len(prefix)], []byte(prefix)) {
			return 401, "missing Authorization (expected Bearer pulsys_...)"
		}
		tok := string(bytes.TrimSpace(auth[len(prefix):]))
		if tok != validPAT {
			return 401, "invalid or revoked token"
		}
		return 0, ""
	})
}

// newGatedStack stands up a testserver Stack with fakeGate installed
// and a small tinymodel seeded so we have something to download.
func newGatedStack(t *testing.T) *testserver.Stack {
	t.Helper()
	files := fixtures.TinyModelFiles("acme/widget")
	return testserver.New(t, testserver.Config{
		AuthGate: fakeGate(),
		Mock: mockhub.Config{
			Repos: []mockhub.RepoSpec{{
				Name: "acme/widget", InitialFiles: files,
			}},
		},
	})
}

// TestProxyDataPlane_RevokedPATIsRejected reproduces the user's
// 2026-05-21 report ("revoked token still allowed me to download")
// against the production stack wiring.  When an AuthGate is installed
// on the coreserver, a forged or revoked pulsys_... token must be
// rejected with 401 before either the warm cache or the upstream
// forwarder runs.
func TestProxyDataPlane_RevokedPATIsRejected(t *testing.T) {
	stack := newGatedStack(t)

	url := stack.ProxyURL() + "/acme/widget/resolve/main/config.json"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer pulsys_7be71e62_thistokenwasrevoked")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged PAT: want 401, got %d (body=%q)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got == "" {
		t.Errorf("expected WWW-Authenticate header on 401, got none")
	}
	if !bytes.Contains(body, []byte("invalid")) && !bytes.Contains(body, []byte("revoked")) {
		t.Errorf("401 body should explain why the token was rejected, got %q", body)
	}
}

// TestProxyDataPlane_UnauthenticatedIsRejected covers the
// no-Authorization-header case.  Should fail closed when a gate is
// installed.
func TestProxyDataPlane_UnauthenticatedIsRejected(t *testing.T) {
	stack := newGatedStack(t)

	resp, err := http.Get(stack.ProxyURL() + "/acme/widget/resolve/main/config.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("anonymous request: want 401, got %d (body=%q)", resp.StatusCode, body)
	}
}

// TestProxyDataPlane_ValidPATIsAdmitted is the happy path: a token
// the gate recognizes should be admitted, the cold fetch should
// reach upstream, and the response body should match the seeded
// fixture.  Without this anchor a buggy gate that rejects everything
// would satisfy the two failing tests above while breaking
// production.
func TestProxyDataPlane_ValidPATIsAdmitted(t *testing.T) {
	stack := newGatedStack(t)
	files := fixtures.TinyModelFiles("acme/widget")

	url := stack.ProxyURL() + "/acme/widget/resolve/main/config.json"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+validPAT)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("valid PAT: want 200, got %d (body=%q)", resp.StatusCode, body)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, files["config.json"]) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(got), len(files["config.json"]))
	}
}

// TestProxyDataPlane_WarmHitStillGated proves the gate runs ahead of
// the warm-cache fast path.  Sequence:
//
//  1. Authenticate once with a valid PAT to populate the cache.
//  2. Retry the same URL with a forged PAT.  The cache hit must NOT
//     be replayed; the response must be 401.
//
// This is the warm-path equivalent of the user's report: the bug
// would be especially nasty if a previously authenticated token had
// warmed the cache and any subsequent unauthenticated request could
// drain the cached bytes for free.
func TestProxyDataPlane_WarmHitStillGated(t *testing.T) {
	stack := newGatedStack(t)
	url := stack.ProxyURL() + "/acme/widget/resolve/main/config.json"

	// 1. Cold fetch with valid PAT primes the cache.
	prime, _ := http.NewRequest(http.MethodGet, url, nil)
	prime.Header.Set("Authorization", "Bearer "+validPAT)
	resp1, err := http.DefaultClient.Do(prime)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("priming request: want 200, got %d", resp1.StatusCode)
	}

	// 2. Warm-path attempt with a forged PAT must be rejected.
	bad, _ := http.NewRequest(http.MethodGet, url, nil)
	bad.Header.Set("Authorization", "Bearer pulsys_7be71e62_thistokenwasrevoked")
	resp2, err := http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("warm-hit with revoked PAT: want 401, got %d (body=%q)\n"+
			"this means the AuthGate runs AFTER the warm-cache lookup, "+
			"which would let a revoked token replay any cached object",
			resp2.StatusCode, body)
	}
}

// TestProxyDataPlane_HealthzBypassesGate verifies the /healthz carve-
// out so load balancers can probe the proxy without holding a PAT.
func TestProxyDataPlane_HealthzBypassesGate(t *testing.T) {
	stack := newGatedStack(t)
	resp, err := http.Get(stack.ProxyURL() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz with gate enabled: want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("/healthz body = %q, want %q", body, "ok")
	}
}

// TestProxyDataPlane_NoGateIsPermissive locks in the dev-mode
// contract: when AuthGate is nil (PULSYS_DB_DSN not configured), the
// proxy continues to serve any request, mirroring the existing
// docker-compose / dev experience.  Without this anchor a future
// refactor could accidentally make the gate mandatory and break
// every dev harness in the repo.
func TestProxyDataPlane_NoGateIsPermissive(t *testing.T) {
	files := fixtures.TinyModelFiles("acme/widget")
	stack := testserver.New(t, testserver.Config{
		// AuthGate intentionally omitted.
		Mock: mockhub.Config{
			Repos: []mockhub.RepoSpec{{
				Name: "acme/widget", InitialFiles: files,
			}},
		},
	})
	resp, err := http.Get(stack.ProxyURL() + "/acme/widget/resolve/main/config.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dev-mode (no gate): want 200, got %d", resp.StatusCode)
	}
}
