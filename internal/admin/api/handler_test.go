// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/admin/api"
	"github.com/pulsys-io/pulsys/internal/admin/models"
	adminstore "github.com/pulsys-io/pulsys/internal/admin/store"
	"github.com/pulsys-io/pulsys/internal/auth"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/db"
	"github.com/pulsys-io/pulsys/internal/importer"
	"github.com/pulsys-io/pulsys/internal/testpg"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

type recordingAdminStore struct {
	auditAction   string
	auditResource string
	auditMeta     json.RawMessage
}

func (s *recordingAdminStore) InsertAudit(_ context.Context, _, _ string, _ *string, action, resource, _ string, metadata json.RawMessage, _, _ string) error {
	s.auditAction = action
	s.auditResource = resource
	s.auditMeta = metadata
	return nil
}

func adminActor(role auth.Role) context.Context {
	return auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: "tid1", UserID: "u1", Role: role,
	})
}

func writeGroupedFixture(t *testing.T, dir string) {
	t.Helper()
	writeMeta := func(key, path string, total int64) {
		root := filepath.Join(dir, "v1", "objects", key)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := map[string]any{
			"upstream_host": "huggingface.co",
			"path":          path,
			"status_code":   200,
			"total":         total,
		}
		b, _ := json.Marshal(meta)
		if err := os.WriteFile(filepath.Join(root, "meta.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeMeta("k1", "/Org/Model/resolve/main/a.bin", 100)
	writeMeta("k2", "/Org/Model/resolve/main/b.bin", 200)
	writeMeta("k3", "/Other/X/resolve/main/c.bin", 50)
}

func TestListModelsGrouped_OK(t *testing.T) {
	dir := t.TempDir()
	writeGroupedFixture(t, dir)
	h := &api.Handler{CacheDir: dir}
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/models/grouped?include_files=true", nil)
	req = req.WithContext(adminActor(auth.RoleReader))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listing models.GroupedListing
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Items) != 2 || listing.GrandTotalBytes != 350 {
		t.Fatalf("listing=%+v", listing)
	}
}

func TestListModelsGrouped_Forbidden(t *testing.T) {
	dir := t.TempDir()
	h := &api.Handler{CacheDir: dir}
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/models/grouped", nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorToken, TenantID: "tid1", TokenID: "t1", Scopes: []string{"models:read"},
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestCacheStats_OK(t *testing.T) {
	dir := t.TempDir()
	writeGroupedFixture(t, dir)
	store, err := cache.NewStoreWithOptions(dir, "none", cache.StoreOptions{MaxBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	h := &api.Handler{CacheDir: dir, Cache: store}
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/cache/stats", nil)
	req = req.WithContext(adminActor(auth.RoleReader))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		UsedBytes     int64 `json:"used_bytes"`
		QuotaBytes    int64 `json:"quota_bytes"`
		FreeDiskBytes int64 `json:"free_disk_bytes"`
		EntryCount    int   `json:"entry_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.UsedBytes != 350 || got.QuotaBytes != 1000 || got.EntryCount != 3 {
		t.Fatalf("stats=%+v", got)
	}
	if got.FreeDiskBytes == 0 {
		t.Fatalf("free_disk_bytes should be non-zero or -1, got %+v", got)
	}
}

func TestPurgeCacheByPrefix_OK(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	writeGroupedFixture(t, dir)
	st := &recordingAdminStore{}
	h := &api.Handler{CacheDir: dir, Cache: store, AuditInsert: st.InsertAudit}
	mux := http.NewServeMux()
	h.Mount(mux)

	body := strings.NewReader(`{"org":"Org","name":"Model"}`)
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/v1/models/cache", body)
	req = req.WithContext(adminActor(auth.RoleAdmin))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Purged     int   `json:"purged"`
		BytesFreed int64 `json:"bytes_freed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Purged != 2 || resp.BytesFreed != 300 {
		t.Fatalf("resp=%+v", resp)
	}
	if st.auditAction != "cache.purge" || st.auditResource != "Org/Model" {
		t.Fatalf("audit action=%q resource=%q", st.auditAction, st.auditResource)
	}
	if _, err := os.Stat(filepath.Join(dir, "v1", "objects", "k1")); !os.IsNotExist(err) {
		t.Fatal("k1 should be purged")
	}
	if _, err := os.Stat(filepath.Join(dir, "v1", "objects", "k3")); err != nil {
		t.Fatal("k3 should remain")
	}
}

func TestPurgeCacheByPrefix_Validation(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	h := &api.Handler{CacheDir: dir, Cache: store}
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/v1/models/cache", bytes.NewReader([]byte(`{"org":""}`)))
	req = req.WithContext(adminActor(auth.RoleAdmin))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestPurgeCacheByPrefix_ReaderForbidden(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	h := &api.Handler{CacheDir: dir, Cache: store}
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/v1/models/cache", bytes.NewReader([]byte(`{"org":"x","name":"y"}`)))
	req = req.WithContext(adminActor(auth.RoleReader))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}

// purgeResponse mirrors the JSON body of DELETE /admin/api/v1/models/cache.
// Lives here so all three regression sub-tests below can share a
// strict-decoder without sprinkling local structs.
type purgeResponse struct {
	Purged     int   `json:"purged"`
	Trimmed    int   `json:"trimmed"`
	BytesFreed int64 `json:"bytes_freed"`
}

func writeXetBody(t *testing.T, dir, key, originPath string, total int64) {
	t.Helper()
	root := filepath.Join(dir, "v1", "objects", key)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"upstream_host": "cas-bridge.xethub.hf.co",
		"path":          "/xet-bridge-us/" + key + "/sha",
		"origin_path":   originPath,
		"origin_paths":  []string{originPath},
		"status_code":   200,
		"total":         total,
	}
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(root, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "body"), bytes.Repeat([]byte("x"), int(total)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeXetBodyMultiOwner(t *testing.T, dir, key string, owners []string, total int64) {
	t.Helper()
	root := filepath.Join(dir, "v1", "objects", key)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"upstream_host": "cas-bridge.xethub.hf.co",
		"path":          "/xet-bridge-us/" + key + "/sha",
		"origin_path":   owners[0],
		"origin_paths":  owners,
		"status_code":   200,
		"total":         total,
	}
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(root, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "body"), bytes.Repeat([]byte("x"), int(total)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func doPurge(t *testing.T, mux *http.ServeMux, org, name string) (int, purgeResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/v1/models/cache",
		strings.NewReader(`{"org":"`+org+`","name":"`+name+`"}`))
	req = req.WithContext(adminActor(auth.RoleAdmin))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var resp purgeResponse
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	}
	return rec.Code, resp
}

// Regression for the original bug: a Xet/CAS body whose only owner
// is Qwen/Qwen2.5-0.5B (Path is an opaque content hash) must be
// removed when the user purges that model.  Before this change the
// predicate looked at Path only and silently skipped the weight.
func TestPurgeCacheByPrefix_SingleOwnerXetCascades(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	// Same-host JSON metadata (HF resolve-cache).
	writeGroupedFixture(t, dir)
	// Adjust the fixture path for Qwen so we can keep using the
	// shared helper's other rows as control.
	writeXetBody(t, dir, "kQwenWeights",
		"/Qwen/Qwen2.5-0.5B/resolve/main/model.safetensors", 988097824)
	// Plus a small same-host metadata row that should also cascade.
	writeXetBody(t, dir, "kQwenJSON",
		"/Qwen/Qwen2.5-0.5B/resolve/main/config.json", 1024)

	audit := &recordingAdminStore{}
	h := &api.Handler{CacheDir: dir, Cache: store, AuditInsert: audit.InsertAudit}
	mux := http.NewServeMux()
	h.Mount(mux)

	code, resp := doPurge(t, mux, "Qwen", "Qwen2.5-0.5B")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if resp.Purged != 2 || resp.Trimmed != 0 || resp.BytesFreed != 988097824+1024 {
		t.Fatalf("resp=%+v", resp)
	}
	for _, key := range []string{"kQwenWeights", "kQwenJSON"} {
		if _, err := os.Stat(filepath.Join(dir, "v1", "objects", key)); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone", key)
		}
	}
	// Unrelated fixture rows must survive.
	for _, key := range []string{"k1", "k2", "k3"} {
		if _, err := os.Stat(filepath.Join(dir, "v1", "objects", key)); err != nil {
			t.Fatalf("%s should remain: %v", key, err)
		}
	}
}

// Shared Xet body owned by two HF repos.  Purging one owner must
// rewrite meta (Trim) and keep the body for the other owner.
func TestPurgeCacheByPrefix_SharedBodyTrims(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	writeXetBodyMultiOwner(t, dir, "kShared", []string{
		"/A/M/resolve/main/x.bin",
		"/B/M/resolve/main/x.bin",
	}, 1024)

	h := &api.Handler{CacheDir: dir, Cache: store, AuditInsert: (&recordingAdminStore{}).InsertAudit}
	mux := http.NewServeMux()
	h.Mount(mux)

	code, resp := doPurge(t, mux, "A", "M")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if resp.Purged != 0 || resp.Trimmed != 1 || resp.BytesFreed != 0 {
		t.Fatalf("resp=%+v", resp)
	}
	if _, err := os.Stat(filepath.Join(dir, "v1", "objects", "kShared", "body")); err != nil {
		t.Fatalf("shared body should still exist: %v", err)
	}
	meta, err := store.LoadMeta("kShared")
	if err != nil || meta == nil {
		t.Fatalf("kShared meta missing: %v %v", meta, err)
	}
	if meta.OriginPath != "/B/M/resolve/main/x.bin" {
		t.Fatalf("origin_path=%q want promoted to B", meta.OriginPath)
	}
	if len(meta.OriginPaths) != 1 || meta.OriginPaths[0] != "/B/M/resolve/main/x.bin" {
		t.Fatalf("origin_paths=%v want [B]", meta.OriginPaths)
	}
}

// Once both owners are gone the body itself should be removed.
// Verifies the trim-then-remove progression survives across two
// separate purge calls (the realistic operator workflow).
func TestPurgeCacheByPrefix_LastOwnerRemoves(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	writeXetBodyMultiOwner(t, dir, "kShared", []string{
		"/A/M/resolve/main/x.bin",
		"/B/M/resolve/main/x.bin",
	}, 2048)

	h := &api.Handler{CacheDir: dir, Cache: store, AuditInsert: (&recordingAdminStore{}).InsertAudit}
	mux := http.NewServeMux()
	h.Mount(mux)

	if code, _ := doPurge(t, mux, "A", "M"); code != http.StatusOK {
		t.Fatalf("first purge status=%d", code)
	}
	code, resp := doPurge(t, mux, "B", "M")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if resp.Purged != 1 || resp.Trimmed != 0 || resp.BytesFreed != 2048 {
		t.Fatalf("resp=%+v", resp)
	}
	if _, err := os.Stat(filepath.Join(dir, "v1", "objects", "kShared")); !os.IsNotExist(err) {
		t.Fatal("kShared should be gone after both owners purged")
	}
}

func TestImports_CreateListGet(t *testing.T) {
	adminSt, _, tenantID, userID := testImportAPIStore(t)
	audit := &recordingAdminStore{}
	h := &api.Handler{Store: adminSt, AuditInsert: audit.InsertAudit}
	mux := http.NewServeMux()
	h.Mount(mux)

	body := bytes.NewReader([]byte(`{"repo_id":"Qwen/Qwen2.5-0.5B","revision":"main"}`))
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/imports", body)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tenantID, UserID: userID, Role: auth.RoleAdmin,
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created adminstore.ImportJob
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Status != adminstore.ImportJobQueued {
		t.Fatalf("created=%+v", created)
	}
	if audit.auditAction != "import.create" || audit.auditResource != "imports/"+created.ID {
		t.Fatalf("audit action=%q resource=%q", audit.auditAction, audit.auditResource)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/v1/imports?limit=10", nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tenantID, UserID: userID, Role: auth.RoleReader,
	}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Items []adminstore.ImportJob `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 || listed.Items[0].ID != created.ID {
		t.Fatalf("listed=%+v", listed.Items)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/v1/imports/"+created.ID, nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tenantID, UserID: userID, Role: auth.RoleReader,
	}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got adminstore.ImportJob
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID {
		t.Fatalf("got id=%q want %q", got.ID, created.ID)
	}
}

func TestImports_CreateValidationAndForbidden(t *testing.T) {
	adminSt, _, tenantID, userID := testImportAPIStore(t)
	h := &api.Handler{Store: adminSt}
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/imports", bytes.NewReader([]byte(`{"repo_id":"../bad"}`)))
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tenantID, UserID: userID, Role: auth.RoleAdmin,
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("validation status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/api/v1/imports", bytes.NewReader([]byte(`{"repo_id":"Qwen/Qwen2.5-0.5B"}`)))
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tenantID, UserID: userID, Role: auth.RoleReader,
	}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reader create status=%d", rec.Code)
	}
}

// TestImports_CancelAndDelete covers the cancel + delete admin
// endpoints.  We exercise the four interesting paths:
//
//  1. Cancel a queued job: River's JobCancel is idempotent and we
//     surface a successful 204 along with an "import.cancel" audit
//     row.
//  2. Delete a queued job: any non-running terminal-or-queued state
//     is removable; River's JobDelete returns the row.
//  3. Cross-tenant delete: another tenant's PAT must see 404, not
//     a 204, even though it has admin role.  This is the same
//     scoping invariant ListImportJobs uses.
//  4. Delete by id of a non-existent job: 404.
//
// The "delete running -> 409" path is covered by a dedicated test
// below that needs a live worker to put the job in Running state.
func TestImports_CancelAndDelete(t *testing.T) {
	adminSt, authSt, tenantID, userID := testImportAPIStore(t)
	audit := &recordingAdminStore{}
	h := &api.Handler{Store: adminSt, AuditInsert: audit.InsertAudit}
	mux := http.NewServeMux()
	h.Mount(mux)

	ctx := context.Background()
	created, err := adminSt.CreateImportJob(ctx, tenantID, userID, adminstore.ImportJobTypeHFCacheImport,
		json.RawMessage(`{"repo_id":"Qwen/Qwen2.5-0.5B"}`))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/imports/"+created.ID+"/cancel", nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tenantID, UserID: userID, Role: auth.RoleAdmin,
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	if audit.auditAction != "import.cancel" || audit.auditResource != "imports/"+created.ID {
		t.Fatalf("cancel audit action=%q resource=%q", audit.auditAction, audit.auditResource)
	}

	deleted, err := adminSt.CreateImportJob(ctx, tenantID, userID, adminstore.ImportJobTypeHFCacheImport,
		json.RawMessage(`{"repo_id":"openai-community/gpt2"}`))
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodDelete, "/admin/api/v1/imports/"+deleted.ID, nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tenantID, UserID: userID, Role: auth.RoleAdmin,
	}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if audit.auditAction != "import.delete" || audit.auditResource != "imports/"+deleted.ID {
		t.Fatalf("delete audit action=%q resource=%q", audit.auditAction, audit.auditResource)
	}

	if _, err := adminSt.GetImportJob(ctx, tenantID, deleted.ID); err == nil {
		t.Fatal("expected deleted job to be gone")
	}

	otherTID, err := authSt.EnsureTenant(ctx, "import-cancel-other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	otherUID, err := authSt.CreateUserOIDC(ctx, auth.User{
		TenantID: otherTID, Email: "other@test.local", DisplayName: "Other",
		Role: auth.RoleAdmin, OIDCSub: "import-cancel-other-sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	mineForCross, err := adminSt.CreateImportJob(ctx, tenantID, userID, adminstore.ImportJobTypeHFCacheImport,
		json.RawMessage(`{"repo_id":"Qwen/Qwen2.5-0.5B"}`))
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodDelete, "/admin/api/v1/imports/"+mineForCross.ID, nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: otherTID, UserID: otherUID, Role: auth.RoleAdmin,
	}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/admin/api/v1/imports/9999999", nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tenantID, UserID: userID, Role: auth.RoleAdmin,
	}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing delete status=%d", rec.Code)
	}
}

// insertOnlyDownloader satisfies the importer.Downloader interface
// for insert-only test clients.  Workers are registered solely so
// River can validate that inserted job kinds match a known worker;
// these clients never call Start, so Download is never invoked.
type insertOnlyDownloader struct{}

func (insertOnlyDownloader) Download(context.Context, importer.ImportSpec, func(importer.ImportProgress)) error {
	return nil
}

// blockingDownloader holds the worker in Running state until the
// test signals release.  We use it to construct the exact race
// the admin UI exposes: the user clicks Delete on a job whose
// row has already been claimed by the worker.  The contract is a
// 409, not silent corruption.
type blockingDownloader struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingDownloader) Download(ctx context.Context, _ importer.ImportSpec, _ func(importer.ImportProgress)) error {
	close(b.started)
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// failingDownloader always errors so River transitions the job
// into the Retryable state after the worker finishes its attempt.
type failingDownloader struct {
	failed chan struct{}
	once   bool
}

func (f *failingDownloader) Download(_ context.Context, _ importer.ImportSpec, _ func(importer.ImportProgress)) error {
	if !f.once {
		f.once = true
		close(f.failed)
	}
	return errors.New("synthetic failure")
}

// longBackoffRetryPolicy parks failed jobs 1 hour in the future so
// they sit in Retryable state long enough for the test to observe
// them and exercise DeleteImportJob.  Without this we'd race
// against River's default ~1s backoff and the job would flip back
// into Running before we could call Delete.
type longBackoffRetryPolicy struct{}

func (longBackoffRetryPolicy) NextRetry(_ *rivertype.JobRow) time.Time {
	return time.Now().Add(time.Hour)
}

func TestImports_DeleteRunning_Conflict(t *testing.T) {
	pool := testpg.Acquire(t)
	ctx := context.Background()
	if err := db.MigrateRiverPool(ctx, pool); err != nil {
		t.Fatal(err)
	}

	dl := &blockingDownloader{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, &importer.CacheImportWorker{
		Downloader:    dl,
		ProgressEvery: time.Millisecond,
	})
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers: workers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(dl.release)
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})

	adminSt := adminstore.NewAdminStore(pool)
	adminSt.SetRiverClient(client)
	authSt := authstore.NewPG(pool)
	tid, err := authSt.EnsureTenant(ctx, "import-delete-running", "Delete Running")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := authSt.CreateUserOIDC(ctx, auth.User{
		TenantID: tid, Email: "delete-running@test.local", DisplayName: "DR",
		Role: auth.RoleAdmin, OIDCSub: "import-delete-running-sub",
	})
	if err != nil {
		t.Fatal(err)
	}

	created, err := adminSt.CreateImportJob(ctx, tid, uid, adminstore.ImportJobTypeHFCacheImport,
		json.RawMessage(`{"repo_id":"Qwen/Qwen2.5-0.5B"}`))
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-dl.started:
	case <-time.After(10 * time.Second):
		t.Fatal("worker never started")
	}

	h := &api.Handler{Store: adminSt}
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/v1/imports/"+created.ID, nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tid, UserID: uid, Role: auth.RoleAdmin,
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete running status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestImports_DeleteRetryable_OK is the regression test for the
// dashboard bug "I can't remove a job that errored though.  There
// is no option because it's in a running state."  River parks a
// failed-but-not-yet-discarded job in the Retryable state.  Before
// the fix we mapped Retryable to ImportJobRunning and the admin UI
// hid Delete; we now expose it as ImportJobRetrying and allow
// Delete, since the row is not actually held by the worker.
func TestImports_DeleteRetryable_OK(t *testing.T) {
	pool := testpg.Acquire(t)
	ctx := context.Background()
	if err := db.MigrateRiverPool(ctx, pool); err != nil {
		t.Fatal(err)
	}

	dl := &failingDownloader{failed: make(chan struct{})}
	workers := river.NewWorkers()
	river.AddWorker(workers, &importer.CacheImportWorker{
		Downloader:    dl,
		ProgressEvery: time.Millisecond,
	})
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:      map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:     workers,
		RetryPolicy: longBackoffRetryPolicy{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})

	adminSt := adminstore.NewAdminStore(pool)
	adminSt.SetRiverClient(client)
	authSt := authstore.NewPG(pool)
	tid, err := authSt.EnsureTenant(ctx, "import-delete-retryable", "Delete Retryable")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := authSt.CreateUserOIDC(ctx, auth.User{
		TenantID: tid, Email: "delete-retryable@test.local", DisplayName: "DR",
		Role: auth.RoleAdmin, OIDCSub: "import-delete-retryable-sub",
	})
	if err != nil {
		t.Fatal(err)
	}

	created, err := adminSt.CreateImportJob(ctx, tid, uid, adminstore.ImportJobTypeHFCacheImport,
		json.RawMessage(`{"repo_id":"Qwen/Qwen2.5-0.5B"}`))
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-dl.failed:
	case <-time.After(10 * time.Second):
		t.Fatal("worker never executed first attempt")
	}

	// Worker has returned an error.  Poll until River writes the
	// Retryable state (the failed-attempt finalization runs in
	// the worker goroutine just after Download returns).
	var observed adminstore.ImportJobStatus
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := adminSt.GetImportJob(ctx, tid, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		observed = got.Status
		if got.Status == adminstore.ImportJobRetrying {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if observed != adminstore.ImportJobRetrying {
		t.Fatalf("expected retrying status, got %q", observed)
	}

	h := &api.Handler{Store: adminSt}
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/v1/imports/"+created.ID, nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tid, UserID: uid, Role: auth.RoleAdmin,
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete retryable status=%d body=%s", rec.Code, rec.Body.String())
	}

	if _, err := adminSt.GetImportJob(ctx, tid, created.ID); !errors.Is(err, adminstore.ErrImportJobNotFound) {
		t.Fatalf("expected ErrImportJobNotFound after delete, got %v", err)
	}
}

// TestImports_ForceCancel exercises the admin escape hatch for
// stuck rows. The handler must:
//
//  1. Force-cancel a queued row (the trivial path: flips state and
//     returns 204 with an `import.force_cancel` audit entry).
//  2. Reject cross-tenant force-cancel with 404 -- River's job ids
//     are global integers and must not leak across tenants.
//  3. Return 404 for missing ids.
//
// The orphaned-running-row scenario is covered end-to-end by
// TestImportJobs_ForceCancel_OrphanedRunning in the store tests
// (it manipulates river_job.state directly, which is the only way
// to repro without a real wedged worker).
func TestImports_ForceCancel(t *testing.T) {
	adminSt, authSt, tenantID, userID := testImportAPIStore(t)
	audit := &recordingAdminStore{}
	h := &api.Handler{Store: adminSt, AuditInsert: audit.InsertAudit}
	mux := http.NewServeMux()
	h.Mount(mux)

	ctx := context.Background()
	created, err := adminSt.CreateImportJob(ctx, tenantID, userID, adminstore.ImportJobTypeHFCacheImport,
		json.RawMessage(`{"repo_id":"Qwen/Qwen2.5-0.5B"}`))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/imports/"+created.ID+"/force-cancel", nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tenantID, UserID: userID, Role: auth.RoleAdmin,
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("force-cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	if audit.auditAction != "import.force_cancel" || audit.auditResource != "imports/"+created.ID {
		t.Fatalf("force-cancel audit action=%q resource=%q, want import.force_cancel imports/%s",
			audit.auditAction, audit.auditResource, created.ID)
	}

	got, err := adminSt.GetImportJob(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != adminstore.ImportJobCanceled {
		t.Fatalf("after force-cancel status=%q want canceled", got.Status)
	}

	otherTID, err := authSt.EnsureTenant(ctx, "import-force-other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	otherUID, err := authSt.CreateUserOIDC(ctx, auth.User{
		TenantID: otherTID, Email: "force-other@test.local", DisplayName: "Other",
		Role: auth.RoleAdmin, OIDCSub: "import-force-other-sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	mineForCross, err := adminSt.CreateImportJob(ctx, tenantID, userID, adminstore.ImportJobTypeHFCacheImport,
		json.RawMessage(`{"repo_id":"Qwen/Qwen2.5-0.5B"}`))
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/admin/api/v1/imports/"+mineForCross.ID+"/force-cancel", nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: otherTID, UserID: otherUID, Role: auth.RoleAdmin,
	}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant force-cancel status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/api/v1/imports/9999999/force-cancel", nil)
	req = req.WithContext(auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tenantID, UserID: userID, Role: auth.RoleAdmin,
	}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing force-cancel status=%d", rec.Code)
	}
}

// TestImports_ForceCancel_StuckRunning is the end-to-end repro for
// the production bug "job stuck at running, can't cancel, can't
// delete." We:
//
//  1. Insert a job through the normal admin API.
//  2. Manually flip its river_job.state to 'running' (simulates the
//     row left behind by a previous proxy process; reproducible by
//     decreasing the job timeout so a worker is killed mid-flight,
//     then killing the proxy before the row finalizes).
//  3. POST /cancel -- River signals (no worker subscribed), 204
//     returned, row stays running. This matches the user's report:
//     "Status Code 204 No Content" but no visible state change.
//  4. DELETE -- River refuses; 409.
//  5. POST /force-cancel -- the escape hatch flips state to
//     canceled and DELETE then succeeds.
func TestImports_ForceCancel_StuckRunning(t *testing.T) {
	adminSt, _, tenantID, userID := testImportAPIStore(t)
	audit := &recordingAdminStore{}
	h := &api.Handler{Store: adminSt, AuditInsert: audit.InsertAudit}
	mux := http.NewServeMux()
	h.Mount(mux)

	ctx := context.Background()
	created, err := adminSt.CreateImportJob(ctx, tenantID, userID, adminstore.ImportJobTypeHFCacheImport,
		json.RawMessage(`{"repo_id":"Qwen/Qwen2.5-0.5B"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminSt.Pool.Exec(ctx, `
		UPDATE river_job
		SET state = 'running'::river_job_state, attempted_at = now(), attempt = 1
		WHERE id = $1
	`, created.ID); err != nil {
		t.Fatal(err)
	}

	actor := auth.ContextWithActor(context.Background(), auth.Actor{
		Type: auth.ActorUser, TenantID: tenantID, UserID: userID, Role: auth.RoleAdmin,
	})

	// Step 3: regular cancel succeeds (this is the user-reported 204)
	// but the row stays running because there's no worker subscribed.
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/imports/"+created.ID+"/cancel", nil).WithContext(actor)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	stuck, err := adminSt.GetImportJob(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stuck.Status != adminstore.ImportJobRunning {
		t.Fatalf("after cancel status=%q want still running (orphan repro)", stuck.Status)
	}
	if stuck.CancelRequestedAt == nil {
		t.Fatal("after cancel CancelRequestedAt should be populated so UI shows Canceling…")
	}

	// Step 4: delete refuses with 409.
	req = httptest.NewRequest(http.MethodDelete, "/admin/api/v1/imports/"+created.ID, nil).WithContext(actor)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete running status=%d body=%s want 409", rec.Code, rec.Body.String())
	}

	// Step 5: force-cancel is the escape hatch. After it, delete
	// succeeds.
	req = httptest.NewRequest(http.MethodPost, "/admin/api/v1/imports/"+created.ID+"/force-cancel", nil).WithContext(actor)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("force-cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	if audit.auditAction != "import.force_cancel" {
		t.Fatalf("audit action=%q want import.force_cancel", audit.auditAction)
	}
	cleaned, err := adminSt.GetImportJob(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.Status != adminstore.ImportJobCanceled {
		t.Fatalf("after force-cancel status=%q want canceled", cleaned.Status)
	}

	req = httptest.NewRequest(http.MethodDelete, "/admin/api/v1/imports/"+created.ID, nil).WithContext(actor)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete after force-cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func testImportAPIStore(t *testing.T) (*adminstore.AdminStore, *authstore.PG, string, string) {
	t.Helper()
	pool := testpg.Acquire(t)
	ctx := context.Background()
	if err := db.MigrateRiverPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Insert-only River client: no Queues / no Start.  Cancel + Delete + Get
	// + List + Insert all hit the database directly through the driver, so
	// they work without a running worker; this matches River v0.38's
	// "insert-only" mode documented at https://riverqueue.com/docs/insert-only-clients
	// and avoids the MaxWorkers >= 1 validation that v0.38 added.
	workers := river.NewWorkers()
	river.AddWorker(workers, &importer.CacheImportWorker{Downloader: insertOnlyDownloader{}})
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Workers: workers})
	if err != nil {
		t.Fatal(err)
	}
	adminSt := adminstore.NewAdminStore(pool)
	adminSt.SetRiverClient(client)
	authSt := authstore.NewPG(pool)
	tid, err := authSt.EnsureTenant(ctx, "import-api-test", "Import API Test")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := authSt.CreateUserOIDC(ctx, auth.User{
		TenantID:    tid,
		Email:       "import-api@test.local",
		DisplayName: "Import Admin",
		Role:        auth.RoleAdmin,
		OIDCSub:     "import-api-sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	return adminSt, authSt, tid, uid
}
