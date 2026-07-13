// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package classify

import (
	"strings"
)

// ArtifactGET reports whether a client GET should be treated as download-class
// (mandatory disk tee on upstream miss).
//
// Hugging Face exposes file content under several URL patterns that have
// evolved over the lifetime of the python `huggingface_hub` library:
//
//   - /<repo>/resolve/<rev>/<file>           — classic resolve URL
//   - /<repo>/resolve/<rev>/<file>?...       — same with query
//   - /<repo>/resolve%2F<rev>/...            — percent-encoded variant
//   - /api/resolve-cache/models/<repo>/<rev>/<file>  — newer (huggingface_hub
//     >= 0.20) "Xet-aware" resolve-cache endpoint that returns the file
//     body directly (no redirect to LFS), under /api/.
//   - /api/resolve-cache/datasets|spaces/...  — same for datasets / spaces
//   - /info/lfs/...                          — LFS protocol
//   - any non-default host (LFS / CDN / Xet) — treated as artifact unless
//     it's a JSON metadata API call
//
// The arguments are strings (not []byte) so the proxy hot path can pass its
// existing string locals with zero conversion allocations — pprof flagged the
// previous []byte signature as one of the top per-request allocators.
func ArtifactGET(defaultHost, upstreamHost, method, path string) bool {
	// Both ingress engines (coreserver fast path + net/http slow path)
	// pass canonical upper-case method strings, so the literal equality
	// is allocation-free and case-correct.
	if method != "GET" {
		return false
	}
	if strings.HasPrefix(path, "/_p/") {
		return true
	}
	if containsLower(path, "/resolve/") {
		return true
	}
	if containsLower(path, "/info/lfs/") {
		return true
	}
	if containsLower(path, "/resolve%2f") { // percent-encoded
		return true
	}
	// Newer huggingface_hub (>= 0.20) routes file content through
	// /api/resolve-cache/{models,datasets,spaces}/...  These responses
	// carry the actual file body (no LFS redirect for files served by
	// the resolve-cache endpoint), so they MUST be teed to disk.
	if containsLowerPrefix(path, "/api/resolve-cache/") {
		return true
	}
	if !equalFoldHost(upstreamHost, defaultHost) {
		// Multi-host CDN blobs (not hub API JSON).
		if containsLowerPrefix(path, "/api/") {
			return false
		}
		return true
	}
	return !containsLowerPrefix(path, "/api/")
}

// containsLower reports whether the case-insensitive substring needle (which
// MUST already be lower case) appears in haystack, without allocating a
// lower-cased copy of haystack.
func containsLower(haystack, needleLower string) bool {
	if len(needleLower) == 0 {
		return true
	}
	for i := 0; i+len(needleLower) <= len(haystack); i++ {
		if equalFoldASCII(haystack[i:i+len(needleLower)], needleLower) {
			return true
		}
	}
	return false
}

// containsLowerPrefix reports whether haystack begins (case-insensitive) with
// prefix.  prefix MUST already be lower case.
func containsLowerPrefix(haystack, prefixLower string) bool {
	if len(haystack) < len(prefixLower) {
		return false
	}
	return equalFoldASCII(haystack[:len(prefixLower)], prefixLower)
}

// equalFoldASCII compares two ASCII-only strings case-insensitively without
// allocating.  b MUST already be lower case.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca := a[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if ca != b[i] {
			return false
		}
	}
	return true
}

// equalFoldHost compares two host names case-insensitively, ignoring leading
// and trailing whitespace.  Hosts are ASCII so we avoid the heavier
// strings.EqualFold path.
func equalFoldHost(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
