// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
)

// bodyHandle wraps a process-shared *os.File for one cached object.
//
// The file is opened once per key on the first warm hit and kept open
// across many requests so that pprof's #1 warm-hit allocation hotspot
// (os.OpenFile + os.newFile) becomes amortized across the lifetime of
// the cached entry.  Access uses pread (via io.ReaderAt /
// io.NewSectionReader / unix.Sendfile), so a single fd safely services
// concurrent requests with no seek-cursor races.
//
// Lifecycle:
//   - refs counts active borrowers (incremented by AcquireBody, decremented
//     by Close).  When refs is positive, the underlying *os.File MUST stay
//     open so in-flight ReadAt / sendfile syscalls remain valid.
//   - evicted is set by the bounded-LRU's onEvict hook when the entry is
//     pushed out by capacity pressure.  When refs reaches 0 AND evicted
//     is true, the next Close() releases the OS file descriptor.
//   - The next AcquireBody for the same key will then re-open the file
//     and publish a fresh bodyHandle.
//
// This means warm hits never pay an open(2) cost in steady state, and a
// long-running sidecar with thousands of keys still sits comfortably
// under RLIMIT_NOFILE.
type bodyHandle struct {
	f    *os.File
	size int64
	refs atomic.Int64

	evicted atomic.Bool

	// raw / fd are lazily populated on first call to RawConn / Fd.
	// We cache one *syscall.RawConn per bodyHandle so warm sendfile
	// callers don't pay an os.newRawConn allocation on every request,
	// and we cache the integer fd so callers that only need the fd
	// (e.g. our sendfile fast path on Linux/macOS) can skip the
	// rawConn.Control closure entirely.  Both are derived from the
	// same underlying *os.File and remain valid until the cache evicts
	// the entry and the last Close() releases the fd.
	rawOnce sync.Once
	raw     syscall.RawConn
	rawErr  error

	fdOnce sync.Once
	fd     int
	fdErr  error
}

// ReadAt forwards to the underlying file's pread implementation.
func (h *bodyHandle) ReadAt(p []byte, off int64) (int, error) { return h.f.ReadAt(p, off) }

// File returns the underlying *os.File.  Intended for callers that need
// fd-level operations (typically sendfile / splice via the unix package).
//
// SAFETY: callers MUST NOT close the returned file directly — the cache
// owns its lifecycle.  Use the io.Closer returned by AcquireBody to signal
// that the borrow is over.
func (h *bodyHandle) File() *os.File { return h.f }

// RawConn returns a cached syscall.RawConn for the underlying file.  The
// returned handle is shared across every warm-hit call for this object, so
// callers MUST NOT mutate it (Control / Read / Write are reentrant by
// design).  Computing it once eliminates the os.newRawConn allocation
// from the warm-hit hot path.
func (h *bodyHandle) RawConn() (syscall.RawConn, error) {
	h.rawOnce.Do(func() {
		h.raw, h.rawErr = h.f.SyscallConn()
	})
	return h.raw, h.rawErr
}

// Fd returns the cached integer file descriptor for the underlying file.
//
// SAFETY:
//
//   - The returned fd is owned by the cache; callers MUST NOT close it.
//   - Calling os.File.Fd() puts the file into blocking I/O mode (it
//     disassociates from the netpoller).  This is fine for our use
//     case — cached object files are regular disk files which are
//     ALWAYS in blocking mode anyway, and we only ever pread / sendfile
//     them, never select on them.
//
// The optimisation matters because sendfile fast-path callers can use
// the cached int directly and skip the rawConn.Control(func(uintptr))
// closure roundtrip, which costs ~2 heap allocations per request.
func (h *bodyHandle) Fd() (int, error) {
	h.fdOnce.Do(func() {
		// os.File.Fd never returns an error, but it can return ^uintptr(0)
		// if the file was already closed; treat that as an error so the
		// caller falls back to the safe path.
		raw := h.f.Fd()
		if raw == ^uintptr(0) {
			h.fdErr = os.ErrClosed
			return
		}
		h.fd = int(raw)
	})
	return h.fd, h.fdErr
}

// markEvicted is called by the bodyHandles LRU eviction hook.  It does
// NOT close the underlying file — that happens lazily inside Close()
// once the last in-flight reader releases its reference.
func (h *bodyHandle) markEvicted() { h.evicted.Store(true) }

// Close decrements refs.  If the entry has been evicted and refs reaches
// zero, the underlying *os.File is closed here.
func (h *bodyHandle) Close() error {
	if h.refs.Add(-1) == 0 && h.evicted.Load() {
		return h.f.Close()
	}
	return nil
}

// CloseUnderlying forces the OS file descriptor closed.  Only the Store
// calls this on full shutdown.
func (h *bodyHandle) CloseUnderlying() error { return h.f.Close() }

// Size returns the body file's size as observed when the handle was first
// opened.  Cached objects on this proxy are immutable post-commit (a re-fetch
// goes through AcquireRange + new SegmentWriter, which writes back via the
// same path but does not change file size for fully-cached objects).  The
// size is used as the upper bound for io.NewSectionReader.
func (h *bodyHandle) Size() int64 { return h.size }

// AcquireBody returns a shared *bodyHandle for key.  The returned handle's
// ReadAt is goroutine-safe.  Call Close on the returned io.Closer when done
// (this decrements the shared refcount and may release the OS file
// descriptor if the entry has since been evicted by the LRU).
func (s *Store) AcquireBody(key string) (*bodyHandle, io.Closer, error) {
	if v, ok := s.bodyHandles.Get(key); ok {
		bh := v.(*bodyHandle)
		// Skip stale entries that have been evicted but not yet closed.
		// Re-opening below restores both the LRU slot and the open fd.
		if !bh.evicted.Load() {
			bh.refs.Add(1)
			return bh, bh, nil
		}
	}
	// Not cached (or evicted) — open and publish.  Concurrent openers
	// race; the loser closes its own fd and re-uses the published handle.
	f, err := os.Open(s.bodyPath(key))
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	bh := &bodyHandle{f: f, size: st.Size()}
	bh.refs.Add(1)
	v, existed := s.bodyHandles.GetOrAdd(key, bh)
	if existed {
		// Lost the publish race — discard our fd, take the published one.
		_ = f.Close()
		old := v.(*bodyHandle)
		if old.evicted.Load() {
			// Extremely rare: the published handle was evicted between
			// our GetOrAdd and now.  Fall through to a fresh open.
			return s.AcquireBody(key)
		}
		old.refs.Add(1)
		return old, old, nil
	}
	return bh, bh, nil
}
