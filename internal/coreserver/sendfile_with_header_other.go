// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux

package coreserver

import (
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/pulsys-io/pulsys/internal/telemetry"
)

// sendFileWithHeaderViaRaw — Linux fallback.
//
// Linux's sendfile(2) does NOT accept header iovecs, so on Linux we keep
// the classic two-call form: write(headers); sendfile(body).  Both calls
// go through the cached syscall.RawConn so we still avoid the per-request
// rawConn allocation that Go's net.(*TCPConn).readFrom would otherwise
// incur.
//
// When useCork is true we additionally set TCP_CORK on the socket
// before the write and clear it after the sendfile completes.  Cork
// coalesces the small header + the first segment of the body into a
// single outbound TCP segment, eliminating the "header in tiny segment
// followed by body in MSS-sized segments" wire pattern that wastes a
// segment of header overhead on every warm response.  Cost: two extra
// setsockopt calls per response; benefit: 1 fewer wire segment per
// response.  On localhost wrk benches the cost dominates; on real-RTT
// links the benefit dominates.  Measure both with the A6 bench harness.
//
// The right way to fuse the WRITE+SENDFILE pair on Linux into a SINGLE
// syscall is io_uring with linked SQEs.  That is Track A3 in
// docs/internals.md and obsoletes this cork-bracket entirely
// when enabled.
func sendFileWithHeaderViaRaw(connRaw syscall.RawConn, inFd int, off, n int64, header []byte, useCork bool, progress *int64) (int64, error) {
	if useCork {
		// corkOn / corkOff are package-level functions; passing them
		// to connRaw.Control does not allocate a closure (the
		// rawConn.Control implementation captures the funcval into a
		// pooled internal struct).  Best-effort: if cork setting
		// fails (e.g. unsupported socket type in a test) we silently
		// fall through.
		if err := connRaw.Control(corkOn); err == nil {
			telemetry.IncTCPCorkCall()
			// Uncork happens at the bottom of this function on every
			// exit path; using a deferred funclit here would heap-
			// allocate a closure, which we are deliberately avoiding.
			defer connRawUncork(connRaw)
		}
	}

	st := lxHdtrStatePool.Get().(*lxHdtrState)
	st.header = header
	st.headerSent = 0
	st.writeErr = nil

	werr := connRaw.Write(st.writeHeader)
	writeErr := st.writeErr
	st.header = nil
	lxHdtrStatePool.Put(st)
	if writeErr != nil {
		return 0, writeErr
	}
	if werr != nil {
		return 0, werr
	}
	return sendFileViaRaw(connRaw, inFd, off, n, progress)
}

// corkOn / corkOff are package-level callbacks passed to
// syscall.RawConn.Control.  Defined as named functions (not lambdas) so
// the resulting funcval is statically allocated rather than created on
// the heap per request.
func corkOn(fd uintptr) {
	_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_CORK, 1)
}

func corkOff(fd uintptr) {
	_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_CORK, 0)
}

// connRawUncork is the deferred uncork helper.  Pulled out into a named
// function so the `defer` statement does not capture any local variable
// (which would heap-allocate a closure).  The single connRaw argument is
// passed via the deferred call's own argument frame, not via a closure.
func connRawUncork(connRaw syscall.RawConn) {
	_ = connRaw.Control(corkOff)
}

type lxHdtrState struct {
	header     []byte
	headerSent int
	writeErr   error
}

var lxHdtrStatePool = sync.Pool{New: func() any { return &lxHdtrState{} }}

func (st *lxHdtrState) writeHeader(outFdU uintptr) bool {
	outFd := int(outFdU)
	for st.headerSent < len(st.header) {
		n, err := syscall.Write(outFd, st.header[st.headerSent:])
		if n > 0 {
			st.headerSent += n
		}
		switch err {
		case nil:
			if n == 0 {
				return true
			}
		case syscall.EAGAIN:
			return false
		case syscall.EINTR:
			continue
		default:
			st.writeErr = err
			return true
		}
	}
	return true
}
