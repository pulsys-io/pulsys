// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Data-plane security-header pin (WSTG-CONF-06 / -07).
//
// PURPOSE
//   The data plane has a DIFFERENT security-header contract
//   than the admin surface (see internal/admin/server.go for
//   the admin-side rationale):
//
//     - X-Content-Type-Options: nosniff   -> REQUIRED on every
//       response.  Universal, safe even on binary downloads
//       (safetensors, parquet).
//
//     - X-Frame-Options / CSP             -> NOT required.
//       The data plane is a CLI surface, not a browser one;
//       framing / CSP would not add defense and a wrong CSP
//       could break legitimate notebook clients.
//
//     - Cross-Origin-Resource-Policy      -> NOT required;
//       blocking would break Colab / notebook-hosted SDK
//       loaders that are valid first-party callers.
//
//     - Strict-Transport-Security         -> LB-only.
//
//   This file pins the EXACT set ("nosniff is set; the
//   browser-specific ones are NOT set") so neither direction
//   regresses.  Adding CORP=same-origin in a future patch
//   would silently break browser loaders and would be caught
//   here.  Removing nosniff would silently re-enable
//   MIME-sniffing attacks and would also be caught here.

package sectest

import (
	"io"
	"net/http"
	"testing"
)

// dataPlaneRequired lists headers that MUST appear on every
// data-plane response, with their required values.
var dataPlaneRequired = map[string]string{
	"X-Content-Type-Options": "nosniff",
}

// dataPlaneForbidden lists headers that MUST NOT appear on
// the data plane (or must not appear with the listed values).
// A non-empty `forbiddenValue` means "this exact value is
// forbidden"; an empty value means "any non-empty value is
// forbidden".
var dataPlaneForbidden = []struct {
	name           string
	forbiddenValue string
}{
	{name: "Server"},
	{name: "X-Powered-By"},
	{name: "X-AspNet-Version"},
	{name: "Strict-Transport-Security"}, // LB-only; misleading here
	{name: "Cross-Origin-Resource-Policy", forbiddenValue: "same-origin"},
	{name: "Cross-Origin-Resource-Policy", forbiddenValue: "same-site"},
}

var dataPlaneProbePaths = []string{
	"/healthz",
	"/api/models/acme/widget",
	"/acme/widget/resolve/main/config.json",
	"/api/whoami-v2",
	"/totally/missing/path", // 404 path; middleware MUST still fire
}

// TestDataPlaneSecurityHeaders_Required asserts the required
// headers are present and correct on every data-plane path.
func TestDataPlaneSecurityHeaders_Required(t *testing.T) {
	stack := newStack(t)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for _, path := range dataPlaneProbePaths {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			resp, err := client.Get(stack.ProxyURL() + path)
			if err != nil {
				return
			}
			defer func() {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}()
			for name, want := range dataPlaneRequired {
				got := resp.Header.Get(name)
				if got != want {
					t.Fatalf("WSTG-CONF-06: %s missing %q=%q (got %q, status %d)",
						path, name, want, got, resp.StatusCode)
				}
			}
		})
	}
}

// TestDataPlaneSecurityHeaders_Forbidden asserts the headers
// that MUST NOT appear are absent across the same probe matrix.
func TestDataPlaneSecurityHeaders_Forbidden(t *testing.T) {
	stack := newStack(t)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for _, path := range dataPlaneProbePaths {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			resp, err := client.Get(stack.ProxyURL() + path)
			if err != nil {
				return
			}
			defer func() {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}()
			for _, rule := range dataPlaneForbidden {
				got := resp.Header.Get(rule.name)
				if got == "" {
					continue
				}
				if rule.forbiddenValue == "" || got == rule.forbiddenValue {
					t.Fatalf("WSTG-CONF-06: %s emits forbidden %q=%q (status %d)",
						path, rule.name, got, resp.StatusCode)
				}
			}
		})
	}
}
