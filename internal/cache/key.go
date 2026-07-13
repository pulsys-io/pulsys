// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
)

// contentAddressedHosts are upstream hostnames whose URLs are
// content-addressed: the path includes the SHA256 (or equivalent) of the
// file body, while the query string carries a short-lived presigned
// signature that changes on every redirect.  For cache-key purposes we
// MUST normalise the query to "" so that the cold and warm requests for
// the same file hash to the same cache slot.
//
// Without this normalisation:
//   - Hugging Face Xet (cas-bridge.xethub.hf.co) and legacy LFS
//     (cdn-lfs.huggingface.co) re-issue a fresh AWS presigned URL on
//     every 302 from the hub.
//   - Each fresh redirect produces a unique rawQuery → unique key →
//     forced cache miss → re-fetch of the entire (potentially multi-GB)
//     body, defeating the cache for exactly the files that benefit from
//     it most.
//
// This list is intentionally tight; do not add hosts whose query
// parameters carry semantic information (e.g. ?revision=...).
var contentAddressedHosts = map[string]struct{}{
	"cas-bridge.xethub.hf.co": {},
	"cas-server.xethub.hf.co": {},
	"cdn-lfs.huggingface.co":  {},
	"cdn-lfs-us-1.hf.co":      {},
	"cdn-lfs-us-2.hf.co":      {},
	"cdn-lfs-eu-1.hf.co":      {},
	"lfs.huggingface.co":      {},
}

// IsContentAddressedHost reports whether host's URLs identify content
// solely by their path (and the query is short-lived presign noise).
// Callers normalise rawQuery to "" before computing the cache key for
// these hosts.
func IsContentAddressedHost(host string) bool {
	_, ok := contentAddressedHosts[strings.ToLower(strings.TrimSpace(host))]
	return ok
}

// keyBufPool provides scratch buffers for KeyHex assembly.  Profiling showed
// the previous fmt.Sprintf-based implementation allocating one transient
// string per request — the pool drops that to a single 64-byte heap escape
// (the returned key string itself).
var keyBufPool = sync.Pool{
	New: func() any { b := make([]byte, 0, 512); return &b },
}

// KeyHex returns a stable, hex-encoded sha256 cache key for the logical
// object identified by (method, upstreamHost, path, rawQuery, authHeader).
//
// Inputs are normalised the same way the proxy normalises requests:
//   - method:       upper-case, trimmed
//   - upstreamHost: lower-case, trimmed
//   - path:         trimmed; "" becomes "/"
//   - rawQuery:     used verbatim
//   - authHeader:   bucketed via writeAuthBucket so the token never appears
//     in the key material on disk
//
// The output is 64 lower-case hex characters and is byte-for-byte
// compatible with the previous fmt.Sprintf("%s|%s|%s|%s|%s", …)-based
// implementation, so existing cache directories continue to work.
func KeyHex(method, upstreamHost, path, rawQuery, authHeader string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	upstreamHost = strings.ToLower(strings.TrimSpace(upstreamHost))
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/"
	}

	bufp := keyBufPool.Get().(*[]byte)
	buf := (*bufp)[:0]
	buf = append(buf, method...)
	buf = append(buf, '|')
	buf = append(buf, upstreamHost...)
	buf = append(buf, '|')
	buf = append(buf, path...)
	buf = append(buf, '|')
	buf = append(buf, rawQuery...)
	buf = append(buf, '|')
	buf = appendAuthBucket(buf, authHeader)

	sum := sha256.Sum256(buf)

	*bufp = buf[:0]
	keyBufPool.Put(bufp)

	var out [sha256.Size * 2]byte
	hex.Encode(out[:], sum[:])
	return string(out[:])
}

// appendAuthBucket appends a stable bucket identifier for the supplied auth
// header to buf and returns the extended slice.  Anonymous requests share a
// single "anon" bucket.  Other auth values are bucketed by the first 8 bytes
// (16 hex chars) of their sha256 so the token itself is never persisted.
func appendAuthBucket(buf []byte, auth string) []byte {
	a := strings.TrimSpace(auth)
	if a == "" {
		return append(buf, "anon"...)
	}
	sum := sha256.Sum256([]byte(a))
	var hexBuf [16]byte
	hex.Encode(hexBuf[:], sum[:8])
	return append(buf, hexBuf[:]...)
}

// HashAuth returns the same 16-hex-char bucket identifier that goes into
// KeyHex for the supplied raw token (no "Bearer " prefix; pass the bare
// token bytes).  Exposed for diagnostic logging so two requests can be
// correlated by auth identity without ever logging the token itself.
func HashAuth(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	var out [16]byte
	hex.Encode(out[:], sum[:8])
	return string(out[:])
}

// Span is a half-open byte interval [Start, End) present on disk.
type Span struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// MergeSpans returns merged non-overlapping spans sorted by Start.
//
// The 0- and 1-span cases short-circuit without allocating: a fully cached
// 200-style object only ever has a single span, so the warm hit path never
// hits the sort+copy hot loop that pprof flagged at ~2.3% of allocs.
func MergeSpans(in []Span) []Span {
	switch len(in) {
	case 0:
		return nil
	case 1:
		return in
	}
	if isSortedAndDisjoint(in) {
		return in
	}
	cp := append([]Span(nil), in...)
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].Start == cp[j].Start {
			return cp[i].End < cp[j].End
		}
		return cp[i].Start < cp[j].Start
	})
	out := cp[:1]
	for _, s := range cp[1:] {
		last := &out[len(out)-1]
		if s.Start <= last.End {
			if s.End > last.End {
				last.End = s.End
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// isSortedAndDisjoint reports whether spans are already in canonical form
// (sorted by Start with no overlapping or touching neighbors).  When true,
// MergeSpans can return the input slice directly with zero allocations.
func isSortedAndDisjoint(spans []Span) bool {
	for i := 1; i < len(spans); i++ {
		if spans[i].Start < spans[i-1].End {
			return false
		}
	}
	return true
}

// Covers reports whether spans fully cover [start, end) (half-open).
//
// The single-span case (the common warm hit) takes a zero-allocation
// fast path that neither sorts nor reslices.
func Covers(spans []Span, start, end int64) bool {
	if start >= end {
		return true
	}
	if len(spans) == 1 {
		s := spans[0]
		return start >= s.Start && end <= s.End
	}
	sp := MergeSpans(spans)
	for _, s := range sp {
		if start >= s.End {
			continue
		}
		if start < s.Start {
			return false
		}
		if end <= s.End {
			return true
		}
		if s.End >= end {
			return true
		}
		start = s.End
		if start >= end {
			return true
		}
	}
	return false
}
