// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package mockhub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestModelInfo(t *testing.T) {
	srv := New(t, Config{
		Repos: []RepoSpec{{
			Name:         "acme/widget",
			InitialFiles: map[string][]byte{"config.json": []byte("{}"), "README.md": []byte("# widget")},
		}},
	})
	resp := mustGet(t, srv.URL()+"/api/models/acme/widget", "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var info modelInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.ID != "acme/widget" || info.SHA == "" {
		t.Fatalf("bad info: %+v", info)
	}
	if len(info.Siblings) != 2 {
		t.Fatalf("want 2 siblings got %d: %+v", len(info.Siblings), info.Siblings)
	}
}

func TestTree(t *testing.T) {
	srv := New(t, Config{Repos: []RepoSpec{{
		Name: "acme/widget",
		InitialFiles: map[string][]byte{
			"a.json":     []byte("a"),
			"sub/b.json": []byte("b"),
		},
	}}})
	resp := mustGet(t, srv.URL()+"/api/models/acme/widget/tree/main", "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var entries []treeEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries got %d", len(entries))
	}
}

func TestPathsInfo(t *testing.T) {
	srv := New(t, Config{Repos: []RepoSpec{{
		Name: "acme/widget",
		InitialFiles: map[string][]byte{
			"config.json": []byte("{}"),
			"weights.bin": bytes.Repeat([]byte{0xab}, 4096),
		},
	}}})
	body, _ := json.Marshal(pathsInfoRequest{Paths: []string{"config.json", "weights.bin", "missing.txt"}})
	resp := mustPost(t, srv.URL()+"/api/models/acme/widget/paths-info/main", "application/json", "", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out []pathsInfoEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 entries got %d", len(out))
	}
}

func TestResolveDownload(t *testing.T) {
	body := []byte("hello world, this is a test artifact")
	srv := New(t, Config{Repos: []RepoSpec{{
		Name:         "acme/widget",
		InitialFiles: map[string][]byte{"README.md": body},
	}}})
	resp := mustGet(t, srv.URL()+"/acme/widget/resolve/main/README.md", "")
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

func TestResolveHEAD(t *testing.T) {
	srv := New(t, Config{Repos: []RepoSpec{{
		Name:         "acme/widget",
		InitialFiles: map[string][]byte{"config.json": []byte("{\"x\":1}")},
	}}})
	req, _ := http.NewRequest(http.MethodHead, srv.URL()+"/acme/widget/resolve/main/config.json", nil)
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
}

func TestResolveRange(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 100) // 1000 bytes
	srv := New(t, Config{Repos: []RepoSpec{{
		Name:         "acme/widget",
		InitialFiles: map[string][]byte{"big.bin": body},
	}}})
	cases := []struct {
		name        string
		header      string
		wantStart   int
		wantEnd     int
		wantStatus  int
		wantContent string
	}{
		{"first 10", "bytes=0-9", 0, 9, http.StatusPartialContent, "0123456789"},
		{"middle", "bytes=100-104", 100, 104, http.StatusPartialContent, "01234"},
		{"open-ended", "bytes=990-", 990, 999, http.StatusPartialContent, "0123456789"},
		{"suffix", "bytes=-5", 995, 999, http.StatusPartialContent, "56789"},
		{"out-of-range", "bytes=5000-6000", 0, 0, http.StatusRequestedRangeNotSatisfiable, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL()+"/acme/widget/resolve/main/big.bin", nil)
			req.Header.Set("Range", c.header)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.wantStatus {
				t.Fatalf("status=%d want %d", resp.StatusCode, c.wantStatus)
			}
			if c.wantStatus == http.StatusPartialContent {
				got, _ := io.ReadAll(resp.Body)
				if string(got) != c.wantContent {
					t.Fatalf("body=%q want %q", got, c.wantContent)
				}
				wantCR := fmt.Sprintf("bytes %d-%d/1000", c.wantStart, c.wantEnd)
				if got := resp.Header.Get("Content-Range"); got != wantCR {
					t.Fatalf("Content-Range=%q want %q", got, wantCR)
				}
			}
		})
	}
}

func TestPreuploadDedup(t *testing.T) {
	srv := New(t, Config{Repos: []RepoSpec{{
		Name:         "acme/widget",
		InitialFiles: map[string][]byte{"config.json": []byte("{}")},
	}}})
	body, _ := json.Marshal(preuploadRequest{Files: []preuploadFileRequest{
		{Path: "config.json", Size: 2},       // dedup hit
		{Path: "new.json", Size: 5},          // new
		{Path: "weights.bin", Size: 1 << 22}, // > 10 MiB? No: 4 MiB → regular
		{Path: "big.bin", Size: 1 << 25},     // 32 MiB → lfs
	}})
	resp := mustPost(t, srv.URL()+"/api/models/acme/widget/preupload/main", "application/json", "", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var pr preuploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	if len(pr.Files) != 4 {
		t.Fatalf("want 4 files got %d", len(pr.Files))
	}
	byPath := map[string]preuploadFileResponse{}
	for _, f := range pr.Files {
		byPath[f.Path] = f
	}
	if !byPath["config.json"].ShouldIgnore {
		t.Fatal("config.json should be deduped")
	}
	if byPath["new.json"].ShouldIgnore {
		t.Fatal("new.json should not be deduped")
	}
	if byPath["big.bin"].UploadMode != "lfs" {
		t.Fatalf("big.bin mode=%q want lfs", byPath["big.bin"].UploadMode)
	}
	if byPath["weights.bin"].UploadMode != "regular" {
		t.Fatalf("weights.bin mode=%q want regular", byPath["weights.bin"].UploadMode)
	}
}

func TestCommitInlineFiles(t *testing.T) {
	srv := New(t, Config{Repos: []RepoSpec{{Name: "acme/widget"}}})
	var body bytes.Buffer
	writeNDJSON(t, &body, map[string]any{"key": "header", "value": map[string]string{"summary": "add files"}})
	writeNDJSON(t, &body, map[string]any{"key": "file", "value": map[string]string{
		"path": "config.json", "encoding": "utf-8", "content": `{"hidden_size": 768}`,
	}})
	writeNDJSON(t, &body, map[string]any{"key": "file", "value": map[string]string{
		"path": "README.md", "encoding": "utf-8", "content": "# acme/widget",
	}})

	resp := mustPost(t, srv.URL()+"/api/models/acme/widget/commit/main", "application/x-ndjson", "", &body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var cr commitResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	if !cr.Success || cr.CommitOID == "" {
		t.Fatalf("bad commit response: %+v", cr)
	}
	bs, ok := srv.FileBytes("models", "acme/widget", "main", "config.json")
	if !ok || string(bs) != `{"hidden_size": 768}` {
		t.Fatalf("file not stored or wrong: ok=%v body=%q", ok, bs)
	}
}

func TestCommitDeleteFile(t *testing.T) {
	srv := New(t, Config{Repos: []RepoSpec{{
		Name:         "acme/widget",
		InitialFiles: map[string][]byte{"old.txt": []byte("old"), "keep.txt": []byte("keep")},
	}}})
	var body bytes.Buffer
	writeNDJSON(t, &body, map[string]any{"key": "header", "value": map[string]string{"summary": "rm"}})
	writeNDJSON(t, &body, map[string]any{"key": "deletedFile", "value": map[string]string{"path": "old.txt"}})

	resp := mustPost(t, srv.URL()+"/api/models/acme/widget/commit/main", "application/x-ndjson", "", &body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if _, ok := srv.FileBytes("models", "acme/widget", "main", "old.txt"); ok {
		t.Fatal("old.txt should be deleted")
	}
	if _, ok := srv.FileBytes("models", "acme/widget", "main", "keep.txt"); !ok {
		t.Fatal("keep.txt should remain")
	}
}

func TestLFSUploadFlow(t *testing.T) {
	srv := New(t, Config{Repos: []RepoSpec{{Name: "acme/widget"}}})
	payload := bytes.Repeat([]byte{0xCA, 0xFE, 0xBA, 0xBE}, 1<<14) // 256 KiB
	oid := sha256Hex(payload)

	batchReq, _ := json.Marshal(lfsBatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []lfsBatchObj{{OID: oid, Size: int64(len(payload))}},
	})
	resp := mustPost(t, srv.URL()+"/acme/widget.git/info/lfs/objects/batch", "application/vnd.git-lfs+json", "", bytes.NewReader(batchReq))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("batch status=%d", resp.StatusCode)
	}
	var batchResp lfsBatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatal(err)
	}
	if len(batchResp.Objects) != 1 {
		t.Fatalf("want 1 object got %d", len(batchResp.Objects))
	}
	upload := batchResp.Objects[0].Actions["upload"]
	if upload.Href == "" {
		t.Fatalf("missing upload href: %+v", batchResp.Objects[0])
	}

	req, _ := http.NewRequest(http.MethodPut, upload.Href, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("PUT status=%d", resp2.StatusCode)
	}

	verifyURL := batchResp.Objects[0].Actions["verify"].Href
	verifyReq, _ := json.Marshal(lfsVerifyRequest{OID: oid, Size: int64(len(payload))})
	resp3 := mustPost(t, verifyURL, "application/vnd.git-lfs+json", "", bytes.NewReader(verifyReq))
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("verify status=%d", resp3.StatusCode)
	}

	if !srv.state.hasLFSObject(oid) {
		t.Fatal("object not stored")
	}
}

func TestAuthRoles(t *testing.T) {
	srv := New(t, Config{
		RequireAuth: true,
		Tokens: []Token{
			{Value: "rd_tok", Role: RoleRead},
			{Value: "wr_tok", Role: RoleWrite},
			{Value: "ex_tok", Role: RoleExpired},
		},
		Repos: []RepoSpec{{Name: "acme/widget", InitialFiles: map[string][]byte{"a.json": []byte("a")}}},
	})

	// No token → 401.
	resp := mustGet(t, srv.URL()+"/api/models/acme/widget", "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("anon got %d want 401", resp.StatusCode)
	}

	// Expired → 401.
	resp = mustGet(t, srv.URL()+"/api/models/acme/widget", "ex_tok")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expired got %d want 401", resp.StatusCode)
	}

	// Read token can read.
	resp = mustGet(t, srv.URL()+"/api/models/acme/widget", "rd_tok")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("read got %d want 200", resp.StatusCode)
	}

	// Read token CANNOT commit.
	var body bytes.Buffer
	writeNDJSON(t, &body, map[string]any{"key": "header", "value": map[string]string{"summary": "x"}})
	resp = mustPost(t, srv.URL()+"/api/models/acme/widget/commit/main", "application/x-ndjson", "rd_tok", &body)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("read-token commit got %d want 401", resp.StatusCode)
	}

	// Write token CAN commit.
	body.Reset()
	writeNDJSON(t, &body, map[string]any{"key": "header", "value": map[string]string{"summary": "x"}})
	writeNDJSON(t, &body, map[string]any{"key": "file", "value": map[string]string{"path": "b.json", "encoding": "utf-8", "content": "b"}})
	resp = mustPost(t, srv.URL()+"/api/models/acme/widget/commit/main", "application/x-ndjson", "wr_tok", &body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("write commit got %d want 200", resp.StatusCode)
	}
}

func TestCallCount(t *testing.T) {
	srv := New(t, Config{Repos: []RepoSpec{{
		Name:         "acme/widget",
		InitialFiles: map[string][]byte{"a.json": []byte("a")},
	}}})
	for i := 0; i < 5; i++ {
		resp := mustGet(t, srv.URL()+"/acme/widget/resolve/main/a.json", "")
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if got := srv.CallCount("GET", "/{repo}/resolve/{rev}/{path}"); got != 5 {
		t.Fatalf("call count=%d want 5", got)
	}
}

func TestConcurrentDownloads(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 4096)
	srv := New(t, Config{Repos: []RepoSpec{{
		Name:         "acme/widget",
		InitialFiles: map[string][]byte{"a.bin": body},
	}}})

	var wg sync.WaitGroup
	const N = 50
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			resp := mustGet(t, srv.URL()+"/acme/widget/resolve/main/a.bin", "")
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if !bytes.Equal(got, body) {
				t.Errorf("body mismatch")
			}
		}()
	}
	wg.Wait()
	if c := srv.CallCount("GET", "/{repo}/resolve/{rev}/{path}"); c != N {
		t.Fatalf("call count=%d want %d", c, N)
	}
}

func TestRepoNotFound(t *testing.T) {
	srv := New(t, Config{})
	resp := mustGet(t, srv.URL()+"/api/models/missing/repo", "")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
}

func TestSeedHelpers(t *testing.T) {
	srv := New(t, Config{})
	srv.AddRepo("models", "acme/x")
	sha1 := srv.SeedFile("models", "acme/x", "a.json", []byte(`{"a":1}`))
	sha2 := srv.SeedFile("models", "acme/x", "b.json", []byte(`{"b":2}`))
	if sha1 == "" || sha2 == "" || sha1 == sha2 {
		t.Fatalf("commit shas wrong: %q %q", sha1, sha2)
	}
	if srv.HeadCommitSHA("models", "acme/x") != sha2 {
		t.Fatal("HEAD did not advance")
	}
	// Both files should still be present at HEAD.
	for _, p := range []string{"a.json", "b.json"} {
		if _, ok := srv.FileBytes("models", "acme/x", "main", p); !ok {
			t.Fatalf("file %s missing at HEAD", p)
		}
	}
}

func TestSeedLFSPointerThenDownload(t *testing.T) {
	srv := New(t, Config{})
	srv.AddRepo("models", "acme/x")
	body := bytes.Repeat([]byte{0x42}, 32*1024)
	srv.SeedLFSPointer("models", "acme/x", "weights.safetensors", body)

	resp := mustGet(t, srv.URL()+"/acme/x/resolve/main/weights.safetensors", "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Fatal("LFS body mismatch")
	}

	wantHash := hex.EncodeToString(func() []byte { h := sha256.Sum256(body); return h[:] }())
	if got := strings.Trim(resp.Header.Get("ETag"), `"`); got != wantHash {
		t.Fatalf("etag=%q want %q", got, wantHash)
	}
}

// ----- helpers -----

func mustGet(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mustPost(t *testing.T, url, ct, token string, body io.Reader) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, body)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func writeNDJSON(t *testing.T, buf *bytes.Buffer, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	buf.Write(b)
	buf.WriteByte('\n')
}
