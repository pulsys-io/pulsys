// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package observability_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pulsys-io/pulsys/internal/observability"
)

func TestMetricsHandler(t *testing.T) {
	reg := observability.NewRegistry()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pulsys_cache_hits") {
		t.Fatalf("missing counter: %s", body)
	}
}

func TestHealthHandlerNoDB(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	observability.HealthHandler(nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}
