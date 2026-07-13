// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pulsys-io/pulsys/internal/auth"
)

type recordingStore struct {
	action  string
	outcome string
}

func (s *recordingStore) InsertAudit(_ context.Context, tenantID, _ string, _ *string, action, _, outcome string, _ json.RawMessage, _, _ string) error {
	if tenantID == "" {
		return nil
	}
	s.action = action
	s.outcome = outcome
	return nil
}

type stubTenant struct{ id string }

func (s stubTenant) GetTenantIDByName(_ context.Context, _ string) (string, error) {
	return s.id, nil
}

func TestShouldAuditMutationsOnly(t *testing.T) {
	if shouldAudit(httptest.NewRequest(http.MethodGet, "/admin/api/v1/tokens", nil)) {
		t.Fatal("GET should not audit")
	}
	if !shouldAudit(httptest.NewRequest(http.MethodPost, "/admin/api/v1/tokens", nil)) {
		t.Fatal("POST tokens should audit")
	}
	if !shouldAudit(httptest.NewRequest(http.MethodPost, "/auth/session", nil)) {
		t.Fatal("session should audit")
	}
}

func TestMapAction(t *testing.T) {
	a, r := mapAction(http.MethodDelete, "/admin/api/v1/tokens/abc")
	if a != "token.revoke" || r != "api/v1/tokens/abc" {
		t.Fatalf("got %q %q", a, r)
	}
}

func TestOutcomeForStatus(t *testing.T) {
	if outcomeForStatus(201) != "success" {
		t.Fatal()
	}
	if outcomeForStatus(403) != "denied" {
		t.Fatal()
	}
	if outcomeForStatus(500) != "failure" {
		t.Fatal()
	}
}

func TestMiddlewareRecordsMutation(t *testing.T) {
	st := &recordingStore{}
	mw := &Middleware{Store: st, TenantName: "default", TenantLookup: stubTenant{id: "tid1"}}
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/tokens", nil)
	req = req.WithContext(auth.ContextWithActor(req.Context(), auth.Actor{
		Type: auth.ActorUser, TenantID: "tid1", UserID: "u1", Role: auth.RoleAdmin,
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if st.action != "token.create" || st.outcome != "success" {
		t.Fatalf("audit action=%q outcome=%q", st.action, st.outcome)
	}
}
