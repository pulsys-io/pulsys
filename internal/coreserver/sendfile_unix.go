// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux || darwin

package coreserver

import (
	"errors"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/pulsys-io/pulsys/internal/telemetry"
)

// errNotTCP is returned when the destination connection is not a *net.TCPConn
// (e.g. fasthttputil.InmemoryListener pipes used in some unit tests).  The
// caller should fall back to io.CopyBuffer.
var errNotTCP = errors.New("coreserver: dst is not *net.TCPConn")

// sendFileViaRaw transfers exactly n bytes from the file at integer fd
// inFd (starting at off) into the socket behind connRaw, using
// unix.Sendfile.  This bypasses Go's net.(*TCPConn).readFrom wrapper and
// the io.copyBuffer interface boxing it relies on.
//
// Inputs are designed to be CACHED on long-lived objects:
//
//   - connRaw lives for the duration of the keep-alive HTTP/1.1 session
//     (one allocation per accepted connection, amortized across many
//     requests).
//   - inFd is the integer file descriptor for the cached object, captured
//     once when the bodyHandle is opened (no syscall.RawConn / Control
//     closure needed in the hot path).
//
// Together this drives the warm-hit hot path to a single
// connRaw.Write closure allocation per request -- the irreducible cost of
// integrating with Go's netpoller.
//
// Cross-platform notes:
//
//   - The unix.Sendfile signature (outfd, infd, *offset, count) -> (n, err)
//     is identical on Linux and Darwin in golang.org/x/sys/unix.  Kernel
//     semantics differ (Linux: splice from page cache to socket; Darwin:
//     sendfile(2) with similar zero-copy properties) but the Go-side call
//     is the same.
//
//   - Returning false from the rawConn.Write callback parks the goroutine
//     on the netpoller until the socket is writable again -- exactly the
//     correct response to EAGAIN.
//
//   - Sendfile batches are capped by maxSendfileSlice with a small,
//     payload-aware ladder so the syscall <-> netpoll round-trip count
//     scales sensibly with body size:
//
//     <  4 MiB:  single sendfile call (the body itself).
//     <= 32 MiB: up to 16 MiB slices  -- covers the HF mid-tier
//     (Hub default chunked download is 10 MiB, common
//     tunings push to 16 MiB).  Most responses go in
//     exactly one syscall, occasionally two.
//     >  32 MiB: up to 32 MiB slices  -- elephant single-response
//     transfers (monolithic safetensors shards) need
//     fewer syscall <-> netpoll round-trips.
//
// Implementation note on the state struct:
//
//	We use a heap-allocated *sfState for the closure to capture by
//	pointer rather than capturing each variable individually (which the
//	compiler would otherwise box into a synthetic struct anyway).  This
//	keeps the closure allocation count to exactly one per call (a single
//	*sfState plus the closure header that points to it -- Go merges them
//	into one heap object).
//
// progress, when non-nil, receives the running transferred-byte count
// via atomic stores so a write-deadline pump (see write_deadline.go) can
// observe forward progress on large warm transfers.  It is nil on the
// small/mid hot path so the steady state pays no atomic stores.
func sendFileViaRaw(connRaw syscall.RawConn, inFd int, off, n int64, progress *int64) (int64, error) {
	st := sfStatePool.Get().(*sfState)
	st.inFd = inFd
	st.off = off
	st.n = n
	st.transferred = 0
	st.sendErr = nil
	st.progress = progress

	// st.cb is pre-bound to (*sfState).write at pool-init time so this
	// is a single pointer load -- no per-request method-value bind, no
	// per-request closure heap allocation.
	werr := connRaw.Write(st.cb)

	transferred := st.transferred
	sendErr := st.sendErr
	st.progress = nil // drop reference before returning to the pool
	sfStatePool.Put(st)

	if sendErr != nil {
		return transferred, sendErr
	}
	return transferred, werr
}

// sfState carries the closure state for sendFileViaRaw.  Pooling it lets
// us reuse the heap object across calls; cb is the pre-bound method
// value for st.write so the steady-state hot loop allocates exactly
// zero closures per request.
type sfState struct {
	inFd        int
	off         int64
	n           int64
	transferred int64
	sendErr     error
	progress    *int64 // optional: atomic running byte count for the deadline pump
	cb          func(uintptr) bool
}

var sfStatePool = sync.Pool{New: func() any {
	st := &sfState{}
	st.cb = st.write
	return st
}}

const (
	// midSendfileChunk covers the HF "default chunked download" mid-tier
	// (Hub uses 10 MiB blocks; common tunings push to 16 MiB).  16 MiB
	// lets a single response complete in 1-2 sendfile syscalls without
	// monopolising the kernel's send-side scheduling for too long.
	midSendfileChunk        = int64(16 << 20)
	elephantSendfileChunk   = int64(32 << 20)
	elephantSendfileThresh  = int64(32 << 20) // bodies strictly larger use 32 MiB slices
	smallSendfileSingleshot = int64(4 << 20)  // bodies <= this fit in one sendfile call
)

// maxSendfileSlice picks the next sendfile(2) batch size for a response
// of total bytes, with `remaining` bytes still to transfer.  See the
// sendFileViaRaw doc comment for the rationale of the ladder.
func maxSendfileSlice(total int64, remaining int64) int64 {
	var max int64
	switch {
	case total <= smallSendfileSingleshot:
		// One shot: the whole body.
		max = total
	case total > elephantSendfileThresh:
		max = elephantSendfileChunk
	default:
		max = midSendfileChunk
	}
	if remaining < max {
		return remaining
	}
	return max
}

// write is the rawConn.Write callback bound by method-value below.  Using
// a method on a pooled receiver avoids capturing local variables in a
// fresh closure on every call.
func (st *sfState) write(outFdU uintptr) bool {
	outFd := int(outFdU)
	for st.transferred < st.n {
		offset := st.off + st.transferred
		remaining := st.n - st.transferred
		batch := maxSendfileSlice(st.n, remaining)
		telemetry.IncSendfileBodyOnlyCall()
		wrote, errno := unix.Sendfile(outFd, st.inFd, &offset, int(batch))
		if wrote > 0 {
			st.transferred += int64(wrote)
			if st.progress != nil {
				atomic.StoreInt64(st.progress, st.transferred)
			}
		}
		switch errno {
		case nil:
			if wrote == 0 {
				return true // premature EOF; caller observes short transfer
			}
			continue
		case unix.EAGAIN:
			telemetry.IncSendfileEAGAIN()
			return false // wait for socket writability
		case unix.EINTR:
			continue
		default:
			st.sendErr = errno
			return true
		}
	}
	return true
}
