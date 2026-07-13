// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"context"
	"sync"
	"time"
)

// inflightSet tracks byte ranges that are currently being fetched from
// upstream for one cache key.  Its purpose is to deduplicate concurrent
// upstream fetches without serializing disjoint range requests.
//
// Concretely, the previous design grabbed one Mutex per key and held it
// for the entire duration of the upstream stream.  When `hf download`
// dispatches 4-16 parallel range requests for one large LFS file, all of
// them hash to the same cache key (Range is not part of the key), so the
// mutex pinned effective parallelism to 1.  This was the single biggest
// cold-path bottleneck.
//
// inflightSet keeps a slice of currently-in-flight [Start, End) spans
// guarded by a mutex + sync.Cond.  AcquireRange:
//
//   - blocks until no in-flight span overlaps the requested [start, end)
//   - then registers the requested span as in-flight
//   - returns a release function that removes the span and wakes waiters
//
// Two callers asking for *disjoint* ranges of the same key proceed in
// parallel; two callers asking for the *same* range serialize so the
// second one observes a populated cache when re-checking.
type inflightSet struct {
	mu       sync.Mutex
	cond     *sync.Cond
	spans    []Span
	condInit sync.Once
}

func (s *inflightSet) ensureCond() {
	s.condInit.Do(func() { s.cond = sync.NewCond(&s.mu) })
}

// acquire blocks until [start, end) is disjoint from every in-flight span,
// then records it.  Returns a release closure that the caller MUST invoke
// exactly once when the upstream fetch (and any disk write) is complete.
func (s *inflightSet) acquire(start, end int64) func() {
	s.ensureCond()
	s.mu.Lock()
	for spansOverlap(s.spans, start, end) {
		s.cond.Wait()
	}
	s.spans = append(s.spans, Span{Start: start, End: end})
	s.mu.Unlock()
	return func() { s.release(start, end) }
}

// acquireCtx is a bounded, cancellable variant of acquire.  It returns
// (release, true) once [start, end) is disjoint from every in-flight
// span, or (nil, false) if ctx is cancelled or maxWait elapses first.
//
// The uncontended fast path takes the span immediately without spawning
// any goroutine.  Only when there IS an overlapping in-flight span do we
// arm a one-shot waker that broadcasts the cond when the deadline or ctx
// fires, so the cond.Wait loop can observe the bail conditions even when
// no release happens (the whole point: a slow whole-file holder must not
// pin a waiter past its caller's deadline).
func (s *inflightSet) acquireCtx(ctx context.Context, start, end int64, maxWait time.Duration) (func(), bool) {
	s.ensureCond()

	s.mu.Lock()
	if !spansOverlap(s.spans, start, end) {
		s.spans = append(s.spans, Span{Start: start, End: end})
		s.mu.Unlock()
		return func() { s.release(start, end) }, true
	}
	s.mu.Unlock()

	var deadline time.Time
	if maxWait > 0 {
		deadline = time.Now().Add(maxWait)
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		var tc <-chan time.Time
		if maxWait > 0 {
			t := time.NewTimer(maxWait)
			defer t.Stop()
			tc = t.C
		}
		var ctxDone <-chan struct{}
		if ctx != nil {
			ctxDone = ctx.Done()
		}
		select {
		case <-stop:
		case <-tc:
		case <-ctxDone:
		}
		// Wake the waiter so it re-checks ctx/deadline.  A single
		// broadcast suffices: both bail conditions are monotonic, so
		// once tripped the waiter's loop returns without re-waiting.
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	}()

	s.mu.Lock()
	for spansOverlap(s.spans, start, end) {
		if ctx != nil && ctx.Err() != nil {
			s.mu.Unlock()
			return nil, false
		}
		if maxWait > 0 && !time.Now().Before(deadline) {
			s.mu.Unlock()
			return nil, false
		}
		s.cond.Wait()
	}
	s.spans = append(s.spans, Span{Start: start, End: end})
	s.mu.Unlock()
	return func() { s.release(start, end) }, true
}

func (s *inflightSet) release(start, end int64) {
	s.mu.Lock()
	for i, sp := range s.spans {
		if sp.Start == start && sp.End == end {
			s.spans = append(s.spans[:i], s.spans[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if s.cond != nil {
		s.cond.Broadcast()
	}
}

// spansOverlap reports whether any span in `set` overlaps [start, end).
// Half-open ranges: [a, b) overlaps [c, d) iff a < d && c < b.
func spansOverlap(set []Span, start, end int64) bool {
	for _, sp := range set {
		if sp.Start < end && start < sp.End {
			return true
		}
	}
	return false
}

// AcquireRange blocks until no existing in-flight upstream fetch for `key`
// overlaps [start, end), then registers [start, end) and returns a release
// closure.  The returned closure MUST be called exactly once when the
// upstream fetch (and any disk write) finishes — typically inside the
// streaming response body's Close.
//
// AcquireRange replaces the coarse-grained Lock(key) for the artifact
// miss path.  Two goroutines requesting non-overlapping ranges of the same
// object now proceed in parallel; two goroutines requesting the same range
// still serialize so the second observes a populated cache when it
// re-checks.
//
// Callers that don't have a meaningful range (no Range header, full body)
// should pass [0, math.MaxInt64) or any wide range that covers the whole
// object — this naturally serializes against any other concurrent fetch
// for that key, matching the previous Lock(key) semantics.
func (s *Store) AcquireRange(key string, start, end int64) func() {
	v, _ := s.inflight.LoadOrStore(key, &inflightSet{})
	set := v.(*inflightSet)
	return set.acquire(start, end)
}

// AcquireRangeCtx is a bounded, cancellable AcquireRange.  It returns
// (release, true) once [start, end) is free of overlapping in-flight
// fetches, or (nil, false) if ctx is cancelled or maxWait elapses while
// waiting.  maxWait <= 0 means "wait until ctx is done" (no time bound).
//
// The public ingress uses this with a short budget so an end-user
// download contending with a long whole-file fetch falls through to an
// independent pass-through instead of blocking past the client's header
// read timeout.  The importer's loopback handler keeps the unbounded
// AcquireRange so it waits for the cache to populate.
func (s *Store) AcquireRangeCtx(ctx context.Context, key string, start, end int64, maxWait time.Duration) (func(), bool) {
	v, _ := s.inflight.LoadOrStore(key, &inflightSet{})
	set := v.(*inflightSet)
	return set.acquireCtx(ctx, start, end, maxWait)
}
