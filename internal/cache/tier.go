// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

// Tier is the policy-aware orchestration over a HotBackend.
//
// P1 (this phase) wires Tier as a thin pass-through to a single
// HotBackend, but the type already exposes the seams a future
// cold tier (P11) needs:
//
//   - The Policy hooks (OnMiss / OnHit / OnEvict) fire from the
//     read paths so a cold-tier implementation can populate the
//     hot tier on a miss without further interface churn.
//   - The Hot() accessor lets call sites that want raw access
//     (e.g. low-level benchmarks) bypass the policy machinery.
//
// Tier deliberately does NOT replace *Store at any consumer site
// yet: the proxy handler, coreserver, and main still hold a
// *cache.Store directly.  That migration is part of P11 once a
// concrete ColdBackend exists; until then Tier is a stake in the
// ground that locks the contract.
type Tier struct {
	hot    HotBackend
	policy Policy
}

// NewTier returns a hot-tier-only Tier wrapping hot with the
// supplied policy.  Pass NoopPolicy{} for the default (no
// promotion, no demotion, no observation).
func NewTier(hot HotBackend, policy Policy) *Tier {
	if hot == nil {
		panic("cache.NewTier: hot backend is nil")
	}
	if policy == nil {
		policy = NoopPolicy{}
	}
	return &Tier{hot: hot, policy: policy}
}

// Hot returns the underlying HotBackend.  Use this for hot-path
// reads that must NOT pay the policy hook cost (e.g. zero-alloc
// benchmarks).  All other callers should prefer the Tier methods
// so the policy machinery runs.
func (t *Tier) Hot() HotBackend { return t.hot }

// Policy returns the configured Policy.  Exposed for tests and
// for future Tier-aware diagnostics.
func (t *Tier) Policy() Policy { return t.policy }

// LoadMeta calls the hot backend's LoadMeta and routes the result
// through the policy hooks.
//
//   - (meta != nil, err == nil)  -> policy.OnHit
//   - (meta == nil, err == nil)  -> policy.OnMiss
//   - (err != nil)               -> hooks are NOT called; the error
//     is propagated unchanged
//
// On a miss the policy MAY (in P11) populate the hot tier from a
// cold backend and return the freshly written *Meta.  For P1 the
// NoopPolicy returns (nil, nil) and the miss propagates.
func (t *Tier) LoadMeta(key string) (*Meta, error) {
	m, err := t.hot.LoadMeta(key)
	if err != nil {
		return nil, err
	}
	if m != nil {
		t.policy.OnHit(key, m)
		return m, nil
	}
	if pm, perr := t.policy.OnMiss(key, t.hot); perr != nil {
		return nil, perr
	} else if pm != nil {
		return pm, nil
	}
	return nil, nil
}
