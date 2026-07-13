// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package authcontract

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/auth"
	hffixtures "github.com/pulsys-io/pulsys/internal/hfhub/fixtures"
	"github.com/pulsys-io/pulsys/internal/hfhub/mockhub"
	"github.com/pulsys-io/pulsys/internal/testserver"
)

// dataplaneEndpoints returns the full auth contract for the pulsys
// data plane.  Unlike the admin surface, every protected route here
// has the same shape -- the PATGate doesn't enforce scope or role,
// only "is this a real, non-revoked, non-expired PAT?" -- so most
// rows reuse the gateProtected() outcomes map.
//
// The list spans every HF wire surface the proxy exposes:
//
//   - Read-side metadata (modelinfo, tree, paths-info) for models /
//     datasets / spaces.
//   - Read-side resolve (GET + HEAD) and LFS download paths.
//   - Write-side registry (repo create, preupload, commit) and LFS
//     upload (batch, verify, storage PUT).
//   - The /healthz carve-out for load balancers.
//
// If a new path-handling surface is added (e.g. a new /api/...
// endpoint or a webhook), it MUST get a row here.  The
// TestDataPlaneContract_NewPathsAreNot401Free regression check
// hammers a small set of "interesting" paths through the gate and
// fails if any of them returns 200 with the anonymous credential --
// catching a future regression where someone widens the allowlist.
func dataplaneEndpoints() []Endpoint {
	jsonBody := []byte(`{}`)
	return []Endpoint{
		// ---- public carve-out ----
		{
			Method:   "GET",
			Path:     "/healthz",
			Note:     "load balancer probe",
			Outcomes: publicDataPlane(),
		},
		// ---- read-side metadata ----
		{
			Method:   "GET",
			Path:     "/api/models/acme/widget",
			Note:     "model info",
			Outcomes: gateProtected(),
		},
		{
			Method:   "GET",
			Path:     "/api/models/acme/widget/tree/main",
			Note:     "tree listing",
			Outcomes: gateProtected(),
		},
		{
			Method:      "POST",
			Path:        "/api/models/acme/widget/paths-info/main",
			Body:        jsonBody,
			ContentType: "application/json",
			Note:        "paths info",
			Outcomes:    gateProtected(),
		},
		{
			Method:   "GET",
			Path:     "/api/datasets/acme/widget",
			Note:     "dataset info (HF wire variant)",
			Outcomes: gateProtected(),
		},
		{
			Method:   "GET",
			Path:     "/api/spaces/acme/widget",
			Note:     "space info (HF wire variant)",
			Outcomes: gateProtected(),
		},

		// ---- read-side resolve / download ----
		{
			Method:   "GET",
			Path:     "/acme/widget/resolve/main/config.json",
			Note:     "blob download (the original incident path)",
			Outcomes: gateProtected(),
		},
		{
			Method:   "HEAD",
			Path:     "/acme/widget/resolve/main/config.json",
			Note:     "HEAD probe before download",
			Outcomes: gateProtected(),
		},
		{
			Method:   "GET",
			Path:     "/datasets/acme/widget/resolve/main/config.json",
			Note:     "dataset blob download (HF wire variant)",
			Outcomes: gateProtected(),
		},

		// ---- write-side registry ----
		{
			Method:      "POST",
			Path:        "/api/repos/create",
			Body:        []byte(`{"name":"matrix/test","type":"model","private":false}`),
			ContentType: "application/json",
			Note:        "repo create",
			Outcomes:    gateProtected(),
		},
		{
			Method:      "POST",
			Path:        "/api/models/acme/widget/preupload/main",
			Body:        jsonBody,
			ContentType: "application/json",
			Note:        "preupload (write)",
			Outcomes:    gateProtected(),
		},
		{
			Method:      "POST",
			Path:        "/api/models/acme/widget/commit/main",
			Body:        jsonBody,
			ContentType: "application/json",
			Note:        "commit (write)",
			Outcomes:    gateProtected(),
		},

		// ---- LFS surface ----
		{
			Method:      "POST",
			Path:        "/acme/widget.git/info/lfs/objects/batch",
			Body:        jsonBody,
			ContentType: "application/vnd.git-lfs+json",
			Note:        "LFS batch",
			Outcomes:    gateProtected(),
		},
		{
			Method:      "POST",
			Path:        "/acme/widget.git/info/lfs/verify",
			Body:        jsonBody,
			ContentType: "application/vnd.git-lfs+json",
			Note:        "LFS verify",
			Outcomes:    gateProtected(),
		},
		{
			Method:      "PUT",
			Path:        "/lfs-storage/0123456789abcdef",
			Body:        []byte("payload"),
			ContentType: "application/octet-stream",
			Note:        "LFS storage PUT",
			Outcomes:    gateProtected(),
		},

		// ---- catch-all probe ----
		// A randomly-chosen unmapped path must still be gated.  This is
		// a hedge: if a future refactor widens the gate's allowlist by
		// accident (e.g. matches by prefix instead of exact equality),
		// this row catches it.
		{
			Method:   "GET",
			Path:     "/notarealendpoint/probe/" + strings.Repeat("x", 8),
			Note:     "anonymous probe of unmapped path must still 401",
			Outcomes: gateProtected(),
		},
	}
}

// publicDataPlane is the outcomes map for routes the PATGate
// short-circuits (/healthz, /readyz, /metrics).  Every credential is
// admitted because the gate returns (0, "") before any token lookup.
func publicDataPlane() map[Credential]Outcome {
	out := make(map[Credential]Outcome, 7)
	for _, c := range dataPlaneCredentials() {
		out[c] = Admitted
	}
	return out
}

// gateProtected is the outcomes map for every non-allowlisted data
// plane endpoint.  Invalid credentials 401; any valid PAT (read /
// write / admin:*) is admitted because the gate doesn't enforce
// scope -- that's the upstream handler's job.
func gateProtected() map[Credential]Outcome {
	return map[Credential]Outcome{
		CredAnonymous:        Unauth401,
		CredBogusPAT:         Unauth401,
		CredRevokedPAT:       Unauth401,
		CredExpiredPAT:       Unauth401,
		CredPATScopeRead:     Admitted,
		CredPATScopeWrite:    Admitted,
		CredPATScopeAdminAll: Admitted,
	}
}

// TestDataPlaneContract drives every (endpoint, credential) cell
// against a real coreserver wired with the production auth.PATGate.
//
// The test boots:
//
//   - A fresh Postgres database (via testpg.Acquire).
//   - PATs / users / sessions via newFixtures.
//   - A testserver.Stack with a real coreserver, a mock Hub
//     upstream, and the production PATGate installed -- exactly
//     what cmd/pulsys/main.go wires when PULSYS_DB_DSN is set.
//
// Every request goes through the real coreserver listener (HTTP/1.1
// over a loopback socket), so the warm-cache fast path and the
// fallback path are both covered.  This is the high-confidence
// integration anchor that would have caught the 2026-05-21 incident.
func TestDataPlaneContract(t *testing.T) {
	f := newFixtures(t)

	// Use a small PositiveTTL so we don't worry about cache stalking
	// between sub-tests; revoked-and-expired tokens are never cached
	// (lookupCached only stores on positive results), but a previous
	// run's positive cache could mask a later DB change.  500ms is
	// well below test timeouts and above lookup latency.
	gate := &auth.PATGate{
		Store:       f.AuthStore,
		PositiveTTL: 500 * time.Millisecond,
	}

	stack := testserver.New(t, testserver.Config{
		AuthGate: gate,
		Mock: mockhub.Config{
			Repos: []mockhub.RepoSpec{{
				Name:         "acme/widget",
				InitialFiles: hffixtures.TinyModelFiles("acme/widget"),
			}},
		},
	})

	endpoints := dataplaneEndpoints()

	for _, ep := range endpoints {
		ep := ep
		t.Run(ep.String(), func(t *testing.T) {
			// Not Parallel.  PATGate caches positive results for
			// PositiveTTL; running sub-tests serially keeps the
			// cache predictable per credential class.
			for _, cred := range dataPlaneCredentials() {
				cred := cred
				want := ep.requireOutcome(cred)
				t.Run(cred.String(), func(t *testing.T) {
					url := stack.ProxyURL() + ep.Path
					var body io.Reader
					if ep.Body != nil {
						body = bytes.NewReader(ep.Body)
					}
					req, err := http.NewRequest(ep.Method, url, body)
					if err != nil {
						t.Fatalf("build request: %v", err)
					}
					if ep.ContentType != "" {
						req.Header.Set("Content-Type", ep.ContentType)
					}
					f.Apply(req, cred)

					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatalf("request: %v", err)
					}
					defer resp.Body.Close()
					respBody, _ := io.ReadAll(resp.Body)

					if msg := want.Check(resp.StatusCode); msg != "" {
						t.Errorf("auth contract violated for %s with %s\n  want: %s\n  got:  %d %s\n  body: %s\n  note: %s",
							ep, cred, want, resp.StatusCode,
							http.StatusText(resp.StatusCode),
							strings.TrimSpace(string(respBody)), ep.Note)
					}
				})
			}
		})
	}
}
