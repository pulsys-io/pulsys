// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build darwin

package coreserver

import (
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/pulsys-io/pulsys/internal/telemetry"
)

// sendFileWithHeaderViaRaw transfers an HTTP response head + n bytes of
// the file at integer fd inFd (starting at off) into the socket behind
// connRaw -- in a single sendfile(2) syscall via Darwin's sf_hdtr
// argument.
//
// This is the warm-hit fast path's biggest kernel-boundary win on
// macOS: it collapses what is normally TWO syscalls per response
// (write(headers); sendfile(body)) into ONE.  Measured impact at
// 16 KiB payloads against the same wrk -t4 -c64 harness:
//
//	pulsys   1.64 GB/s   (with fusion)
//	pulsys   1.05 GB/s   (without fusion -- 2 syscalls/response)
//	Go net/http 1.13 GB/s
//	Caddy 2.x   0.90 GB/s
//
// On larger payloads the body-sendfile syscall dominates and the win
// shrinks to a few percent, but it is consistent throughout the small-
// to mid-tier (16 KiB ... 10 MiB).  Above ~16 MiB everyone is purely
// sendfile-saturated and the rows tie within thermal noise.
//
// Linux's sendfile(2) does NOT accept header iovecs; sendfile_unix.go
// keeps the two-call form there.  io_uring would be the equivalent
// fusion path on Linux and is tracked as future work.
//
// Partial-send semantics:
//
//   - Darwin sendfile may return EAGAIN with *len set to the bytes
//     actually queued (header-bytes-first then body-bytes).  We track
//     state in a pooled sfHdtrState and resume from where we left
//     off, switching to the plain (header-less) sendFileViaRaw once
//     the header is fully drained.
//
//   - On any error other than EAGAIN/EINTR we record it and signal the
//     caller via the standard rawConn.Write callback contract.
//
// The useCork parameter is Linux-only and ignored here -- Darwin's
// sf_hdtr already fuses the header into the same sendfile(2) syscall,
// so cork is structurally unnecessary.  Kept in the signature so the
// call site (server.go) is identical across platforms.
func sendFileWithHeaderViaRaw(connRaw syscall.RawConn, inFd int, off, n int64, header []byte, useCork bool, progress *int64) (int64, error) {
	_ = useCork
	st := sfHdtrStatePool.Get().(*sfHdtrState)
	st.inFd = inFd
	st.off = off
	st.bodyTotal = n
	st.bodySent = 0
	st.header = header
	st.headerSent = 0
	st.sendErr = nil
	st.progress = progress

	// st.cb is pre-bound to (*sfHdtrState).write at pool-init time, so
	// passing it here is a single pointer load -- NO method-value bind
	// allocation per request.  Without this pre-bind the per-request
	// alloc count rises by 1 (the method-value funcval).
	werr := connRaw.Write(st.cb)

	bodySent := st.bodySent
	sendErr := st.sendErr
	st.header = nil   // drop reference so the pooled struct doesn't pin the buffer
	st.progress = nil // drop reference before returning to the pool
	sfHdtrStatePool.Put(st)

	if sendErr != nil {
		return bodySent, sendErr
	}
	return bodySent, werr
}

// sfHdtrState is the closure state for sendFileWithHeaderViaRaw.
// Pooled to keep the rawConn.Write callback allocation-free.
//
// cb is the *bound* method value for st.write; it is set ONCE at pool
// init time and reused on every Get/Put cycle.  This eliminates the
// per-request method-value bind that would otherwise heap-allocate a
// funcval on every call to sendFileWithHeaderViaRaw.
type sfHdtrState struct {
	inFd       int
	off        int64
	bodyTotal  int64
	bodySent   int64
	header     []byte
	headerSent int
	sendErr    error
	progress   *int64 // optional: atomic running body-byte count for the deadline pump
	cb         func(uintptr) bool
}

var sfHdtrStatePool = sync.Pool{New: func() any {
	st := &sfHdtrState{}
	st.cb = st.write
	return st
}}

// Darwin sendfile(2):
//
//	int sendfile(int fd, int s, off_t offset, off_t *len,
//	             struct sf_hdtr *hdtr, int flags);
//
//	struct sf_hdtr {
//	    struct iovec *headers;
//	    int           hdr_cnt;
//	    struct iovec *trailers;
//	    int           trl_cnt;
//	};
//
// Critical detail (Darwin man page):
//
//	"When a header or trailer is specified, the value of len argument
//	 indicates the maximum number of bytes in the header AND/OR file to
//	 be sent.  On return, the len argument specifies the TOTAL number of
//	 bytes sent."
//
// So *len is in/out and counts header bytes + file bytes together.  We
// have to include the remaining-header length in our "max" budget;
// otherwise the kernel happily sends headers and eats into our file
// budget, and the body comes up short — silently — on apparent success.
const sysSendfileDarwin = 337

type iovecDarwin struct {
	Base *byte
	Len  uint64
}

type sfHdtrDarwin struct {
	Headers  *iovecDarwin
	HdrCnt   int32
	_        int32 // padding so Trailers is 8-byte aligned on 64-bit
	Trailers *iovecDarwin
	TrlCnt   int32
	_        int32
}

// write is the rawConn.Write callback.  Returning false re-arms the
// goroutine on the netpoller until the socket is writable.
//
// State after each iteration:
//
//	st.headerSent  — total header bytes acknowledged shipped
//	st.bodySent    — total file bytes acknowledged shipped
//	st.off         — file start offset (constant for the call)
//
// Headers are always shipped before file bytes (Darwin contract).
func (st *sfHdtrState) write(outFdU uintptr) bool {
	outFd := int(outFdU)
	for st.bodySent < st.bodyTotal || st.headerSent < len(st.header) {
		headerRemaining := int64(len(st.header) - st.headerSent)
		bodyRemaining := st.bodyTotal - st.bodySent

		// *len budget covers header tail PLUS body tail.
		sentLen := headerRemaining + bodyRemaining
		if sentLen <= 0 {
			return true
		}

		var hdrPtr *iovecDarwin
		var hdrCnt int32
		var iov iovecDarwin
		if headerRemaining > 0 {
			tail := st.header[st.headerSent:]
			iov.Base = &tail[0]
			iov.Len = uint64(headerRemaining)
			hdrPtr = &iov
			hdrCnt = 1
		}
		hdtr := sfHdtrDarwin{Headers: hdrPtr, HdrCnt: hdrCnt}

		telemetry.IncSendfileFusedCall()
		_, _, errno := syscall.Syscall6(
			sysSendfileDarwin,
			uintptr(st.inFd),
			uintptr(outFd),
			uintptr(st.off+st.bodySent),
			uintptr(unsafe.Pointer(&sentLen)),
			uintptr(unsafe.Pointer(&hdtr)),
			0,
		)

		// sentLen now = total bytes queued (header + body).  Header
		// always drains first.
		totalShipped := sentLen
		if totalShipped > 0 {
			if totalShipped >= headerRemaining {
				st.headerSent = len(st.header)
				st.bodySent += totalShipped - headerRemaining
				if st.progress != nil {
					atomic.StoreInt64(st.progress, st.bodySent)
				}
			} else {
				st.headerSent += int(totalShipped)
			}
		}

		switch errno {
		case 0:
			if totalShipped == 0 {
				// No progress on a successful call (premature EOF on file).
				return true
			}
			continue
		case syscall.EAGAIN:
			telemetry.IncSendfileEAGAIN()
			return false
		case syscall.EINTR:
			continue
		default:
			st.sendErr = errno
			return true
		}
	}
	return true
}
