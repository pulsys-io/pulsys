// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/pulsys-io/pulsys/internal/blobstore"
	"github.com/pulsys-io/pulsys/internal/hfhub/fixtures"
	"github.com/pulsys-io/pulsys/internal/registry"
	"github.com/pulsys-io/pulsys/internal/testpg"
	"github.com/pulsys-io/pulsys/internal/testserver"
)

// uploadEnv extends registryTestEnv with an AuditPool wired into the
// stack so commit handlers can assert audit_log rows.
func uploadEnv(t *testing.T) *registryTestEnv {
	t.Helper()
	pool := testpg.Acquire(t)
	var tenantID string
	if err := pool.QueryRow(context.Background(), `
INSERT INTO tenants (name, display_name) VALUES ('default', 'Default')
RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	reg := registry.New(pool)
	blobs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stack := testserver.New(t, testserver.Config{
		Registry: &testserver.RegistryConfig{
			Store:       reg,
			Blobs:       blobs,
			TenantID:    tenantID,
			AuditExecer: pool,
		},
	})
	return &registryTestEnv{
		stack:    stack,
		reg:      reg,
		blobs:    blobs,
		tenantID: tenantID,
		mockHub:  stack.Mock,
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestUpload_CreateRepo(t *testing.T) {
	env := uploadEnv(t)
	body, _ := json.Marshal(map[string]any{
		"name":    "acme/widget",
		"type":    "model",
		"private": false,
	})
	resp, err := http.Post(env.stack.ProxyURL()+"/api/repos/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	// exist_ok behavior: second POST returns 200 with the same name.
	resp2, err := http.Post(env.stack.ProxyURL()+"/api/repos/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("duplicate create status=%d want 200", resp2.StatusCode)
	}
}

func TestUpload_Preupload_DedupAndLFSThreshold(t *testing.T) {
	env := uploadEnv(t)
	env.seedRegistryRepo(t, "acme", "widget", map[string][]byte{
		"config.json": []byte("{}"),
	})

	body, _ := json.Marshal(map[string]any{
		"files": []map[string]any{
			{"path": "config.json", "size": 2},    // dedup hit
			{"path": "README.md", "size": 12},     // new, regular
			{"path": "small.bin", "size": 4096},   // .bin -> lfs by extension
			{"path": "big.txt", "size": 32 << 20}, // over threshold -> lfs
		},
	})
	resp, err := http.Post(env.stack.ProxyURL()+"/api/models/acme/widget/preupload/main",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var pr struct {
		Files []struct {
			Path         string `json:"path"`
			UploadMode   string `json:"uploadMode"`
			ShouldIgnore bool   `json:"shouldIgnore"`
		} `json:"files"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	got := map[string]struct {
		mode    string
		ignored bool
	}{}
	for _, f := range pr.Files {
		got[f.Path] = struct {
			mode    string
			ignored bool
		}{f.UploadMode, f.ShouldIgnore}
	}
	for path, want := range map[string]struct {
		mode    string
		ignored bool
	}{
		"config.json": {"regular", true},
		"README.md":   {"regular", false},
		"small.bin":   {"lfs", false},
		"big.txt":     {"lfs", false},
	} {
		if g := got[path]; g != want {
			t.Errorf("path=%s got=%+v want=%+v", path, g, want)
		}
	}
}

func TestUpload_CommitInlineFiles_AtomicAndQueryable(t *testing.T) {
	env := uploadEnv(t)
	repo, err := env.reg.CreateRepo(context.Background(), env.tenantID, "models", "acme", "widget", false, "")
	if err != nil {
		t.Fatal(err)
	}

	var nd bytes.Buffer
	mustNDJSON(t, &nd, map[string]any{"key": "header", "value": map[string]string{"summary": "Initial commit"}})
	mustNDJSON(t, &nd, map[string]any{"key": "file", "value": map[string]string{
		"path": "config.json", "encoding": "utf-8", "content": string(fixtures.ConfigJSON()),
	}})
	mustNDJSON(t, &nd, map[string]any{"key": "file", "value": map[string]string{
		"path": "README.md", "encoding": "utf-8", "content": "# acme/widget",
	}})
	resp, err := http.Post(env.stack.ProxyURL()+"/api/models/acme/widget/commit/main",
		"application/x-ndjson", &nd)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("commit status=%d body=%s", resp.StatusCode, out)
	}
	var cr struct {
		Success   bool   `json:"success"`
		CommitOID string `json:"commitOid"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	if !cr.Success || cr.CommitOID == "" {
		t.Fatalf("bad commit response: %+v", cr)
	}

	files, _, _ := env.reg.ListFiles(context.Background(), repo.ID, cr.CommitOID)
	if len(files) != 2 {
		t.Fatalf("want 2 files got %d", len(files))
	}
	if files["config.json"].IsLFS {
		t.Fatal("inline file flagged as LFS")
	}
	if files["config.json"].BlobOID != sha256Hex(fixtures.ConfigJSON()) {
		t.Fatalf("config.json oid mismatch: %s", files["config.json"].BlobOID)
	}

	// Audit row written.
	var auditCount int
	if err := env.queryAuditCount(t, "upload.commit", &auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("audit count=%d want 1", auditCount)
	}
}

func TestUpload_FiftyFilesInOneCommit(t *testing.T) {
	env := uploadEnv(t)
	if _, err := env.reg.CreateRepo(context.Background(), env.tenantID, "models", "acme", "bulk", false, ""); err != nil {
		t.Fatal(err)
	}

	var nd bytes.Buffer
	mustNDJSON(t, &nd, map[string]any{"key": "header", "value": map[string]string{"summary": "bulk add"}})
	for i := 0; i < 50; i++ {
		mustNDJSON(t, &nd, map[string]any{"key": "file", "value": map[string]string{
			"path":     fmt.Sprintf("file_%02d.txt", i),
			"encoding": "utf-8",
			"content":  fmt.Sprintf("content of file %d", i),
		}})
	}
	resp, err := http.Post(env.stack.ProxyURL()+"/api/models/acme/bulk/commit/main",
		"application/x-ndjson", &nd)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}

	// Verify the 50 files round-trip via the read path.
	r2, err := http.Get(env.stack.ProxyURL() + "/api/models/acme/bulk/tree/main")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var entries []map[string]any
	_ = json.NewDecoder(r2.Body).Decode(&entries)
	if len(entries) != 50 {
		t.Fatalf("tree entries=%d want 50", len(entries))
	}
}

func TestUpload_LFSRoundTrip(t *testing.T) {
	env := uploadEnv(t)
	if _, err := env.reg.CreateRepo(context.Background(), env.tenantID, "models", "acme", "widget", false, ""); err != nil {
		t.Fatal(err)
	}

	payload := bytes.Repeat([]byte{0xAB, 0xCD}, 1<<15) // 64 KiB
	oid := sha256Hex(payload)

	// 1. LFS batch (upload mode) -> presigned URL.
	batchBody, _ := json.Marshal(map[string]any{
		"operation": "upload",
		"transfers": []string{"basic"},
		"objects":   []map[string]any{{"oid": oid, "size": len(payload)}},
	})
	resp, err := http.Post(env.stack.ProxyURL()+"/acme/widget.git/info/lfs/objects/batch",
		"application/vnd.git-lfs+json", bytes.NewReader(batchBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("batch status=%d", resp.StatusCode)
	}
	var bresp struct {
		Objects []struct {
			Actions map[string]struct {
				Href string `json:"href"`
			} `json:"actions"`
		} `json:"objects"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&bresp)
	if len(bresp.Objects) != 1 || bresp.Objects[0].Actions["upload"].Href == "" {
		t.Fatalf("batch response missing upload URL: %+v", bresp)
	}
	uploadURL := bresp.Objects[0].Actions["upload"].Href
	verifyURL := bresp.Objects[0].Actions["verify"].Href
	if verifyURL == "" {
		t.Fatal("missing verify URL")
	}

	// 2. PUT the bytes - this is the multi-GB-safe streaming path.
	req, _ := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(payload))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("PUT status=%d", resp2.StatusCode)
	}

	// 3. verify
	vBody, _ := json.Marshal(map[string]any{"oid": oid, "size": len(payload)})
	resp3, err := http.Post(verifyURL, "application/vnd.git-lfs+json", bytes.NewReader(vBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("verify status=%d", resp3.StatusCode)
	}

	// 4. commit referencing the LFS pointer
	var nd bytes.Buffer
	mustNDJSON(t, &nd, map[string]any{"key": "header", "value": map[string]string{"summary": "add weights"}})
	mustNDJSON(t, &nd, map[string]any{"key": "lfsFile", "value": map[string]any{
		"path": "model.safetensors", "oid": oid, "size": len(payload), "algo": "sha256",
	}})
	resp4, err := http.Post(env.stack.ProxyURL()+"/api/models/acme/widget/commit/main",
		"application/x-ndjson", &nd)
	if err != nil {
		t.Fatal(err)
	}
	defer resp4.Body.Close()
	if resp4.StatusCode != 200 {
		out, _ := io.ReadAll(resp4.Body)
		t.Fatalf("commit status=%d body=%s", resp4.StatusCode, out)
	}

	// 5. read back via resolve
	resp5, err := http.Get(env.stack.ProxyURL() + "/acme/widget/resolve/main/model.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	defer resp5.Body.Close()
	if resp5.StatusCode != 200 {
		t.Fatalf("resolve status=%d", resp5.StatusCode)
	}
	got, _ := io.ReadAll(resp5.Body)
	if !bytes.Equal(got, payload) {
		t.Fatalf("body mismatch (got %d, want %d)", len(got), len(payload))
	}
}

func TestUpload_LFSPut_OIDMismatch_422(t *testing.T) {
	env := uploadEnv(t)
	if _, err := env.reg.CreateRepo(context.Background(), env.tenantID, "models", "acme", "widget", false, ""); err != nil {
		t.Fatal(err)
	}

	bogus := strings.Repeat("a", 64)
	req, _ := http.NewRequest(http.MethodPut,
		env.stack.ProxyURL()+"/lfs-storage/"+bogus,
		bytes.NewReader([]byte("definitely not matching the oid")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422", resp.StatusCode)
	}
}

func TestUpload_LFSBatch_AlreadyUploaded_NoActions(t *testing.T) {
	env := uploadEnv(t)
	if _, err := env.reg.CreateRepo(context.Background(), env.tenantID, "models", "acme", "widget", false, ""); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte{0x1}, 1024)
	st, _ := env.blobs.Put(context.Background(), bytes.NewReader(body), blobstore.PutOptions{})
	if err := env.reg.UpsertBlob(context.Background(), st.OID, st.Size, st.StorageURL); err != nil {
		t.Fatal(err)
	}

	batchBody, _ := json.Marshal(map[string]any{
		"operation": "upload",
		"objects":   []map[string]any{{"oid": st.OID, "size": st.Size}},
	})
	resp, err := http.Post(env.stack.ProxyURL()+"/acme/widget.git/info/lfs/objects/batch",
		"application/vnd.git-lfs+json", bytes.NewReader(batchBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var br struct {
		Objects []struct {
			Actions map[string]any `json:"actions"`
		} `json:"objects"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&br)
	if len(br.Objects) != 1 {
		t.Fatalf("want 1 obj got %d", len(br.Objects))
	}
	if len(br.Objects[0].Actions) != 0 {
		t.Fatalf("expected empty actions (already uploaded), got %+v", br.Objects[0].Actions)
	}
}

func TestUpload_Commit_RejectsUnknownLFSPointer(t *testing.T) {
	env := uploadEnv(t)
	if _, err := env.reg.CreateRepo(context.Background(), env.tenantID, "models", "acme", "widget", false, ""); err != nil {
		t.Fatal(err)
	}

	var nd bytes.Buffer
	mustNDJSON(t, &nd, map[string]any{"key": "header", "value": map[string]string{"summary": "bad"}})
	mustNDJSON(t, &nd, map[string]any{"key": "lfsFile", "value": map[string]any{
		"path": "f.safetensors", "oid": strings.Repeat("0", 64), "size": 1,
	}})
	resp, err := http.Post(env.stack.ProxyURL()+"/api/models/acme/widget/commit/main",
		"application/x-ndjson", &nd)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want 400, body=%s", resp.StatusCode, out)
	}
}

func TestUpload_ConcurrentCommitsSameRepo(t *testing.T) {
	env := uploadEnv(t)
	if _, err := env.reg.CreateRepo(context.Background(), env.tenantID, "models", "acme", "concurrent", false, ""); err != nil {
		t.Fatal(err)
	}
	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			var nd bytes.Buffer
			mustNDJSON(t, &nd, map[string]any{"key": "header", "value": map[string]string{"summary": fmt.Sprintf("c%d", i)}})
			mustNDJSON(t, &nd, map[string]any{"key": "file", "value": map[string]string{
				"path": fmt.Sprintf("f%d.txt", i), "encoding": "utf-8", "content": fmt.Sprintf("v%d", i),
			}})
			// The handler retries serialization failures internally;
			// a single POST is sufficient.
			body := bytes.NewReader(nd.Bytes())
			resp, err := http.Post(env.stack.ProxyURL()+"/api/models/acme/concurrent/commit/main",
				"application/x-ndjson", body)
			if err != nil {
				t.Errorf("c%d post: %v", i, err)
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("c%d status=%d body=%s", i, resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
}

func TestUpload_PreuploadOnUnknownRepo_404(t *testing.T) {
	env := uploadEnv(t)
	body, _ := json.Marshal(map[string]any{"files": []map[string]any{{"path": "x", "size": 1}}})
	resp, err := http.Post(env.stack.ProxyURL()+"/api/models/nobody/missing/preupload/main",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
}

// ---- helpers ----

func mustNDJSON(t *testing.T, w *bytes.Buffer, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(b)
	w.WriteByte('\n')
}

func (e *registryTestEnv) queryAuditCount(t *testing.T, action string, out *int) error {
	t.Helper()
	// Reach into the registry's pool via a fresh query through the
	// store - registry.Store doesn't expose its pool, so we use a
	// helper test-only query through the AuditPool. The test sets
	// AuditPool == pool so we can use the same handle.
	return e.reg.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action=$1`, action).Scan(out)
}
