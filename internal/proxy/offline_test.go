// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy_test

import (
	"bytes"
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/logx"
	"github.com/pulsys-io/pulsys/internal/proxy"
	"github.com/pulsys-io/pulsys/internal/telemetry"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

// newOfflineProxyOnExistingCache reuses a cache directory populated by an
// earlier online proxy and starts a NEW proxy with -offline.  This mirrors
// production: warm the cache once, then run in no-egress mode forever.
func newOfflineProxyOnExistingCache(tb testing.TB, cacheDir string, fake upstream.Client) (*http.Client, string, func()) {
	tb.Helper()
	cfg, err := config.ParseFlags(flag.NewFlagSet("offline", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", cacheDir,
		"-public-base-url", "http://test.local",
		"-strict-offline",
	})
	if err != nil {
		tb.Fatal(err)
	}
	if !cfg.StrictOffline {
		tb.Fatal("expected -strict-offline to be set")
	}
	store, err := cache.NewStore(cfg.CacheDir, "none")
	if err != nil {
		tb.Fatal(err)
	}
	h := proxy.NewHandler(cfg, store, fake, logx.New("error"))
	srv := httptest.NewServer(h)
	tr := &http.Transport{MaxIdleConns: 16, DisableCompression: true}
	client := &http.Client{Transport: tr}
	return client, srv.URL, func() {
		tr.CloseIdleConnections()
		srv.Close()
	}
}

// TestOfflineWarmHitServes verifies the production-critical contract: once
// the cache holds the file, a -offline proxy serves the body with ZERO
// upstream attempts.  This is the "pull the network cable after warming"
// guarantee.
func TestOfflineWarmHitServes(t *testing.T) {
	payload := bytes.Repeat([]byte("o"), 96*1024)

	// Phase 1: ONLINE proxy populates the cache.
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	online := &fakeUpstream{}
	online.set(&fakeResp{status: 200, body: payload, contentType: "application/octet-stream"})
	onlineCfg, err := config.ParseFlags(flag.NewFlagSet("online", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", cacheDir,
		"-public-base-url", "http://test.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewStore(onlineCfg.CacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	onlineH := proxy.NewHandler(onlineCfg, store, online, logx.New("error"))
	onlineSrv := httptest.NewServer(onlineH)
	t.Cleanup(onlineSrv.Close)
	onlineClient := &http.Client{Transport: &http.Transport{DisableCompression: true}}

	path := "/openai-community/gpt2/resolve/main/big.bin"
	if status, body := drainGet(t, onlineClient, onlineSrv.URL, path, nil); status != 200 || !bytes.Equal(body, payload) {
		t.Fatalf("warm-phase cold GET: status=%d len=%d", status, len(body))
	}

	// Phase 2: OFFLINE proxy on the same cache dir.  Use a fake upstream
	// that PANICS if called -- if the test hits it, we have a real bug,
	// not a flake.
	panicUp := &panickingUpstream{t: t}
	c, base, stop := newOfflineProxyOnExistingCache(t, cacheDir, panicUp)
	defer stop()

	refusalsBefore := telemetry.OfflineRefusalsSnapshot()

	// 2a: full-body GET MUST succeed and MUST NOT call upstream.
	status, got := drainGet(t, c, base, path, nil)
	if status != 200 {
		t.Fatalf("offline full GET: got status=%d, want 200", status)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("offline full GET: body mismatch (got %d bytes, want %d)", len(got), len(payload))
	}

	// 2b: range GET against the cached full file MUST also succeed.
	status, got = drainGet(t, c, base, path, map[string]string{"Range": "bytes=1024-2047"})
	if status != http.StatusPartialContent {
		t.Fatalf("offline range GET: got status=%d, want 206", status)
	}
	if !bytes.Equal(got, payload[1024:2048]) {
		t.Fatalf("offline range GET: body mismatch")
	}

	// 2c: a request for an UNCACHED file MUST 504 (not silently re-fetch).
	uncached := "/openai-community/gpt2/resolve/main/never-fetched.bin"
	status, body := drainGet(t, c, base, uncached, nil)
	if status != http.StatusGatewayTimeout {
		t.Fatalf("offline uncached GET: got status=%d, want 504", status)
	}
	if !strings.Contains(string(body), "offline=1") || !strings.Contains(string(body), "cache-miss-in-offline-mode") {
		t.Fatalf("offline uncached GET body does not name the missing slot:\n%s", body)
	}
	if !strings.Contains(string(body), uncached) {
		t.Fatalf("offline uncached GET body should mention path=%s:\n%s", uncached, body)
	}

	// Refusal counter should have ticked exactly once (for the uncached request).
	if delta := telemetry.OfflineRefusalsSnapshot() - refusalsBefore; delta != 1 {
		t.Fatalf("offline refusal counter delta = %d, want 1", delta)
	}
}

// panickingUpstream blows up the test if hit; any call means -offline
// failed to short-circuit a request before reaching the upstream client.
type panickingUpstream struct{ t testing.TB }

func (p *panickingUpstream) Do(_ context.Context, _, _, _, _ string, _ http.Header, _ []byte) (*upstream.Response, error) {
	p.t.Fatalf("upstream called in offline mode -- proxy reached upstream when it should have returned 504")
	return nil, nil
}

// TestOfflineRedirectHitServes verifies that a cached 302 from the
// resolve endpoint is replayed in offline mode without any upstream
// call.  This is the binding optimisation for parallel range clients:
// without it every range issues one upstream HF API call just to
// re-read the same redirect target, and offline mode (which has no
// upstream) cannot serve the resolve hop at all.
func TestOfflineRedirectHitServes(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	// Phase 1: online proxy receives a 302 from upstream and caches it.
	online := &fakeUpstream{}
	online.set(&fakeResp{
		status:     http.StatusFound,
		body:       nil,
		contentLen: 0,
	})
	online.resp.Load().contentRange = "" // ensure no Content-Range
	// fakeUpstream's Do doesn't honor a Location header field on
	// fakeResp by default; extend with a Location-aware variant.
	loUp := &locUpstream{
		target: "https://cas-bridge.xethub.hf.co/xet-bridge-us/abc/deadbeef?X-Amz-Date=20260517T000000Z",
	}
	onlineCfg, err := config.ParseFlags(flag.NewFlagSet("online-redir", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", cacheDir,
		"-public-base-url", "http://test.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewStore(onlineCfg.CacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	onlineH := proxy.NewHandler(onlineCfg, store, loUp, logx.New("error"))
	onlineSrv := httptest.NewServer(onlineH)
	t.Cleanup(onlineSrv.Close)
	onlineClient := &http.Client{
		Transport: &http.Transport{DisableCompression: true},
		// Don't follow redirects -- we want to inspect 302 directly.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resolvePath := "/openai-community/gpt2/resolve/main/model.safetensors"
	status, _ := drainGet(t, onlineClient, onlineSrv.URL, resolvePath, nil)
	if status != http.StatusFound {
		t.Fatalf("online cold: got status=%d, want 302", status)
	}
	if calls := loUp.calls.Load(); calls != 1 {
		t.Fatalf("online cold: upstream called %d times, want 1", calls)
	}

	// Phase 2: OFFLINE proxy on the same cache; upstream panics if called.
	panicUp := &panickingUpstream{t: t}
	c, base, stop := newOfflineProxyOnExistingCache(t, cacheDir, panicUp)
	defer stop()
	// Same no-follow client config as above.
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	status, _ = drainGet(t, c, base, resolvePath, nil)
	if status != http.StatusFound {
		t.Fatalf("offline redirect: got status=%d, want 302 (cache hit)", status)
	}
}

// TestOfflineHeadAliasOnGetCache verifies that a HEAD request in
// offline mode hits the GET cache for the same path.  This is the
// production-critical alias that lets huggingface_hub's
// snapshot_download work offline -- it issues a HEAD per file before
// the body GET to validate ETag / Content-Length, and without this
// alias every HEAD 504s on cold cache (we only ever stored the GET).
func TestOfflineHeadAliasOnGetCache(t *testing.T) {
	payload := bytes.Repeat([]byte("h"), 4096)
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	// Phase 1: online proxy populates the GET cache.
	online := &fakeUpstream{}
	online.set(&fakeResp{status: 200, body: payload, contentType: "application/octet-stream", etag: `"v1"`})
	onlineCfg, err := config.ParseFlags(flag.NewFlagSet("online-head", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", cacheDir,
		"-public-base-url", "http://test.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewStore(onlineCfg.CacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	onlineH := proxy.NewHandler(onlineCfg, store, online, logx.New("error"))
	onlineSrv := httptest.NewServer(onlineH)
	t.Cleanup(onlineSrv.Close)
	onlineClient := &http.Client{Transport: &http.Transport{DisableCompression: true}}

	path := "/openai-community/gpt2/resolve/main/config.json"
	if status, _ := drainGet(t, onlineClient, onlineSrv.URL, path, nil); status != 200 {
		t.Fatalf("online cold GET: status=%d", status)
	}

	// Phase 2: OFFLINE proxy; upstream panics if called.
	panicUp := &panickingUpstream{t: t}
	c, base, stop := newOfflineProxyOnExistingCache(t, cacheDir, panicUp)
	defer stop()

	req, err := http.NewRequest(http.MethodHead, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("offline HEAD: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("offline HEAD: got status=%d, want 200 (alias to GET cache)", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != "4096" {
		t.Fatalf("offline HEAD Content-Length: got %q, want 4096", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("offline HEAD Content-Type: got %q", got)
	}
	if got := resp.Header.Get("ETag"); got != `"v1"` {
		t.Fatalf("offline HEAD ETag: got %q", got)
	}
}

// TestOfflineHeadAliasOnRedirectCache: HEAD on /resolve/<rev>/<file>
// should hit the cached 302 (under the GET key) and return 302 with
// Location.  This is the exact path huggingface_hub validates before
// fetching the body.
func TestOfflineHeadAliasOnRedirectCache(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	loUp := &locUpstream{
		target: "https://cas-bridge.xethub.hf.co/xet-bridge-us/abc/deadbeef?X-Amz-Date=20260517T000000Z",
	}
	onlineCfg, err := config.ParseFlags(flag.NewFlagSet("online-head-redir", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", cacheDir,
		"-public-base-url", "http://test.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewStore(onlineCfg.CacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	onlineH := proxy.NewHandler(onlineCfg, store, loUp, logx.New("error"))
	onlineSrv := httptest.NewServer(onlineH)
	t.Cleanup(onlineSrv.Close)
	onlineClient := &http.Client{
		Transport: &http.Transport{DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	path := "/Qwen/Qwen2.5-7B-Instruct/resolve/main/config.json"
	if status, _ := drainGet(t, onlineClient, onlineSrv.URL, path, nil); status != http.StatusFound {
		t.Fatalf("online cold GET: status=%d", status)
	}

	panicUp := &panickingUpstream{t: t}
	c, base, stop := newOfflineProxyOnExistingCache(t, cacheDir, panicUp)
	defer stop()
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	req, _ := http.NewRequest(http.MethodHead, base+path, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("offline HEAD: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("offline HEAD on cached 302: got status=%d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc == "" {
		t.Fatal("offline HEAD on cached 302: missing Location header")
	}
}

// locUpstream is a minimal upstream.Client that always returns a 302
// with a configurable Location header.  Used by the redirect-cache test.
type locUpstream struct {
	target string
	// hfHeaders, when non-nil, are echoed verbatim on every 302 to
	// mimic the real /resolve/<rev>/<file> response that huggingface_hub
	// validates (X-Linked-Etag, X-Linked-Size, X-Repo-Commit, ...).
	hfHeaders map[string]string
	calls     atomic.Int64
}

func (l *locUpstream) Do(_ context.Context, _, _, _, _ string, _ http.Header, _ []byte) (*upstream.Response, error) {
	l.calls.Add(1)
	h := http.Header{}
	h.Set("Location", l.target)
	for k, v := range l.hfHeaders {
		h.Set(k, v)
	}
	return &upstream.Response{
		Status:        http.StatusFound,
		Header:        h,
		ContentLength: 0,
		Body:          ioNopCloser(),
	}, nil
}

func ioNopCloser() io.ReadCloser {
	return io.NopCloser(bytes.NewReader(nil))
}

// TestWarmHeadPreservesHFLinkedHeaders is the regression test for the
// production failure where Python's `hf download` warm phase raised
// LocalEntryNotFoundError after a successful cold phase.  Root cause:
// huggingface_hub validates every file via HEAD on /resolve/<rev>/<file>
// and reads X-Linked-Etag / X-Linked-Size / X-Repo-Commit (preferring
// the X-Linked-* variants over the plain ETag / Content-Length).  If
// any of these are missing on the warm response, snapshot_download
// gives up with "we cannot find the requested files in the local cache".
//
// Pre-fix the proxy persisted only Location / Content-Type / ETag to
// the redirect meta, so the warm HEAD replay was missing the three
// linked-* headers.  The fix preserves them via meta.ExtraHeaders.
//
// This test would have caught the EC2 regression on a unit-test budget.
func TestWarmHeadPreservesHFLinkedHeaders(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	const (
		linkedEtag = "deadbeefcafef00d1234567890abcdef1234567890abcdef1234567890abcdef"
		linkedSize = "16777216"
		commitSha  = "abcdef1234567890abcdef1234567890abcdef12"
	)
	loUp := &locUpstream{
		target: "https://cas-bridge.xethub.hf.co/xet-bridge-us/abc/" + linkedEtag + "?X-Amz-Date=20260517T000000Z",
		hfHeaders: map[string]string{
			"X-Linked-Etag": linkedEtag,
			"X-Linked-Size": linkedSize,
			"X-Repo-Commit": commitSha,
		},
	}

	// Phase 1: ONLINE proxy populates the redirect cache.  Use the
	// regular online handler so the cold response goes through the
	// real forward()/cacheRedirect() path.
	onlineCfg, err := config.ParseFlags(flag.NewFlagSet("online-warm-hf", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", cacheDir,
		"-public-base-url", "http://test.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewStore(onlineCfg.CacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	onlineH := proxy.NewHandler(onlineCfg, store, loUp, logx.New("error"))
	onlineSrv := httptest.NewServer(onlineH)
	t.Cleanup(onlineSrv.Close)
	onlineClient := &http.Client{
		Transport: &http.Transport{DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	const path = "/Qwen/Qwen2.5-7B-Instruct/resolve/main/model-00001-of-00002.safetensors"
	resp, err := onlineClient.Get(onlineSrv.URL + path)
	if err != nil {
		t.Fatalf("cold GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("cold GET: status=%d, want 302", resp.StatusCode)
	}
	// Sanity: the cold path must already pass these through (this is
	// what cold Python sees and validates successfully).
	for _, k := range []string{"X-Linked-Etag", "X-Linked-Size", "X-Repo-Commit"} {
		if resp.Header.Get(k) == "" {
			t.Fatalf("cold GET: missing %s in upstream-rewritten response", k)
		}
	}

	// Phase 2: OFFLINE proxy on the same cache dir.  Issue the HEAD
	// huggingface_hub issues and assert the linked-* headers survived
	// the round-trip through the redirect cache.
	panicUp := &panickingUpstream{t: t}
	c, base, stop := newOfflineProxyOnExistingCache(t, cacheDir, panicUp)
	defer stop()
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	req, _ := http.NewRequest(http.MethodHead, base+path, nil)
	headResp, err := c.Do(req)
	if err != nil {
		t.Fatalf("offline HEAD: %v", err)
	}
	_ = headResp.Body.Close()

	if headResp.StatusCode != http.StatusFound {
		t.Fatalf("offline HEAD: status=%d, want 302", headResp.StatusCode)
	}
	if got := headResp.Header.Get("X-Linked-Etag"); got != linkedEtag {
		t.Fatalf("X-Linked-Etag: got %q, want %q (this is the LocalEntryNotFoundError regression)", got, linkedEtag)
	}
	if got := headResp.Header.Get("X-Linked-Size"); got != linkedSize {
		t.Fatalf("X-Linked-Size: got %q, want %q", got, linkedSize)
	}
	if got := headResp.Header.Get("X-Repo-Commit"); got != commitSha {
		t.Fatalf("X-Repo-Commit: got %q, want %q", got, commitSha)
	}
	if got := headResp.Header.Get("Location"); got == "" {
		t.Fatal("offline HEAD: missing Location")
	}
}

// TestOfflineHeadOnShaResolvesAliasToMain is the regression test for the
// "offline_504 on /resolve/<commit-sha>/" production failure.  huggingface_hub
// downloads via /resolve/main/<file> on the cold pass, but on every
// subsequent run it switches to /resolve/<X-Repo-Commit>/<file> for HEAD
// validation.  The cold cache was indexed under "main", so the HEAD
// landed on a different key and 504'd in offline mode.  The fix writes
// an alias meta at the SHA-pinned key pointing at the canonical (main)
// entry, so both paths converge on the same body without duplicating it
// on disk.
func TestOfflineHeadOnShaResolvesAliasToMain(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	const (
		linkedEtag = "deadbeefcafef00d1234567890abcdef1234567890abcdef1234567890abcdef"
		linkedSize = "16777216"
		commitSha  = "a09a35458c702b33eeacc393d103063234e8bc28"
	)
	loUp := &locUpstream{
		target: "https://cas-bridge.xethub.hf.co/xet-bridge-us/abc/" + linkedEtag + "?X-Amz-Date=20260517T000000Z",
		hfHeaders: map[string]string{
			"X-Linked-Etag": linkedEtag,
			"X-Linked-Size": linkedSize,
			"X-Repo-Commit": commitSha,
		},
	}

	// Phase 1: cold GET on /resolve/main/<file> populates the redirect
	// cache under the symbolic-rev key.  cacheRedirect() should ALSO
	// write a SHA-pinned alias because the response carries X-Repo-Commit.
	onlineCfg, err := config.ParseFlags(flag.NewFlagSet("online-sha-alias", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", cacheDir,
		"-public-base-url", "http://test.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewStore(onlineCfg.CacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	onlineH := proxy.NewHandler(onlineCfg, store, loUp, logx.New("error"))
	onlineSrv := httptest.NewServer(onlineH)
	t.Cleanup(onlineSrv.Close)
	onlineClient := &http.Client{
		Transport: &http.Transport{DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	const mainPath = "/Qwen/Qwen2.5-7B-Instruct/resolve/main/model.safetensors.index.json"
	resp, err := onlineClient.Get(onlineSrv.URL + mainPath)
	if err != nil {
		t.Fatalf("cold GET on /resolve/main/: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("cold GET status=%d, want 302", resp.StatusCode)
	}

	// Phase 2: offline proxy on the same cache dir.  Issue the HEAD
	// huggingface_hub issues -- on /resolve/<SHA>/<file>, NOT /resolve/main.
	// The alias must catch this and serve the cached redirect.
	panicUp := &panickingUpstream{t: t}
	c, base, stop := newOfflineProxyOnExistingCache(t, cacheDir, panicUp)
	defer stop()
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	shaPath := "/Qwen/Qwen2.5-7B-Instruct/resolve/" + commitSha + "/model.safetensors.index.json"
	for _, m := range []string{http.MethodHead, http.MethodGet} {
		req, _ := http.NewRequest(m, base+shaPath, nil)
		got, err := c.Do(req)
		if err != nil {
			t.Fatalf("offline %s on /resolve/<sha>/: %v", m, err)
		}
		_ = got.Body.Close()
		if got.StatusCode != http.StatusFound {
			t.Fatalf("offline %s on /resolve/<sha>/: status=%d, want 302 (alias miss → 504 is the regression)", m, got.StatusCode)
		}
		if loc := got.Header.Get("Location"); loc == "" {
			t.Fatalf("offline %s on /resolve/<sha>/: missing Location", m)
		}
		if got.Header.Get("X-Linked-Etag") != linkedEtag {
			t.Fatalf("offline %s on /resolve/<sha>/: X-Linked-Etag missing/wrong (alias should preserve extra headers)", m)
		}
	}
}

// TestOfflineMetadataHitServes verifies the production-critical contract
// also covers metadata routes (revision / tree / etc.).  This is the bug
// the EC2 bench surfaced: cold proxy populated artifact bodies but did NOT
// cache metadata, so an offline proxy 504'd at the very first request both
// clients make (revision resolution / file listing).
//
// Expected behavior after this fix:
//   - cold:  metadata 200s are tee'd to disk like artifacts.
//   - warm (offline default): metadata serves from disk; 0 upstream calls.
//   - warm with -offline=false: metadata refreshes upstream (freshness).
func TestOfflineMetadataHitServes(t *testing.T) {
	body := []byte(`{"_id":"abc","sha":"deadbeef","siblings":[{"rfilename":"x.bin"}]}`)
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	online := &fakeUpstream{}
	online.set(&fakeResp{
		status:      200,
		body:        body,
		contentType: "application/json",
		etag:        `"v1"`,
	})

	// Phase 1: ONLINE proxy populates metadata cache.
	onlineCfg, err := config.ParseFlags(flag.NewFlagSet("online-md", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", cacheDir,
		"-public-base-url", "http://test.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewStore(onlineCfg.CacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	onlineH := proxy.NewHandler(onlineCfg, store, online, logx.New("error"))
	onlineSrv := httptest.NewServer(onlineH)
	t.Cleanup(onlineSrv.Close)
	onlineClient := &http.Client{Transport: &http.Transport{DisableCompression: true}}

	const metaPath = "/api/models/openai-community/gpt2/tree/main"
	if status, got := drainGet(t, onlineClient, onlineSrv.URL, metaPath, nil); status != 200 || !bytes.Equal(got, body) {
		t.Fatalf("online cold metadata: status=%d body_len=%d", status, len(got))
	}
	if online.fetches.Load() != 1 {
		t.Fatalf("cold metadata should have fetched once, got %d", online.fetches.Load())
	}

	// Same proxy, second GET: default -offline serves cached metadata.
	if status, got := drainGet(t, onlineClient, onlineSrv.URL, metaPath, nil); status != 200 || !bytes.Equal(got, body) {
		t.Fatalf("warm metadata cache hit: status=%d body_len=%d", status, len(got))
	}
	if online.fetches.Load() != 1 {
		t.Fatalf("warm metadata should not refetch upstream, got %d fetches", online.fetches.Load())
	}

	// Phase 2: STRICT-OFFLINE proxy on the same cache dir; upstream panics.
	panicUp := &panickingUpstream{t: t}
	c, base, stop := newOfflineProxyOnExistingCache(t, cacheDir, panicUp)
	defer stop()

	status, got := drainGet(t, c, base, metaPath, nil)
	if status != 200 {
		t.Fatalf("offline metadata GET: got status=%d, want 200 (cache hit)", status)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("offline metadata GET body mismatch:\n  got=%s\n  want=%s", got, body)
	}
}
