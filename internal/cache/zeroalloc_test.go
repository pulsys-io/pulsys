// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"bytes"
	"runtime"
	"testing"
)

// TestLoadMetaWarmHitZeroAllocs is the structural guard for the
// warm-cache hot path.  The Store.LoadMeta call on a process-warm
// key returns a *Meta from the in-memory metaCache without
// touching the filesystem and without allocating; if that ever
// drifts to even 1 alloc/op, this test fails so the regression
// shows up at commit time rather than at production benchmark
// time.
//
// Mechanism: run testing.Benchmark with a tiny b.N (1024) and
// assert AllocsPerOp() == 0.  This is faster than `go test
// -bench` (which iterates until time-stable) but adequate for a
// floor check.
func TestLoadMetaWarmHitZeroAllocs(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	key := KeyHex("GET", "huggingface.co", "/zero-alloc/x", "", "")
	body := bytes.Repeat([]byte("z"), 1024)
	if _, err := s.WriteFullFromStream(
		key, 200, "huggingface.co", "/zero-alloc/x",
		"", "", "application/octet-stream",
		bytes.NewReader(body), int64(len(body)),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Prime the metaCache so the bench measures the warm path,
	// not the first-touch cold path.
	if _, err := s.LoadMeta(key); err != nil {
		t.Fatalf("prime: %v", err)
	}

	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		var meta *Meta
		for i := 0; i < b.N; i++ {
			meta, _ = s.LoadMeta(key)
		}
		runtime.KeepAlive(meta)
	})
	if allocs := res.AllocsPerOp(); allocs != 0 {
		t.Fatalf("Store.LoadMeta warm hit must be 0 allocs/op, got %d", allocs)
	}
}

// TestTierLoadMetaWarmHitZeroAllocs guards the Tier-orchestrated
// warm path: the policy OnHit hook is allowed to observe but must
// not allocate.  NoopPolicy.OnHit is a no-op so the test asserts
// 0 allocs/op for `Tier.LoadMeta` on a hot hit.  A future Policy
// implementation that allocates per hit (e.g. for tracing) would
// fail here, prompting either an explicit opt-in or a pooled
// allocator.
func TestTierLoadMetaWarmHitZeroAllocs(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	key := KeyHex("GET", "huggingface.co", "/zero-alloc/tier", "", "")
	body := bytes.Repeat([]byte("z"), 256)
	if _, err := s.WriteFullFromStream(
		key, 200, "huggingface.co", "/zero-alloc/tier",
		"", "", "application/octet-stream",
		bytes.NewReader(body), int64(len(body)),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tier := NewTier(s, NoopPolicy{})
	if _, err := tier.LoadMeta(key); err != nil {
		t.Fatalf("prime: %v", err)
	}

	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		var meta *Meta
		for i := 0; i < b.N; i++ {
			meta, _ = tier.LoadMeta(key)
		}
		runtime.KeepAlive(meta)
	})
	if allocs := res.AllocsPerOp(); allocs != 0 {
		t.Fatalf("Tier.LoadMeta warm hit must be 0 allocs/op (NoopPolicy), got %d", allocs)
	}
}
