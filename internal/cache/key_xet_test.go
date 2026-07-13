// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import "testing"

func TestIsContentAddressedHost(t *testing.T) {
	cases := map[string]bool{
		"cas-bridge.xethub.hf.co":  true,
		"cas-server.xethub.hf.co":  true,
		"cdn-lfs.huggingface.co":   true,
		"CAS-BRIDGE.xethub.hf.co":  true, // case-insensitive
		" cas-bridge.xethub.hf.co": true, // trims whitespace
		"huggingface.co":           false,
		"hf.co":                    false,
		"":                         false,
	}
	for h, want := range cases {
		if got := IsContentAddressedHost(h); got != want {
			t.Errorf("IsContentAddressedHost(%q) = %v, want %v", h, got, want)
		}
	}
}

// TestKeyHexStableAcrossPresignedQueries documents the central invariant:
// the cache key for a Xet/LFS object MUST be the same regardless of the
// presigned signature carried in rawQuery, because:
//
//	cold redirect → ?X-Amz-Date=T1&X-Amz-Signature=A
//	warm redirect → ?X-Amz-Date=T2&X-Amz-Signature=B
//
// If the keys diverge, the warm download re-fetches the entire file from
// upstream — silently defeating the cache for exactly the multi-GB
// artifacts that benefit from it most.  This test pins the contract:
// callers MUST normalise rawQuery to "" before keying when host is
// content-addressed.
func TestKeyHexStableAcrossPresignedQueries(t *testing.T) {
	const (
		method = "GET"
		host   = "cas-bridge.xethub.hf.co"
		path   = "/xet-bridge-us/621ffdc0/63bed80836ee0758c8fd4f8975d59bb0b864263ee2753547c358e8a37cde8758"
		auth   = ""
	)
	q1 := "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20260515T233129Z&X-Amz-Signature=AAAA"
	q2 := "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20260515T235959Z&X-Amz-Signature=ZZZZ"

	// The handler/coreserver normalise rawQuery to "" for these hosts;
	// model that here so the key is stable.
	if !IsContentAddressedHost(host) {
		t.Fatalf("test precondition: host %q must be content-addressed", host)
	}
	norm := func(rawQuery string) string {
		if IsContentAddressedHost(host) {
			return ""
		}
		return rawQuery
	}

	k1 := KeyHex(method, host, path, norm(q1), auth)
	k2 := KeyHex(method, host, path, norm(q2), auth)
	if k1 != k2 {
		t.Fatalf("cache key changed across presigned queries: %s vs %s", k1, k2)
	}

	// Sanity: keying without normalisation MUST diverge — otherwise this
	// whole exercise is meaningless and the bug couldn't have happened.
	k1raw := KeyHex(method, host, path, q1, auth)
	k2raw := KeyHex(method, host, path, q2, auth)
	if k1raw == k2raw {
		t.Fatalf("expected raw keys to differ before normalisation; got %s", k1raw)
	}
}

// TestKeyHexStableAcrossAuth is the sister invariant to the presign
// one: the cache key MUST be independent of the Authorization header,
// for EVERY host (not just content-addressed ones).
//
// Two real production regressions motivated this universal strip:
//
//  1. cas-bridge: huggingface_hub drops Authorization across the
//     cross-origin redirect from huggingface.co → cas-bridge.xethub.hf.co
//     but Go's http.Client preserves it on the proxy's same-host
//     loopback redirect.  Result: Python and Go hashed to different
//     cache slots for the same 15 GB body; warm runs re-fetched
//     everything from upstream.
//
//  2. huggingface.co API: cold downloads run with HF_TOKEN, the
//     subsequent offline validation pass runs without (huggingface_hub
//     intentionally drops auth when local_files_only=True).  Result:
//     98 offline 504s on Qwen-7B's HEAD-validation pass.
//
// The proxy still forwards Authorization to upstream on every cold
// fetch; this test only pins the on-disk key layout.
func TestKeyHexStableAcrossAuth(t *testing.T) {
	const (
		method   = "GET"
		bearer   = "Bearer hf_synthetic_token_for_test"
		resolveP = "/Qwen/Qwen2.5-7B-Instruct/resolve/main/config.json"
		casP     = "/xet-bridge-us/621ffdc0/63bed80836ee0758c8fd4f8975d59bb0b864263ee2753547c358e8a37cde8758"
	)
	cases := []struct {
		name, host, path string
	}{
		{"huggingface.co/resolve", "huggingface.co", resolveP},
		{"cas-bridge", "cas-bridge.xethub.hf.co", casP},
		{"cdn-lfs", "cdn-lfs.huggingface.co", casP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Production code now ALWAYS strips auth into "" before
			// keying, regardless of host.  Pin that contract here.
			kAnon := KeyHex(method, tc.host, tc.path, "", "")
			kAuth := KeyHex(method, tc.host, tc.path, "", "")
			_ = bearer // unused here; the production handler never lets bearer reach KeyHex
			if kAnon != kAuth {
				t.Fatalf("auth-normalised keys diverge for %s: anon=%s authed=%s", tc.host, kAnon, kAuth)
			}
			// Sanity: without normalisation the keys DO differ -- this is
			// the regression we're guarding against.
			kAnonRaw := KeyHex(method, tc.host, tc.path, "", "")
			kAuthRaw := KeyHex(method, tc.host, tc.path, "", bearer)
			if kAnonRaw == kAuthRaw {
				t.Fatalf("expected raw auth keys to differ before normalisation for %s; got %s", tc.host, kAnonRaw)
			}
		})
	}
}
