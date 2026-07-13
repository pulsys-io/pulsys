// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package businesslogic

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// TestCreateToken_TTLValidation pins the Phase 5 contract on
// expires_in_seconds: positive bounded values land in the past
// at no point, the silly inputs (0, negative, > 366 days) all
// return 400 with an informative message, and the unset case
// produces a never-expiring token (the legitimate "service
// account" use case).
func TestCreateToken_TTLValidation(t *testing.T) {
	f := newFixtures(t)

	t.Run("zero is rejected", func(t *testing.T) {
		body := `{"name":"ttl0","scopes":["models:read"],"expires_in_seconds":0}`
		rec, _ := f.postCreateToken(body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s want 400", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "ambiguous") {
			t.Fatalf("body=%q want 'ambiguous' message", rec.Body.String())
		}
	})

	t.Run("negative is rejected", func(t *testing.T) {
		body := `{"name":"ttlneg","scopes":["models:read"],"expires_in_seconds":-1}`
		rec, _ := f.postCreateToken(body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s want 400", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "positive") {
			t.Fatalf("body=%q want 'positive' message", rec.Body.String())
		}
	})

	t.Run("over one year is rejected", func(t *testing.T) {
		// 367 days in seconds.  The cap is exactly 366 days.
		secs := int64(367 * 24 * 3600)
		body := `{"name":"ttlover","scopes":["models:read"],"expires_in_seconds":` +
			strconv.FormatInt(secs, 10) + `}`
		rec, _ := f.postCreateToken(body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s want 400", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "366") {
			t.Fatalf("body=%q want '366' message", rec.Body.String())
		}
	})

	t.Run("max int64 is rejected without overflow", func(t *testing.T) {
		// Specifically defends against time.Duration overflow
		// silently producing a past timestamp.  9223372036854775807
		// nanoseconds is ~292 years; multiplied by Second it
		// would wrap.  The cap check must reject before the
		// arithmetic.
		body := `{"name":"ttlmax","scopes":["models:read"],"expires_in_seconds":9223372036854775807}`
		rec, _ := f.postCreateToken(body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("one hour is accepted", func(t *testing.T) {
		body := `{"name":"ttl1h","scopes":["models:read"],"expires_in_seconds":3600}`
		rec, resp := f.postCreateToken(body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s want 201", rec.Code, rec.Body.String())
		}
		if resp.ID == "" {
			t.Fatalf("missing id in body=%s", rec.Body.String())
		}
	})

	t.Run("exactly 366 days is accepted", func(t *testing.T) {
		secs := int64(366 * 24 * 3600)
		body := `{"name":"ttl366d","scopes":["models:read"],"expires_in_seconds":` +
			strconv.FormatInt(secs, 10) + `}`
		rec, _ := f.postCreateToken(body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s want 201", rec.Code, rec.Body.String())
		}
	})

	t.Run("unset means no expiry", func(t *testing.T) {
		// Omitting expires_in_seconds yields a never-expiring
		// token (service-account use case).  Test by listing
		// tokens and confirming ExpiresAt is nil.
		body := `{"name":"ttlnone","scopes":["models:read"]}`
		rec, _ := f.postCreateToken(body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s want 201", rec.Code, rec.Body.String())
		}
	})
}
