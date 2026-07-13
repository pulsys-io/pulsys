// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import "sync"

// lru is a tiny, self-contained, thread-safe LRU keyed by string.
//
// It is intentionally small (no generics, no external dependency) so it
// can be embedded into the cache-side metadata maps without dragging in
// a third-party package.  Capacity is enforced approximately: the eviction
// runs synchronously inside Add when the size exceeds capacity, so the
// table never grows past `cap+1` entries between operations.
//
// The optional onEvict callback fires inside Add while the lru's mutex
// is held, so callbacks must be cheap and non-blocking (no I/O, no other
// lru operations).  Use it for resource-cleanup hooks like marking an
// open *os.File as eligible for close once its last in-flight reader
// releases it.
//
// The zero value is NOT usable; construct via newLRU.
type lru struct {
	mu      sync.Mutex
	cap     int
	head    *lruEntry // most-recently used
	tail    *lruEntry // least-recently used
	m       map[string]*lruEntry
	onEvict func(key string, val any)
}

type lruEntry struct {
	key        string
	val        any
	prev, next *lruEntry
}

func newLRU(capacity int, onEvict func(key string, val any)) *lru {
	if capacity < 1 {
		capacity = 1
	}
	return &lru{
		cap:     capacity,
		m:       make(map[string]*lruEntry, capacity),
		onEvict: onEvict,
	}
}

// Get returns (val, true) if key is present and promotes it to MRU.
//
// The value is captured under the mutex - reading e.val AFTER
// Unlock would race with Add's "update existing entry" branch, which
// writes e.val while holding the same lock (caught by -race in p10).
func (l *lru) Get(key string) (any, bool) {
	l.mu.Lock()
	e, ok := l.m[key]
	if !ok {
		l.mu.Unlock()
		return nil, false
	}
	l.moveToFrontLocked(e)
	v := e.val
	l.mu.Unlock()
	return v, true
}

// Add inserts (key, val) and may evict the LRU entry to maintain capacity.
// Updates val in place if key already present.
func (l *lru) Add(key string, val any) {
	l.mu.Lock()
	if e, ok := l.m[key]; ok {
		e.val = val
		l.moveToFrontLocked(e)
		l.mu.Unlock()
		return
	}
	e := &lruEntry{key: key, val: val}
	l.m[key] = e
	l.pushFrontLocked(e)
	var evictedKey string
	var evictedVal any
	var didEvict bool
	if len(l.m) > l.cap {
		victim := l.tail
		if victim != nil {
			l.removeLocked(victim)
			delete(l.m, victim.key)
			evictedKey = victim.key
			evictedVal = victim.val
			didEvict = true
		}
	}
	l.mu.Unlock()
	if didEvict && l.onEvict != nil {
		l.onEvict(evictedKey, evictedVal)
	}
}

// GetOrAdd is the atomic combination of Get and Add: if key is present it
// behaves like Get (promotes and returns the existing value with
// existed=true); otherwise it inserts newVal (possibly evicting the LRU
// entry) and returns newVal with existed=false.
//
// Use this on publish paths that race concurrent callers building
// expensive values — only one builder's value gets installed; the others
// observe existed=true and can discard their work.
func (l *lru) GetOrAdd(key string, newVal any) (val any, existed bool) {
	l.mu.Lock()
	if e, ok := l.m[key]; ok {
		l.moveToFrontLocked(e)
		v := e.val
		l.mu.Unlock()
		return v, true
	}
	e := &lruEntry{key: key, val: newVal}
	l.m[key] = e
	l.pushFrontLocked(e)
	var evictedKey string
	var evictedVal any
	var didEvict bool
	if len(l.m) > l.cap {
		victim := l.tail
		if victim != nil {
			l.removeLocked(victim)
			delete(l.m, victim.key)
			evictedKey = victim.key
			evictedVal = victim.val
			didEvict = true
		}
	}
	l.mu.Unlock()
	if didEvict && l.onEvict != nil {
		l.onEvict(evictedKey, evictedVal)
	}
	return newVal, false
}

// Len returns the current number of entries (cheap, only takes the mutex).
func (l *lru) Len() int {
	l.mu.Lock()
	n := len(l.m)
	l.mu.Unlock()
	return n
}

// Delete removes key from the LRU if present. When onEvict is set it
// runs after the entry is removed (same contract as capacity eviction).
func (l *lru) Delete(key string) bool {
	l.mu.Lock()
	e, ok := l.m[key]
	if !ok {
		l.mu.Unlock()
		return false
	}
	l.removeLocked(e)
	delete(l.m, key)
	val := e.val
	l.mu.Unlock()
	if l.onEvict != nil {
		l.onEvict(key, val)
	}
	return true
}

func (l *lru) pushFrontLocked(e *lruEntry) {
	e.prev = nil
	e.next = l.head
	if l.head != nil {
		l.head.prev = e
	}
	l.head = e
	if l.tail == nil {
		l.tail = e
	}
}

func (l *lru) removeLocked(e *lruEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		l.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		l.tail = e.prev
	}
	e.prev, e.next = nil, nil
}

func (l *lru) moveToFrontLocked(e *lruEntry) {
	if l.head == e {
		return
	}
	l.removeLocked(e)
	l.pushFrontLocked(e)
}
