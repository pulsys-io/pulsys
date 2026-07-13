// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"sync"
	"unsafe"
)

// MaxKeyHexCacheEntries caps the size of the in-memory KeyHex memo
// table.  When the limit is exceeded, oldest entries are dropped (a
// random eviction is sufficient — recomputing KeyHex is cheap, only
// expensive enough to be worth memoising at all).  The default keeps the
// table well under 1 MiB even for very large model repos with many
// shards.
var MaxKeyHexCacheEntries = 50000

// KeyHexCache memoises KeyHex(method, host, path, rawQuery, auth) results
// so repeat requests for the same URL skip both the sha256 hashing CPU
// work and the 64-byte hex string allocation.
//
// The lookup key is a synthetic composite string that uniquely encodes the
// (method, host, path, rawQuery, auth) tuple.  On the read path the
// composite is built into a fixed-size stack buffer and aliased as a
// string with no allocation via unsafe.String.  Map lookups with string
// keys in Go do not retain or copy the key, so a stack-aliased string is
// safe for read access.  On a miss we materialize a heap copy before
// publishing the entry.
//
// Memory characteristics: bounded by MaxKeyHexCacheEntries.  When the
// table reaches that size, the next miss triggers a single random
// eviction — not strict LRU, but cheap (one map iter step) and
// sufficient because KeyHex is fast to recompute.  At the default
// 50,000 entries × ~150 B per row, peak RAM is ~7 MiB.
//
// Concurrency: sync.RWMutex; reads dominate the hot path.
//
// The zero value is ready to use.  Safe for concurrent use.
type KeyHexCache struct {
	mu sync.RWMutex
	m  map[string]string
}

// keyHexStackBufSize is large enough to hold the longest realistic
// composite key (method ~7 + sep + host ~64 + sep + path ~256 + sep +
// query ~256 + sep + auth bucket 16 ≈ 600 bytes).  Inputs longer than
// this fall back to the allocating path.
const keyHexStackBufSize = 768

// Get returns the cached KeyHex for (method, host, path, rawQuery, auth),
// computing and storing it on first miss.  Output is byte-for-byte
// identical to KeyHex on every input.
func (c *KeyHexCache) Get(method, host, path, rawQuery, auth string) string {
	total := len(method) + 1 + len(host) + 1 + len(path) + 1 + len(rawQuery) + 1 + len(auth)
	if total > keyHexStackBufSize {
		return KeyHex(method, host, path, rawQuery, auth)
	}

	var buf [keyHexStackBufSize]byte
	n := 0
	n += copy(buf[n:], method)
	buf[n] = '|'
	n++
	n += copy(buf[n:], host)
	buf[n] = '|'
	n++
	n += copy(buf[n:], path)
	buf[n] = '|'
	n++
	n += copy(buf[n:], rawQuery)
	buf[n] = '|'
	n++
	n += copy(buf[n:], auth)

	lookup := unsafe.String(&buf[0], n)

	c.mu.RLock()
	v, ok := c.m[lookup]
	c.mu.RUnlock()
	if ok {
		return v
	}

	hex := KeyHex(method, host, path, rawQuery, auth)
	keyCopy := string(buf[:n])

	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[string]string, 1024)
	}
	if MaxKeyHexCacheEntries > 0 && len(c.m) >= MaxKeyHexCacheEntries {
		// Evict one random entry to bound memory.  Map iteration order
		// is randomized, so this gives uniform-random eviction in O(1).
		// True LRU would require a doubly-linked list; for KeyHex (cheap
		// to recompute) random eviction is the right cost trade.
		for k := range c.m {
			delete(c.m, k)
			break
		}
	}
	c.m[keyCopy] = hex
	c.mu.Unlock()
	return hex
}
