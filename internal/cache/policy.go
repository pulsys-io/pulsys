// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

// Policy is the tier-policy contract.  Implementations observe
// the read path of a Tier and, optionally, mutate the hot backend
// on a miss (e.g. by populating from a cold backend).
//
// Hooks MUST be cheap on the warm-hit path: OnHit fires on every
// successful LoadMeta and is therefore inside the hot path budget.
// OnMiss is allowed to perform I/O because by definition a miss
// has already paid the cost of an open(2) + read(2) failure.
//
// All methods receive the cache key and (for OnHit / OnMiss) the
// HotBackend they may write through.  Implementations must NOT
// retain *Meta beyond the call: callers may treat the value as
// read-only and clone-on-write under their own lock.
type Policy interface {
	// OnHit is invoked after a successful LoadMeta returns a
	// non-nil *Meta from the hot backend.
	OnHit(key string, m *Meta)

	// OnMiss is invoked after a LoadMeta returns (nil, nil) from
	// the hot backend.  Implementations may populate the hot
	// tier here (e.g. from a cold backend) and return the
	// freshly written *Meta; returning (nil, nil) leaves the
	// miss in place.  Any error is propagated to the caller.
	OnMiss(key string, hot HotBackend) (*Meta, error)

	// OnEvict is invoked when the hot tier sheds an entry (LRU
	// pressure, manual purge, etc.).  Cold-tier policies use
	// this in P11 to demote evicted bodies to S3 before the
	// local copy is removed.
	OnEvict(key string)
}

// NoopPolicy is the P1 default: no observation, no promotion, no
// demotion.  Allocates nothing; safe to share across Tiers.
type NoopPolicy struct{}

// Compile-time assertion that NoopPolicy satisfies Policy.
var _ Policy = NoopPolicy{}

// OnHit is a no-op.
func (NoopPolicy) OnHit(string, *Meta) {}

// OnMiss is a no-op: returns (nil, nil) so the caller keeps the miss.
func (NoopPolicy) OnMiss(string, HotBackend) (*Meta, error) { return nil, nil }

// OnEvict is a no-op.
func (NoopPolicy) OnEvict(string) {}
