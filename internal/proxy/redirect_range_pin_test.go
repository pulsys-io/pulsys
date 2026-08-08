// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pulsys-io/pulsys/internal/upstream"
)

// pinningUpstream models how Hugging Face signs Xet CDN redirects: the
// CloudFront policy embeds a ByteRange condition naming the exact Range
// header of the request that produced it, so the resulting URL is only
// usable for that one chunk.  A resolve issued with no Range comes back
// without the condition and is good for every chunk.
type pinningUpstream struct {
	ranged   atomic.Int64
	unranged atomic.Int64
}

const (
	pinnedPolicyPrefix = "Policy=PINNED-"
	unpinnedPolicy     = "Policy=UNPINNED"
)

func (p *pinningUpstream) Do(_ context.Context, _, _, _, _ string, hdr http.Header, _ []byte) (*upstream.Response, error) {
	policy := unpinnedPolicy
	if rng := hdr.Get("Range"); rng != "" {
		policy = pinnedPolicyPrefix + rng
		p.ranged.Add(1)
	} else {
		p.unranged.Add(1)
	}
	h := http.Header{}
	h.Set("Location", "https://us.aws.cdn.hf.co/xet-bridge-us/66e8/ddfa?"+policy+"&Signature=MEUCIQ")
	h.Set("Content-Length", "0")
	return &upstream.Response{
		Status:        http.StatusFound,
		Header:        h,
		ContentLength: 0,
		Body:          io.NopCloser(strings.NewReader("")),
	}, nil
}

// TestCachedRedirectIsNotRangePinned pins the fix for the import failure
// where a Xet-backed repo died a few hundred MB in with
// "403 Auth failed: invalid range" from us.aws.cdn.hf.co, surfaced to the
// operator as a rejected Hugging Face token.
//
// The redirect cache key deliberately excludes the Range header, so the
// first ranged chunk's 302 was replayed to every other chunk.  Because
// that Location was signed for `bytes=0-...` only, the CDN rejected all
// of them.  The cached Location must therefore be range-agnostic.
func TestCachedRedirectIsNotRangePinned(t *testing.T) {
	fake := &pinningUpstream{}
	client, base, cleanup := newProxyServer(t, fake)
	defer cleanup()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	const path = "/Qwen/Qwen2.5-7B-Instruct/resolve/main/model-00001-of-00004.safetensors"

	// Chunk 1 is a cold miss.  It must receive the Location signed for
	// its own range -- that one is valid and the client is about to
	// follow it.
	first := redirectLocation(t, client, base+path, "bytes=0-16777215")
	if !strings.Contains(first, pinnedPolicyPrefix+"bytes=0-16777215") {
		t.Fatalf("cold ranged request should get its own signed Location, got %q", first)
	}

	// Chunk 2 is served from the redirect cache.  Replaying chunk 1's
	// pinned signature here is the bug: the CDN answers 403.
	second := redirectLocation(t, client, base+path, "bytes=16777216-33554431")
	if strings.Contains(second, pinnedPolicyPrefix) {
		t.Fatalf("cached redirect is range-pinned (403 'Auth failed: invalid range' for every other chunk): %q", second)
	}
	if !strings.Contains(second, unpinnedPolicy) {
		t.Fatalf("cached redirect Location = %q, want the unranged signature %q", second, unpinnedPolicy)
	}

	// Cost of the fix: exactly one extra unranged resolve, on the cold
	// path only.  Chunk 2 must not touch upstream at all.
	if got := fake.unranged.Load(); got != 1 {
		t.Errorf("unranged resolves = %d, want 1", got)
	}
	if got := fake.ranged.Load(); got != 1 {
		t.Errorf("ranged resolves = %d, want 1 (chunk 2 must hit the cache)", got)
	}
}

// TestCachedRedirectUnrangedRequestUnchanged guards the common path: a
// resolve with no Range never pays for the extra upstream roundtrip.
func TestCachedRedirectUnrangedRequestUnchanged(t *testing.T) {
	fake := &pinningUpstream{}
	client, base, cleanup := newProxyServer(t, fake)
	defer cleanup()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	const path = "/openai-community/gpt2/resolve/main/model.safetensors"
	if loc := redirectLocation(t, client, base+path, ""); !strings.Contains(loc, unpinnedPolicy) {
		t.Fatalf("Location = %q, want %q", loc, unpinnedPolicy)
	}
	if got := fake.unranged.Load(); got != 1 {
		t.Errorf("unranged resolves = %d, want 1 (no re-resolve when the request had no Range)", got)
	}
}

func redirectLocation(tb testing.TB, c *http.Client, url, rangeHdr string) string {
	tb.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		tb.Fatal(err)
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	resp, err := c.Do(req)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusFound {
		tb.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}
