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
	"strconv"
	"sync"
	"testing"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/logx"
	"github.com/pulsys-io/pulsys/internal/proxy"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

// recordingUpstream captures the Authorization header and host of the most
// recent upstream Do() call so credential-forwarding behavior can be asserted.
type recordingUpstream struct {
	mu   sync.Mutex
	auth string
	host string
	body []byte
}

func (u *recordingUpstream) Do(_ context.Context, _, host, _, _ string, hdr http.Header, _ []byte) (*upstream.Response, error) {
	u.mu.Lock()
	u.auth = hdr.Get("Authorization")
	u.host = host
	u.mu.Unlock()
	h := http.Header{}
	h.Set("Content-Type", "application/octet-stream")
	h.Set("ETag", `"x"`)
	h.Set("Content-Length", strconv.Itoa(len(u.body)))
	return &upstream.Response{
		Status:        http.StatusOK,
		Header:        h,
		ContentLength: int64(len(u.body)),
		Body:          io.NopCloser(bytes.NewReader(u.body)),
	}, nil
}

func (u *recordingUpstream) lastAuth() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.auth
}

// newRecordingProxy wires a proxy.Handler whose config carries the supplied
// read-only upstream token (empty = no server token configured).
func newRecordingProxy(tb testing.TB, upstreamToken string, up upstream.Client) (*http.Client, string, func()) {
	tb.Helper()
	dir := tb.TempDir()
	cfg, err := config.ParseFlags(flag.NewFlagSet("x", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", filepath.Join(dir, "cache"),
		"-public-base-url", "http://test.local",
	})
	if err != nil {
		tb.Fatal(err)
	}
	cfg.UpstreamHFToken = upstreamToken
	store, err := cache.NewStore(cfg.CacheDir, "none")
	if err != nil {
		tb.Fatal(err)
	}
	h := proxy.NewHandler(cfg, store, up, logx.New("error"))
	srv := httptest.NewServer(h)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	return client, srv.URL, func() { srv.Close() }
}

// TestForwardInjectsServerHFToken pins the credential decoupling: Pulsys
// authenticates a cold-miss read with its own read-only HF token, and the
// caller's inbound pulsys_* key must never reach Hugging Face.
func TestForwardInjectsServerHFToken(t *testing.T) {
	up := &recordingUpstream{body: bytes.Repeat([]byte("a"), 4096)}
	client, base, stop := newRecordingProxy(t, "hf_server_readonly", up)
	defer stop()

	status, _ := drainGet(t, client, base,
		"/openai-community/gpt2/resolve/main/model.bin",
		map[string]string{"Authorization": "Bearer pulsys_clientkey"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := up.lastAuth(); got != "Bearer hf_server_readonly" {
		t.Fatalf("upstream Authorization = %q; want %q (client pulsys_ key must not be forwarded)",
			got, "Bearer hf_server_readonly")
	}
}

// TestForwardNeverForwardsClientToken pins that Pulsys never forwards a
// caller's inbound Authorization upstream, even when no server token is
// configured: the client credential is always dropped, never relayed to
// Hugging Face.
func TestForwardNeverForwardsClientToken(t *testing.T) {
	up := &recordingUpstream{body: bytes.Repeat([]byte("a"), 4096)}
	client, base, stop := newRecordingProxy(t, "", up)
	defer stop()

	status, _ := drainGet(t, client, base,
		"/openai-community/gpt2/resolve/main/model.bin",
		map[string]string{"Authorization": "Bearer pulsys_clientkey"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := up.lastAuth(); got != "" {
		t.Fatalf("upstream Authorization = %q; want empty (client credential must never be forwarded)", got)
	}
}
