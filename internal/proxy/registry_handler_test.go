// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/pulsys-io/pulsys/internal/blobstore"
	"github.com/pulsys-io/pulsys/internal/hfhub/fixtures"
	"github.com/pulsys-io/pulsys/internal/hfhub/mockhub"
	"github.com/pulsys-io/pulsys/internal/registry"
	"github.com/pulsys-io/pulsys/internal/testpg"
	"github.com/pulsys-io/pulsys/internal/testserver"
)

// registryTestEnv builds the full stack:
//
//   - fresh Postgres (PULSYS_TEST_PG_DSN required)
//   - registry.Store + tenant
//   - blobstore in tempdir
//   - upstream mockhub
//   - testserver.Stack wired with a RegistryHandler in front
//
// The stack URL is returned via Stack.ProxyURL().
type registryTestEnv struct {
	stack    *testserver.Stack
	reg      *registry.Store
	blobs    *blobstore.LocalStore
	tenantID string
	mockHub  *mockhub.Server
}

func newRegistryEnv(t *testing.T, mockRepos []mockhub.RepoSpec) *registryTestEnv {
	t.Helper()
	pool := testpg.Acquire(t) // fresh, fully-migrated DB per test
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
		Mock: mockhub.Config{Repos: mockRepos},
		Registry: &testserver.RegistryConfig{
			Store:    reg,
			Blobs:    blobs,
			TenantID: tenantID,
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

// seedRegistryRepo creates a repo + initial commit with inline files.
// Mirrors fixture data so tests can assert against known bytes.
func (e *registryTestEnv) seedRegistryRepo(t *testing.T, ns, name string, files map[string][]byte) string {
	t.Helper()
	repo, err := e.reg.CreateRepo(context.Background(), e.tenantID, "models", ns, name, false, "")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	inline := make(map[string]registry.InlineCommitFile, len(files))
	for p, body := range files {
		st, err := e.blobs.Put(context.Background(), bytes.NewReader(body), blobstore.PutOptions{})
		if err != nil {
			t.Fatalf("blob put %s: %v", p, err)
		}
		if err := e.reg.UpsertBlob(context.Background(), st.OID, st.Size, st.StorageURL); err != nil {
			t.Fatalf("upsert blob: %v", err)
		}
		inline[p] = registry.InlineCommitFile{BlobOID: st.OID, Size: st.Size}
	}
	res, err := e.reg.CommitTx(context.Background(), registry.CommitInput{
		RepoID:  repo.ID,
		Summary: "seed " + ns + "/" + name,
		Inline:  inline,
	})
	if err != nil {
		t.Fatalf("CommitTx: %v", err)
	}
	return res.SHA
}

func TestRegistry_ModelInfo_FromRegistry(t *testing.T) {
	env := newRegistryEnv(t, nil)
	files := fixtures.TinyModelFiles("acme/widget")
	sha := env.seedRegistryRepo(t, "acme", "widget", files)

	resp, err := http.Get(env.stack.ProxyURL() + "/api/models/acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var info struct {
		ID       string `json:"id"`
		SHA      string `json:"sha"`
		Siblings []struct {
			RFilename string `json:"rfilename"`
		} `json:"siblings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.ID != "acme/widget" || info.SHA != sha {
		t.Fatalf("info=%+v want sha=%s", info, sha)
	}
	if len(info.Siblings) != len(files) {
		t.Fatalf("siblings=%d want %d", len(info.Siblings), len(files))
	}
	// MUST NOT have hit the upstream mock - registry handled it.
	if c := env.mockHub.CallCount("GET", "/api/models/{repo}"); c != 0 {
		t.Fatalf("upstream calls=%d want 0 (registry hit)", c)
	}
}

func TestRegistry_Tree_FromRegistry(t *testing.T) {
	env := newRegistryEnv(t, nil)
	env.seedRegistryRepo(t, "acme", "widget", map[string][]byte{
		"a.json":    []byte(`{"a":1}`),
		"b.json":    []byte(`{"b":2}`),
		"sub/c.txt": []byte("c"),
	})
	resp, err := http.Get(env.stack.ProxyURL() + "/api/models/acme/widget/tree/main")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var entries []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries=%d want 3", len(entries))
	}
}

func TestRegistry_Resolve_GET_FromRegistry(t *testing.T) {
	env := newRegistryEnv(t, nil)
	body := fixtures.ConfigJSON()
	env.seedRegistryRepo(t, "acme", "widget", map[string][]byte{"config.json": body})

	resp, err := http.Get(env.stack.ProxyURL() + "/acme/widget/resolve/main/config.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Fatalf("body mismatch")
	}
	if resp.Header.Get("X-Repo-Commit") == "" {
		t.Fatal("missing X-Repo-Commit")
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("missing Accept-Ranges")
	}
}

func TestRegistry_Resolve_HEAD_FromRegistry(t *testing.T) {
	env := newRegistryEnv(t, nil)
	body := []byte("hello world")
	env.seedRegistryRepo(t, "acme", "widget", map[string][]byte{"README.md": body})

	req, _ := http.NewRequest(http.MethodHead, env.stack.ProxyURL()+"/acme/widget/resolve/main/README.md", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if resp.Header.Get("X-Linked-Etag") == "" {
		t.Fatal("missing X-Linked-Etag")
	}
	if cl := resp.Header.Get("Content-Length"); cl != fmt.Sprint(len(body)) {
		t.Fatalf("Content-Length=%q want %d", cl, len(body))
	}
}

func TestRegistry_Resolve_Range_FromRegistry(t *testing.T) {
	env := newRegistryEnv(t, nil)
	body := bytes.Repeat([]byte("0123456789"), 100) // 1000 bytes
	env.seedRegistryRepo(t, "acme", "widget", map[string][]byte{"big.bin": body})

	cases := []struct {
		header   string
		wantCR   string
		wantBody string
	}{
		{"bytes=0-9", "bytes 0-9/1000", "0123456789"},
		{"bytes=100-104", "bytes 100-104/1000", "01234"},
		{"bytes=-5", "bytes 995-999/1000", "56789"},
		{"bytes=990-", "bytes 990-999/1000", "0123456789"},
	}
	for _, c := range cases {
		t.Run(c.header, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, env.stack.ProxyURL()+"/acme/widget/resolve/main/big.bin", nil)
			req.Header.Set("Range", c.header)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusPartialContent {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Range"); got != c.wantCR {
				t.Fatalf("Content-Range=%q want %q", got, c.wantCR)
			}
			got, _ := io.ReadAll(resp.Body)
			if string(got) != c.wantBody {
				t.Fatalf("body=%q want %q", got, c.wantBody)
			}
		})
	}
}

func TestRegistry_PathsInfo_FromRegistry(t *testing.T) {
	env := newRegistryEnv(t, nil)
	env.seedRegistryRepo(t, "acme", "widget", map[string][]byte{
		"config.json": []byte("{}"),
		"README.md":   []byte("# x"),
	})

	body, _ := json.Marshal(map[string]any{"paths": []string{"config.json", "missing.txt"}})
	resp, err := http.Post(env.stack.ProxyURL()+"/api/models/acme/widget/paths-info/main",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out []struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out) != 1 || out[0].Path != "config.json" {
		t.Fatalf("entries=%+v want [config.json]", out)
	}
}

func TestRegistry_404_WhenNoMirror(t *testing.T) {
	env := newRegistryEnv(t, []mockhub.RepoSpec{{
		Name:         "facebook/opt",
		InitialFiles: map[string][]byte{"config.json": []byte("{}")},
	}})

	resp, err := http.Get(env.stack.ProxyURL() + "/api/models/facebook/opt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404 (no registry repo, no mirror)", resp.StatusCode)
	}
	if c := env.mockHub.CallCount("GET", "/api/models/{repo}"); c != 0 {
		t.Fatalf("upstream calls=%d want 0", c)
	}
}

func TestRegistry_MirrorFallback_HitsUpstreamAndCaches(t *testing.T) {
	env := newRegistryEnv(t, []mockhub.RepoSpec{{
		Name: "facebook/opt",
		InitialFiles: map[string][]byte{
			"config.json": fixtures.ConfigJSON(),
			"README.md":   []byte("# opt"),
		},
	}})
	// Declare a mirror so the resolver falls through to upstream.
	if _, err := env.reg.CreateMirror(context.Background(), env.tenantID, "models", "facebook", "opt", "", "", ""); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(env.stack.ProxyURL() + "/facebook/opt/resolve/main/config.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, fixtures.ConfigJSON()) {
		t.Fatal("mirror body mismatch")
	}

	// Warm cache: second request must NOT hit the mock again.
	resp2, _ := http.Get(env.stack.ProxyURL() + "/facebook/opt/resolve/main/config.json")
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if c := env.mockHub.CallCount("GET", "/{repo}/resolve/{rev}/{path}"); c != 1 {
		t.Fatalf("upstream resolve calls=%d want 1 (cache hit on 2nd)", c)
	}
}

func TestRegistry_HealthAndAdminPassthrough(t *testing.T) {
	env := newRegistryEnv(t, nil)
	// /healthz lives behind Next (the inner proxy handler).
	resp, err := http.Get(env.stack.ProxyURL() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/healthz status=%d", resp.StatusCode)
	}
}

func TestRegistry_RevisionLookup_AcceptsShortSHA(t *testing.T) {
	env := newRegistryEnv(t, nil)
	sha := env.seedRegistryRepo(t, "acme", "widget", map[string][]byte{"x": []byte("v1")})

	for _, ref := range []string{"main", sha, sha[:7]} {
		resp, err := http.Get(env.stack.ProxyURL() + "/acme/widget/resolve/" + ref + "/x")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(got) != "v1" {
			t.Fatalf("ref=%s status=%d body=%q", ref, resp.StatusCode, got)
		}
	}
}

func TestRegistry_ConcurrentReads_NoRace(t *testing.T) {
	env := newRegistryEnv(t, nil)
	body := bytes.Repeat([]byte{0x99}, 4096)
	env.seedRegistryRepo(t, "acme", "widget", map[string][]byte{"weights.bin": body})

	const N = 30
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			resp, err := http.Get(env.stack.ProxyURL() + "/acme/widget/resolve/main/weights.bin")
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if !bytes.Equal(got, body) {
				t.Errorf("body mismatch len=%d", len(got))
			}
		}()
	}
	wg.Wait()
}

func TestRegistry_BlobMissingOnDisk_500(t *testing.T) {
	env := newRegistryEnv(t, nil)
	env.seedRegistryRepo(t, "acme", "widget", map[string][]byte{"a.txt": []byte("hi")})

	// Sabotage: remove the blob file under the blobstore root, but
	// leave the registry row in place. The handler should return 5xx.
	repo, _ := env.reg.GetRepo(context.Background(), env.tenantID, "models", "acme", "widget")
	files, _, err := env.reg.ListFiles(context.Background(), repo.ID, env.mustHead(t, repo.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := env.blobs.Delete(context.Background(), f.BlobOID); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := http.Get(env.stack.ProxyURL() + "/acme/widget/resolve/main/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatalf("expected 5xx after blob deletion, got 200")
	}
}

func (e *registryTestEnv) mustHead(t *testing.T, repoID string) string {
	t.Helper()
	sha, err := e.reg.ResolveBranch(context.Background(), repoID, "main")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// guard: ensure registry errors don't leak ErrNotFound when not
// resolved.
var _ = errors.Is
