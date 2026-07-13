// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package auth

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"
)

// PATGate enforces "Bearer pulsys_..." authentication on the pulsys
// data plane.  It satisfies the coreserver.AuthGate duck-typed
// interface (Check(ctx, auth, path) -> (status, reason)).
//
// Policy:
//
//   - /healthz, /readyz, /metrics are admitted unconditionally so
//     load balancers and Prometheus scrapers don't need credentials.
//
//   - Every other path requires an Authorization header of the form
//     "Bearer pulsys_<prefix>_<secret>".  Missing, malformed, or
//     unrecognized tokens are rejected with 401.
//
//   - LookupAPIToken results are cached in a small TTL map keyed by
//     the sha256 of the raw bearer bytes.  This collapses the typical
//     hf-cli pull (one PAT, hundreds of range requests) to a single
//     Postgres roundtrip per PositiveTTL window without ever caching
//     plaintext.  Revocation propagates within PositiveTTL (default
//     60 s) -- acceptable for the threat model where the admin UI
//     immediately flips revoked_at in Postgres and the next lookup
//     after the TTL closes the door.  Negative results are not
//     cached, so unknown tokens always pay the lookup; this is the
//     expensive case for an attacker spamming forged tokens, which
//     is acceptable.
//
// PATGate is safe for concurrent use; the internal cache uses a
// sync.Mutex around a small map.  Production warm-hit throughput
// (typically 1 PAT in steady state) makes lock contention
// negligible.
type PATGate struct {
	Store Store

	// PositiveTTL is the lifetime of a cached "this token is valid"
	// entry.  Zero defaults to 60 seconds.
	PositiveTTL time.Duration

	// Clock is used for cache expiry.  Zero defaults to time.Now;
	// tests override to drive expiry deterministically.
	Clock func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]patCacheEntry
}

type patCacheEntry struct {
	expires time.Time
}

// NewPATGate constructs a PATGate with default TTLs against store.
// Pass it as Server.AuthGate on the coreserver.
func NewPATGate(store Store) *PATGate {
	return &PATGate{Store: store}
}

// Paths that bypass the gate.  Kept as small byte literals so the
// hot-path check is one bytes.Equal per allowlist entry.
var (
	gateAllowHealthz = []byte("/healthz")
	gateAllowReadyz  = []byte("/readyz")
	gateAllowMetrics = []byte("/metrics")
)

// Check implements the coreserver.AuthGate contract.
func (g *PATGate) Check(ctx context.Context, auth, path []byte) (int, string) {
	if bytes.Equal(path, gateAllowHealthz) ||
		bytes.Equal(path, gateAllowReadyz) ||
		bytes.Equal(path, gateAllowMetrics) {
		return 0, ""
	}
	tok, ok := bearerFromBytes(auth)
	if !ok {
		return 401, "missing Authorization (expected Bearer pulsys_...)"
	}
	if !IsPAT(tok) {
		return 401, "unsupported credential (expected Bearer pulsys_...)"
	}
	if g.lookupCached(tok) {
		return 0, ""
	}
	if _, err := g.Store.LookupAPIToken(ctx, tok); err != nil {
		if errors.Is(err, ErrInvalidSession) {
			return 401, "invalid or revoked token"
		}
		return 401, "authentication failed"
	}
	g.storeCached(tok)
	return 0, ""
}

func (g *PATGate) clock() time.Time {
	if g.Clock != nil {
		return g.Clock()
	}
	return time.Now()
}

func (g *PATGate) positiveTTL() time.Duration {
	if g.PositiveTTL > 0 {
		return g.PositiveTTL
	}
	return 60 * time.Second
}

func (g *PATGate) lookupCached(token string) bool {
	h := TokenHash(token)
	var key [32]byte
	copy(key[:], h)
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.cache[key]
	if !ok {
		return false
	}
	if g.clock().After(e.expires) {
		delete(g.cache, key)
		return false
	}
	return true
}

func (g *PATGate) storeCached(token string) {
	h := TokenHash(token)
	var key [32]byte
	copy(key[:], h)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cache == nil {
		g.cache = make(map[[32]byte]patCacheEntry, 4)
	}
	g.cache[key] = patCacheEntry{expires: g.clock().Add(g.positiveTTL())}
}

// Invalidate evicts the cached entry for token (if any).  Called by
// the admin token-revoke handler to make revocation take effect
// immediately on the local proxy without waiting for PositiveTTL to
// expire.  Cross-proxy invalidation (multiple proxy nodes behind one
// LB) is intentionally out of scope here -- a future change will add
// a Postgres LISTEN/NOTIFY bus or a Redis pub/sub for that.
func (g *PATGate) Invalidate(token string) {
	h := TokenHash(token)
	g.InvalidateByHash(h)
}

// InvalidateByHash evicts the cached entry whose key matches the
// supplied sha256 hash.  Required by the admin revoke path which
// holds the stored hash (not the plaintext) and must close the
// 60-second post-revoke admit window that the original
// 2026-05-21 incident chained off.  Safe to call with a nil or
// wrong-length hash (no-op).
func (g *PATGate) InvalidateByHash(hash []byte) {
	if len(hash) != 32 {
		return
	}
	var key [32]byte
	copy(key[:], hash)
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.cache, key)
}

// bearerFromBytes extracts the token after "Bearer " from raw header
// bytes.  Returns "", false on miss.  Case-insensitive on the scheme
// per RFC 7235.
func bearerFromBytes(auth []byte) (string, bool) {
	const prefix = "Bearer "
	if len(auth) < len(prefix) {
		return "", false
	}
	for i := 0; i < len(prefix); i++ {
		a := auth[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		b := prefix[i]
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return "", false
		}
	}
	tok := bytes.TrimSpace(auth[len(prefix):])
	if len(tok) == 0 {
		return "", false
	}
	return string(tok), true
}
