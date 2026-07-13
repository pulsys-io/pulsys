// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache_test

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
)

// p10.2 — cache stress.
// These tests exist to surface concurrency bugs that the unit tests
// (count=1, single goroutine) cannot. They run cheap and stay
// reasonable under -race -count=10.

// TestStress_AcquireRange_DisjointInParallel asserts that two
// goroutines requesting *disjoint* ranges of the same key proceed in
// parallel - the regression we'd hate to ship is a return to the
// coarse-grained Lock(key) that pinned `hf download` to one stream.
//
// We don't measure wall-clock; we measure observed concurrency via a
// shared counter. If acquire serialized disjoint ranges, the counter
// would never exceed 1.
func TestStress_AcquireRange_DisjointInParallel(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.NewStore(filepath.Join(dir, "cache"), "none")
	if err != nil {
		t.Fatal(err)
	}

	const N = 16
	const span = 1024
	var inFlight atomic.Int64
	var peak atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			rel := store.AcquireRange("disjoint", int64(idx*span), int64((idx+1)*span))
			cur := inFlight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			// Hold the range briefly so peers can collide.
			time.Sleep(2 * time.Millisecond)
			inFlight.Add(-1)
			rel()
		}(i)
	}
	close(start)
	wg.Wait()

	if peak.Load() < 2 {
		t.Fatalf("HARD: disjoint ranges serialized, peak in-flight=%d (want >= 2)", peak.Load())
	}
}

// TestStress_AcquireRange_SameRangeSerialises proves the safety side:
// N goroutines asking for the *same* range observe strict
// serialization. Concurrent in-flight count never exceeds 1.
func TestStress_AcquireRange_SameRangeSerialises(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.NewStore(filepath.Join(dir, "cache"), "none")
	if err != nil {
		t.Fatal(err)
	}

	const N = 32
	var inFlight atomic.Int64
	var violations atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rel := store.AcquireRange("identical", 0, 1024)
			if cur := inFlight.Add(1); cur > 1 {
				violations.Add(1)
			}
			time.Sleep(time.Millisecond)
			inFlight.Add(-1)
			rel()
		}()
	}
	close(start)
	wg.Wait()
	if v := violations.Load(); v != 0 {
		t.Fatalf("HARD: %d goroutines observed concurrent in-flight for the same range", v)
	}
}

// TestStress_BodyHandleEvictionUnderLoad exercises the
// MaxBodyHandleEntries-bounded LRU under churn: more bodies than the
// LRU holds, plus N concurrent acquire/release cycles. The invariant
// we assert is that every successfully-acquired handle can be read
// from before its Close, and that no double-Close panics fire.
//
// This is the property bodyhandle relies on for bounded fd count
// under sustained burst traffic.
func TestStress_BodyHandleEvictionUnderLoad(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.NewStore(filepath.Join(dir, "cache"), "none")
	if err != nil {
		t.Fatal(err)
	}

	old := cache.MaxBodyHandleEntries
	cache.MaxBodyHandleEntries = 4
	defer func() { cache.MaxBodyHandleEntries = old }()

	// Pre-populate 32 bodies, well past the LRU cap.
	const bodies = 32
	const bodyLen = 16
	for i := 0; i < bodies; i++ {
		key := fmt.Sprintf("body-%03d", i)
		w, err := store.BeginSegment(key, cache.SegmentParams{
			Status: 200, Start: 0, Length: bodyLen, Total: bodyLen,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(bytes.Repeat([]byte{byte(i)}, bodyLen)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}

	const G = 16
	const iters = 200
	var wg sync.WaitGroup
	var readFail atomic.Int64
	var acqFail atomic.Int64

	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			buf := make([]byte, 1)
			for it := 0; it < iters; it++ {
				key := fmt.Sprintf("body-%03d", (seed*7+it)%bodies)
				h, closer, err := store.AcquireBody(key)
				if err != nil {
					acqFail.Add(1)
					continue
				}
				if _, err := h.ReadAt(buf, 0); err != nil {
					readFail.Add(1)
				}
				_ = closer.Close()
			}
		}(g)
	}
	wg.Wait()

	if acqFail.Load() != 0 {
		t.Fatalf("HARD: %d AcquireBody failures (expected 0 - bodies all on disk)", acqFail.Load())
	}
	if readFail.Load() != 0 {
		t.Fatalf("HARD: %d ReadAt failures on a live handle (eviction freed fd while ref>0)", readFail.Load())
	}
}

// TestStress_MultiKeyCrashRecovery writes 50 segments to disk via the
// normal commit path, then reopens the Store from scratch and verifies
// every committed Meta is readable - exercising the on-disk format's
// crash-cold recovery (process exits, OS reboots, sidecar restarts).
//
// This is the multi-key complement to TestSegmentCheckpointResumesAfterCrash;
// that test asserts intra-segment durability, this one asserts that
// the per-key directory layout doesn't lose entries under churn.
func TestStress_MultiKeyCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "cache")

	store1, err := cache.NewStore(root, "none")
	if err != nil {
		t.Fatal(err)
	}

	const N = 50
	for i := 0; i < N; i++ {
		key := fmt.Sprintf("crash-%03d", i)
		body := bytes.Repeat([]byte{byte(i)}, 64)
		w, err := store1.BeginSegment(key, cache.SegmentParams{
			Status:       200,
			UpstreamHost: "huggingface.co",
			Path:         fmt.Sprintf("/multi/%d", i),
			Start:        0, Length: 64, Total: 64,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Reopen the store (no in-memory state survives).
	store2, err := cache.NewStore(root, "none")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < N; i++ {
		key := fmt.Sprintf("crash-%03d", i)
		meta, err := store2.LoadMeta(key)
		if err != nil {
			t.Fatalf("LoadMeta %s: %v", key, err)
		}
		if meta == nil {
			t.Fatalf("HARD: meta missing after restart: %s", key)
		}
		if meta.StatusCode != 200 {
			t.Fatalf("status=%d want 200 (key=%s)", meta.StatusCode, key)
		}
		if len(meta.Spans) != 1 || meta.Spans[0].Start != 0 || meta.Spans[0].End != 64 {
			t.Fatalf("HARD: spans corrupted after restart: %+v (key=%s)", meta.Spans, key)
		}
	}
}

// TestStress_ConcurrentBeginSegmentDifferentKeys hammers BeginSegment +
// Close from many goroutines on distinct keys. The cache uses no per-key
// mutex inside the writer path, so this is the smoke test for fd /
// directory contention. The data correctness is asserted via Read.
func TestStress_ConcurrentBeginSegmentDifferentKeys(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.NewStore(filepath.Join(dir, "cache"), "none")
	if err != nil {
		t.Fatal(err)
	}

	const G = 32
	const perG = 8
	var wg sync.WaitGroup
	errCh := make(chan error, G*perG)

	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				key := fmt.Sprintf("k-%d-%d", seed, i)
				body := bytes.Repeat([]byte{byte(seed)}, 32)
				w, err := store.BeginSegment(key, cache.SegmentParams{
					Status: 200, Start: 0, Length: 32, Total: 32,
				})
				if err != nil {
					errCh <- fmt.Errorf("begin %s: %w", key, err)
					return
				}
				if _, err := w.Write(body); err != nil {
					errCh <- fmt.Errorf("write %s: %w", key, err)
					return
				}
				if err := w.Close(); err != nil {
					errCh <- fmt.Errorf("close %s: %w", key, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	// Spot-check a sample of keys for correct contents.
	for g := 0; g < G; g++ {
		key := fmt.Sprintf("k-%d-0", g)
		h, closer, err := store.AcquireBody(key)
		if err != nil {
			t.Fatalf("acquire %s: %v", key, err)
		}
		buf := make([]byte, 32)
		if _, err := h.ReadAt(buf, 0); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		_ = closer.Close()
		if !bytes.Equal(buf, bytes.Repeat([]byte{byte(g)}, 32)) {
			t.Fatalf("HARD: %s body corrupt under concurrent writes", key)
		}
	}
}
