// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
)

func newInflightStore(t *testing.T) *cache.Store {
	t.Helper()
	store, err := cache.NewStore(filepath.Join(t.TempDir(), "cache"), "none")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestAcquireRangeCtx_FastPath: an uncontended acquire succeeds
// immediately.
func TestAcquireRangeCtx_FastPath(t *testing.T) {
	store := newInflightStore(t)
	rel, ok := store.AcquireRangeCtx(context.Background(), "k", 0, 100, time.Second)
	if !ok {
		t.Fatal("uncontended acquire returned ok=false")
	}
	rel()
}

// TestAcquireRangeCtx_TimesOutWhileContended: with an overlapping range
// already held, a bounded acquire returns ok=false at ~maxWait instead of
// blocking forever.  This is the property that keeps an end-user download
// from hanging behind a long whole-file fetch.
func TestAcquireRangeCtx_TimesOutWhileContended(t *testing.T) {
	store := newInflightStore(t)
	// Holder takes the whole file [0, MaxInt64) like a no-Range GET.
	holder := store.AcquireRange("k", 0, 1<<62)
	defer holder()

	const budget = 150 * time.Millisecond
	start := time.Now()
	rel, ok := store.AcquireRangeCtx(context.Background(), "k", 0, 100, budget)
	elapsed := time.Since(start)
	if ok {
		rel()
		t.Fatal("acquire succeeded despite an overlapping in-flight holder")
	}
	if elapsed < budget/2 {
		t.Fatalf("returned after %v, well before budget %v", elapsed, budget)
	}
	if elapsed > budget+2*time.Second {
		t.Fatalf("returned after %v, far past budget %v (waker not firing?)", elapsed, budget)
	}
}

// TestAcquireRangeCtx_CancelUnblocks: a cancelled context releases a
// waiting acquire promptly even with no time bound.
func TestAcquireRangeCtx_CancelUnblocks(t *testing.T) {
	store := newInflightStore(t)
	holder := store.AcquireRange("k", 0, 1<<62)
	defer holder()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, ok := store.AcquireRangeCtx(ctx, "k", 0, 100, 0 /* no time bound */)
		done <- ok
	}()

	// Give the goroutine time to block in cond.Wait, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("acquire returned ok=true after context cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not unblock after context cancel")
	}
}

// TestAcquireRangeCtx_AcquiresAfterRelease: a waiter blocked on an
// overlapping holder acquires as soon as the holder releases (before the
// budget elapses), and gets ok=true.
func TestAcquireRangeCtx_AcquiresAfterRelease(t *testing.T) {
	store := newInflightStore(t)
	holder := store.AcquireRange("k", 0, 1<<62)

	done := make(chan bool, 1)
	go func() {
		rel, ok := store.AcquireRangeCtx(context.Background(), "k", 0, 100, 5*time.Second)
		if ok {
			rel()
		}
		done <- ok
	}()

	time.Sleep(50 * time.Millisecond)
	holder() // release; the waiter should now proceed

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("acquire failed even though the holder released within budget")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not proceed after holder released")
	}
}
