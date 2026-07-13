// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestLRUEvictsLeastRecent(t *testing.T) {
	var evicted []string
	var mu sync.Mutex
	l := newLRU(3, func(k string, _ any) {
		mu.Lock()
		evicted = append(evicted, k)
		mu.Unlock()
	})

	l.Add("a", 1)
	l.Add("b", 2)
	l.Add("c", 3)
	if l.Len() != 3 {
		t.Fatalf("len=%d want 3", l.Len())
	}

	// Touch "a" to promote it past "b".  Now LRU order = b, c, a.
	if _, ok := l.Get("a"); !ok {
		t.Fatal("a missing")
	}

	l.Add("d", 4) // should evict "b"
	if l.Len() != 3 {
		t.Fatalf("len=%d want 3", l.Len())
	}
	if _, ok := l.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := l.Get(k); !ok {
			t.Fatalf("%s missing after eviction", k)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 1 || evicted[0] != "b" {
		t.Fatalf("evicted=%v want [b]", evicted)
	}
}

func TestLRUGetOrAddPublishRace(t *testing.T) {
	l := newLRU(8, nil)
	const N = 100
	var wg sync.WaitGroup
	var winners atomic.Int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			val := seq // unique per goroutine
			_, existed := l.GetOrAdd("k", val)
			if !existed {
				winners.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("HARD: GetOrAdd publish race produced %d winners, want exactly 1", winners.Load())
	}
}

// TestBodyHandleEvictionFreesFD proves that an evicted bodyHandle's
// underlying *os.File is released the moment the last in-flight reader
// calls Close.  This is the property that bounds the sidecar's open-fd
// count under load.
func TestBodyHandleEvictionFreesFD(t *testing.T) {
	dir := t.TempDir()
	old := MaxBodyHandleEntries
	MaxBodyHandleEntries = 2
	defer func() { MaxBodyHandleEntries = old }()

	store, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}

	mkBody := func(key, content string) {
		w, err := store.BeginSegment(key, SegmentParams{
			Status: 200, Start: 0, Length: int64(len(content)), Total: int64(len(content)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	mkBody("aaa", "alpha")
	mkBody("bbb", "bravo")
	mkBody("ccc", "charlie")

	// Acquire all three in quick succession; the third pushes the first
	// out of the bounded LRU.
	ha, ca, _ := store.AcquireBody("aaa")
	hb, cb, _ := store.AcquireBody("bbb")
	hc, cc, _ := store.AcquireBody("ccc")
	_, _, _ = ha, hb, hc

	// "aaa" is now evicted.  Releasing its sole reference must close the fd.
	if err := ca.Close(); err != nil {
		t.Fatalf("close evicted handle: %v", err)
	}
	// Reading from ha after Close must fail (fd closed).
	buf := make([]byte, 1)
	if _, err := ha.ReadAt(buf, 0); err == nil {
		t.Fatal("HARD: ReadAt on evicted+released handle succeeded — fd was not closed")
	}

	// The non-evicted entries remain readable.
	if _, err := hb.ReadAt(buf, 0); err != nil {
		t.Fatalf("read on live handle b: %v", err)
	}
	if _, err := hc.ReadAt(buf, 0); err != nil {
		t.Fatalf("read on live handle c: %v", err)
	}
	_ = cb.Close()
	_ = cc.Close()
}
