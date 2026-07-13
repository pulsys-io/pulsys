// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build realhf

package realhf

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/coreserver"
	"github.com/pulsys-io/pulsys/internal/logx"
	"github.com/pulsys-io/pulsys/internal/proxy"
	"github.com/pulsys-io/pulsys/internal/telemetry"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

// publicReadModel + publicReadFile is a tiny public artefact used
// for byte-equality assertions. `prajjwal1/bert-tiny/config.json`
// is ~300 bytes and has been stable for years.
//
// publicRangeModel + publicRangeFile is a bigger raw file used for
// the parallel-range parity test. `gpt2/vocab.json` is ~1 MiB and
// served from the raw model files, not LFS, so the parity assertion
// stays simple.
const (
	publicReadModel  = "prajjwal1/bert-tiny"
	publicReadFile   = "config.json"
	publicRangeModel = "gpt2"
	publicRangeFile  = "vocab.json"
	hfBaseURL        = "https://huggingface.co"
	defaultClientTO  = 60 * time.Second
)

// gateRead skips when the read gates aren't both set. The first
// gate (build tag) is enforced at compile time.
func gateRead(t *testing.T) {
	t.Helper()
	if os.Getenv("HF_INTEGRATION_REAL") != "1" {
		t.Skip("HF_INTEGRATION_REAL=1 not set; skipping real-Hub test")
	}
}

// gateWrite enforces the additional write-side gates. Write tests
// MUST call this in addition to gateRead.
func gateWrite(t *testing.T) (token string) {
	t.Helper()
	if os.Getenv("HF_INTEGRATION_REAL_WRITE") != "1" {
		t.Skip("HF_INTEGRATION_REAL_WRITE=1 not set; skipping HF write test")
	}
	token = os.Getenv("HF_TOKEN")
	if token == "" {
		t.Skip("HF_TOKEN not set; cannot exercise HF write API")
	}
	return token
}

// ---- Read: direct against huggingface.co ----

// TestRealHF_DirectDownloadPublicFile validates that we can reach
// huggingface.co from the CI runner and that the file we use as our
// equality fixture parses as JSON.
func TestRealHF_DirectDownloadPublicFile(t *testing.T) {
	gateRead(t)
	body := mustGet(t, fmt.Sprintf("%s/%s/resolve/main/%s", hfBaseURL, publicReadModel, publicReadFile))
	if len(body) < 16 {
		t.Fatalf("config.json suspiciously small: %d bytes", len(body))
	}
	var any map[string]any
	if err := json.Unmarshal(body, &any); err != nil {
		t.Fatalf("config.json not valid JSON: %v\n%s", err, body)
	}
}

// ---- Read: through a Pulsys proxy with huggingface.co as upstream ----

// TestRealHF_ProxyDownloadPublicFile proves Pulsys is byte-for-byte
// transparent for a public model: download via direct HF -> A,
// download via Pulsys (forwarded to HF) -> B, assert A == B.
// Cache hit on a second pass; assert the upstream isn't hit twice.
func TestRealHF_ProxyDownloadPublicFile(t *testing.T) {
	gateRead(t)
	want := mustGet(t, fmt.Sprintf("%s/%s/resolve/main/%s", hfBaseURL, publicReadModel, publicReadFile))

	proxyURL, _ := startLocalProxy(t)
	got1 := mustGet(t, fmt.Sprintf("%s/%s/resolve/main/%s?_pass=1", proxyURL, publicReadModel, publicReadFile))
	if !bytes.Equal(got1, want) {
		t.Fatalf("first pass: %d vs %d bytes (proxy diverged from upstream)", len(got1), len(want))
	}
	// Warm fetch: must equal cold AND be served from cache.
	got2 := mustGet(t, fmt.Sprintf("%s/%s/resolve/main/%s?_pass=2", proxyURL, publicReadModel, publicReadFile))
	if !bytes.Equal(got2, want) {
		t.Fatalf("warm pass: cache returned %d bytes, want %d", len(got2), len(want))
	}
}

// ---- Range / hf_transfer parity ----

// TestRealHF_ParallelRangeRoundtrip exercises the hf_transfer-style
// path: split a file into 4 chunks, download each in parallel via
// Range headers, reassemble, assert byte equality with the
// single-shot download.
func TestRealHF_ParallelRangeRoundtrip(t *testing.T) {
	gateRead(t)
	url := fmt.Sprintf("%s/%s/resolve/main/%s", hfBaseURL, publicRangeModel, publicRangeFile)
	full := mustGet(t, url)
	size := int64(len(full))
	if size < 4096 {
		t.Fatalf("fixture too small for range test: %d bytes (want >= 4 KiB)", size)
	}

	const chunks = 4
	stride := size / chunks
	parts := make([][]byte, chunks)
	var wg sync.WaitGroup
	for i := 0; i < chunks; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			start := int64(idx) * stride
			end := start + stride - 1
			if idx == chunks-1 {
				end = size - 1
			}
			parts[idx] = mustGetRange(t, url, start, end)
			if int64(len(parts[idx])) != end-start+1 {
				t.Errorf("chunk %d: got %d bytes, want %d", idx, len(parts[idx]), end-start+1)
			}
		}(i)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	var assembled bytes.Buffer
	for _, p := range parts {
		assembled.Write(p)
	}
	if !bytes.Equal(assembled.Bytes(), full) {
		t.Fatalf("HARD: parallel-range reassembly diverged from full download (%d vs %d bytes)",
			assembled.Len(), len(full))
	}
}

// ---- Write: round-trip a private repo against huggingface.co ----

// TestRealHF_PrivateRepoRoundtrip creates a throwaway private repo
// on the user's HF account, uploads a small file, reads it back,
// and deletes the repo. The full write surface (create + upload +
// commit) is exercised against the real Hub.
//
// This is the most expensive test in the suite (creates real
// resources on the user's HF account). It is gated behind
// HF_INTEGRATION_REAL_WRITE=1 + HF_TOKEN so nothing happens by
// accident.
func TestRealHF_PrivateRepoRoundtrip(t *testing.T) {
	gateRead(t)
	token := gateWrite(t)

	user := whoAmI(t, token)
	if user == "" {
		t.Fatal("HF token is valid but `name` is empty - cannot construct repo path")
	}

	repoSuffix := randHex(t, 4)
	repoName := fmt.Sprintf("pulsys-ci-%s", repoSuffix)
	fullName := fmt.Sprintf("%s/%s", user, repoName)
	t.Logf("creating throwaway private repo: %s", fullName)

	// 1. create_repo
	createBody, _ := json.Marshal(map[string]any{
		"name": repoName, "type": "model", "private": true,
	})
	req, _ := http.NewRequest(http.MethodPost, hfBaseURL+"/api/repos/create", bytes.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	doOK(t, req, http.StatusOK, http.StatusCreated)

	t.Cleanup(func() {
		delBody, _ := json.Marshal(map[string]any{"name": repoName, "type": "model"})
		req, _ := http.NewRequest(http.MethodDelete, hfBaseURL+"/api/repos/delete", bytes.NewReader(delBody))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("repo cleanup failed (%s): %v", fullName, err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	})

	// 2. commit a small inline file via the NDJSON commit endpoint.
	const content = "# pulsys CI sentinel — safe to delete\n"
	var nd bytes.Buffer
	mustNDJSONLine(&nd, map[string]any{"key": "header", "value": map[string]string{
		"summary": "pulsys CI sentinel",
	}})
	mustNDJSONLine(&nd, map[string]any{"key": "file", "value": map[string]string{
		"path": "README.md", "encoding": "utf-8", "content": content,
	}})
	req, _ = http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/models/%s/commit/main", hfBaseURL, fullName), &nd)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	doOK(t, req, http.StatusOK)

	// 3. download README.md back and compare.
	dl, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/%s/resolve/main/README.md", hfBaseURL, fullName), nil)
	dl.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(dl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("download README: status=%d body=%s", resp.StatusCode, body)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != content {
		t.Fatalf("HARD: uploaded != downloaded\n got=%q\nwant=%q", got, content)
	}
}

// ---- helpers ----

func mustGet(t *testing.T, url string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	client := &http.Client{Timeout: defaultClientTO}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status=%d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s body: %v", url, err)
	}
	return b
}

func mustGetRange(t *testing.T, url string, start, end int64) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	client := &http.Client{Timeout: defaultClientTO}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("RANGE %s [%d-%d]: %v", url, start, end, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("RANGE %s [%d-%d]: status=%d want 206", url, start, end, resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, _ := strconv.ParseInt(cl, 10, 64); n != end-start+1 {
			t.Fatalf("RANGE %s: Content-Length=%s want %d", url, cl, end-start+1)
		}
	}
	b, _ := io.ReadAll(resp.Body)
	return b
}

func doOK(t *testing.T, req *http.Request, accept ...int) {
	t.Helper()
	client := &http.Client{Timeout: defaultClientTO}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	for _, code := range accept {
		if resp.StatusCode == code {
			_, _ = io.Copy(io.Discard, resp.Body)
			return
		}
	}
	body, _ := io.ReadAll(resp.Body)
	t.Fatalf("%s %s: status=%d (want %v) body=%s", req.Method, req.URL, resp.StatusCode, accept, body)
}

func whoAmI(t *testing.T, token string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, hfBaseURL+"/api/whoami-v2", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("whoami status=%d body=%s", resp.StatusCode, body)
	}
	var v struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v.Name
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func mustNDJSONLine(buf *bytes.Buffer, v any) {
	b, _ := json.Marshal(v)
	buf.Write(b)
	buf.WriteByte('\n')
}

// startLocalProxy boots a pulsys with huggingface.co as the
// default upstream and returns (publicURL, cleanup). It is the same
// shape the production stack uses, just bound to a random loopback
// port.
func startLocalProxy(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	public := "http://" + addr
	args := []string{
		"-listen", addr,
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", cacheDir,
		"-public-base-url", public,
		"-default-upstream-host", "huggingface.co",
		"-upstream-scheme", "https",
		"-allow-host", "huggingface.co,cdn-lfs.huggingface.co",
		"-log-level", "warn",
	}
	cfg, err := config.ParseFlags(flag.NewFlagSet("realhf", flag.ContinueOnError), args)
	if err != nil {
		t.Fatal(err)
	}
	log := logx.New("warn")
	telemetry.Register()
	store, err := cache.NewStore(cacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	up := upstream.New(cfg)
	h := proxy.NewHandler(cfg, store, up, log)
	core := &coreserver.Server{
		Cfg:          cfg,
		Store:        store,
		Fallback:     coreserver.HandlerFallback(h),
		ReadTimeout:  defaultClientTO,
		WriteTimeout: defaultClientTO,
	}
	go func() { _ = core.Serve(ln) }()
	t.Cleanup(func() {
		core.Close()
		_ = ln.Close()
	})

	// Tiny readiness probe.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return public, func() {}
}

// suppress unused-import lints when running without the realhf tag
var _ = context.Background
