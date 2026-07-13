// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux

// io_uring reactor — Option B real implementation.
//
// One reactor goroutine per reuseport listener owns an io_uring ring
// and runs the full accept / recv / parse / write cycle without ever
// touching Go's netpoller.  No goroutine-per-connection; no epoll_ctl;
// no bufio.Reader on the hot path.
//
// Architecture
//
//	listener fd (dup'd from net.TCPListener) ──► IORING_OP_ACCEPT
//	    │
//	    └─► accepted fd ──► IORING_OP_RECV ──► HTTP/1 incremental parse
//	                              │
//	                              └─► warm-hit lookup
//	                                    │
//	                                    └─► IORING_OP_WRITE(header)
//	                                          │
//	                                          └─► raw unix.Sendfile(body)
//	                                                │
//	                                                └─► loop or close
//
// Each reactor is single-issuer (Linux io_uring SINGLE_ISSUER hint), so
// no locking is required on the ring.  All connection state for a
// given reactor is touched only by that reactor's goroutine.
//
// Non-warm requests (cache miss, non-GET, anything tryServeWarm
// declines) are detached: the accepted fd is dup'd to an *os.File,
// wrapped via net.FileConn, and handed to a slow-path goroutine that
// runs the existing serveConn pipeline.  The reactor never blocks on
// fallback handling.
//
// Sendfile note: io_uring has no SENDFILE opcode upstream, and
// IORING_OP_SPLICE requires a pipe between file and socket.  The
// hybrid v1 path proved that a raw unix.Sendfile() syscall after a
// ring WRITE is fast enough for the loopback warm-hit case.  For
// large payloads or remote NICs, the next step is IORING_OP_SPLICE
// with a per-reactor pipe; we'll wire that in once the simpler form
// proves out at bench.
package coreserver

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/classify"
	"github.com/pulsys-io/pulsys/internal/telemetry"
	"golang.org/x/sys/unix"
)

// detachedConnDefaultDeadline bounds the lifetime of a detached
// fallback conn when the operator did not configure explicit
// Read/Write timeouts on the Server.
//
// Why a fallback default rather than "no deadline":
//
//	The cork+sendfile path (server.go serveConn) sets a per-
//	request read deadline before EVERY readRequest() call.  A
//	misbehaving client cannot pin a serveConn goroutine.  The
//	io_uring reactor detaches request handling into runDetached
//	goroutines that own the conn for the duration of the call;
//	without a deadline a slowloris client (or a test that
//	abandons a kept-alive conn) pins that goroutine forever.
//	We catch this in tests via t.Cleanup hangs; in production
//	it manifests as an unbounded reactor goroutine pool.
//
//	The default of 2 minutes matches the Server's
//	default ReadTimeout / WriteTimeout used in cmd/pulsys
//	when the operator does not override.  It is intentionally
//	generous: a slow but legitimate HF /resolve POST with a
//	multi-MB diff-base upload must complete in under 2 minutes
//	or it would already have failed under the cork+sendfile
//	path too.
const detachedConnDefaultDeadline = 2 * time.Minute

// CQE user_data encoding (64 bits):
//
//	bits  0.. 7  op tag      (opAccept / opRecv / opWrite / opClose / opWakeup)
//	bits  8..39  fd (32-bit, sign-extended)
//	bits 40..63  seq/gen     (reserved; helps debugging)
const (
	opTagAccept uint8 = 1
	opTagRecv   uint8 = 2
	opTagWrite  uint8 = 3
	opTagClose  uint8 = 4
	// opTagWakeup tags the IORING_OP_READ SQE the reactor arms
	// on its wakeup eventfd at startup.  Server.Close() writes
	// to the eventfd to complete this READ; the CQE wakes the
	// reactor out of its blocking ring.enter() call so the
	// stop-channel check at the top of the loop can fire.  See
	// ioReactor.run() / ioReactor.close().
	opTagWakeup uint8 = 5
)

func packUserData(op uint8, fd int32) uint64 {
	return uint64(op) | (uint64(uint32(fd)) << 8)
}

func unpackUserData(ud uint64) (op uint8, fd int32) {
	return uint8(ud & 0xff), int32(uint32(ud >> 8))
}

// Per-reactor recv buffer size.  Sized for HTTP/1.1 request line +
// headers; 16 KiB is the same limit the bufio-based parser already
// imposes via headerScratchSize.
const reactorRecvBufSize = 16 * 1024

// connState tracks where a connection is in the request-response
// state machine.
type connState uint8

const (
	csReading connState = iota
	csWriting
	csClosing
)

// ioConn is the per-connection state owned by a single reactor.  All
// fields are touched only by the reactor goroutine.
type ioConn struct {
	fd        int32
	state     connState
	recvBuf   []byte // points into a pooled 16 KiB buffer
	recvBufP  *[]byte
	recvLen   int
	req       Request
	hb        *headerBuf // header staged for write
	keepAlive bool

	// Sendfile bookkeeping when state==csWriting: after the WRITE CQE
	// completes, the reactor calls unix.Sendfile() inline for the body.
	bodyFd  int
	bodyOff int64
	bodyLen int64

	// Hold a cache release until response is fully written.
	release io.Closer

	// releaseSlot, when non-nil, is invoked exactly once at
	// connection close to give the per-IP cap accounting
	// (Server.MaxConnsPerIP) back its slot.  Mirrors the std-lib
	// acceptLoop's release callback so a deployment that toggles
	// -iouring on or off sees the same cap behaviour from the
	// outside.  Nil when MaxConnsPerIP == 0 (cap disabled) or
	// when the peer address could not be parsed (non-TCP path).
	releaseSlot perIPSlot
}

var reactorRecvBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, reactorRecvBufSize)
		return &b
	},
}

var ioConnPool = sync.Pool{
	New: func() any { return &ioConn{} },
}

func acquireIoConn(fd int32) *ioConn {
	c := ioConnPool.Get().(*ioConn)
	c.fd = fd
	c.state = csReading
	c.recvBufP = reactorRecvBufPool.Get().(*[]byte)
	c.recvBuf = *c.recvBufP
	c.recvLen = 0
	c.hb = nil
	c.keepAlive = false
	c.bodyFd = 0
	c.bodyOff = 0
	c.bodyLen = 0
	c.release = nil
	c.releaseSlot = nil
	c.req = Request{}
	return c
}

func releaseIoConn(c *ioConn) {
	if c.recvBufP != nil {
		reactorRecvBufPool.Put(c.recvBufP)
		c.recvBufP = nil
		c.recvBuf = nil
	}
	if c.hb != nil {
		releaseHeaderBuf(c.hb)
		c.hb = nil
	}
	if c.release != nil {
		_ = c.release.Close()
		c.release = nil
	}
	if c.releaseSlot != nil {
		c.releaseSlot()
		c.releaseSlot = nil
	}
	ioConnPool.Put(c)
}

// ioReactor owns one io_uring ring and one listener fd.
type ioReactor struct {
	server     *Server
	ring       *ioUringRing
	listenerFd int32
	conns      map[int32]*ioConn

	// Closed via Server.Close() / listener close.  We test it on the
	// next loop iteration; the in-flight SQEs drain naturally.
	stop chan struct{}

	// shutdownMu serialises access to wakeupFd between close()
	// (called by Server.Close() from an arbitrary goroutine) and
	// the run() goroutine's deferred cleanup that closes the
	// eventfd.  Without the mutex, the race detector flags every
	// shutdown because close() reads wakeupFd while the run()
	// defer is concurrently writing -1 into it.  The lock is taken
	// only at shutdown, never on the request hot path.
	shutdownMu sync.Mutex
	// wakeupFd is a Linux eventfd this reactor reads from via an
	// IORING_OP_READ SQE armed at startup.  Server.Close() writes
	// 8 bytes to the eventfd, which completes the SQE and wakes
	// the reactor out of its blocking ring.enter().  Without this
	// mechanism, a quiet reactor (no traffic) cannot observe the
	// stop channel until the kernel timer or a stray client packet
	// produces some other CQE.  In tests this manifested as
	// indefinite hangs in t.Cleanup; in production it manifested
	// as a per-reactor goroutine that survived Close() for the
	// rest of the process lifetime.  Protected by shutdownMu.
	wakeupFd int32
	// wakeupBuf is the 8-byte sink for the IORING_OP_READ on the
	// eventfd; the bytes themselves are discarded but the read
	// must target valid memory the kernel can write to.
	wakeupBuf [8]byte

	// inFlight tracks goroutines the reactor has spawned that
	// outlive the reactor's run() loop: detached fallback handlers
	// (runDetached), reactor-side sendfile copies, and similar.
	// Server.Close() Waits this group with a bounded timeout
	// before declaring shutdown complete so a caller can rely on
	// "Close has returned" meaning "no reactor-spawned goroutines
	// are still touching server state".
	inFlight sync.WaitGroup

	// done is closed by run() when the reactor's main loop has
	// exited.  Server.Close() Waits on it (with timeout) so
	// callers don't have to race the run() goroutine to know
	// when ring shutdown is safe.
	done chan struct{}
}

func newIoReactor(s *Server, listenerFd int) (*ioReactor, error) {
	// COOP_TASKRUN is enabled (lets the kernel run io_uring task
	// work cooperatively with userspace); SINGLE_ISSUER and
	// DEFER_TASKRUN are deliberately OFF.  With SINGLE_ISSUER, the
	// kernel returns -EEXIST as soon as the Go runtime migrates the
	// reactor goroutine across OS threads or hands off the P during
	// the body-sendfile syscall; the workaround (runtime.LockOSThread)
	// in turn caps throughput by preventing the runtime from running
	// other goroutines on the locked thread.  Without these flags
	// the reactor goroutine is free to migrate, sendfile can release
	// its P during the kernel transfer, and observed throughput on
	// c7i.metal-24xl rises from ~110 k RPS to >1 M RPS at 4 KiB.
	flags := uint32(iouringSetupCoopTaskRun)
	ring, err := newIoUringRing(1024, flags)
	if err != nil {
		// On kernels where COOP_TASKRUN is also rejected, fall back
		// to a bare ring.
		ring, err = newIoUringRing(1024, 0)
		if err != nil {
			return nil, err
		}
	}
	// Create the wakeup eventfd.  EFD_CLOEXEC so child processes
	// don't inherit it; the initial count is 0 so the first READ
	// blocks until Close() pokes the fd.
	wakeupFd, eerr := unix.Eventfd(0, unix.EFD_CLOEXEC)
	if eerr != nil {
		ring.close()
		return nil, fmt.Errorf("reactor eventfd: %w", eerr)
	}

	return &ioReactor{
		server:     s,
		ring:       ring,
		listenerFd: int32(listenerFd),
		conns:      make(map[int32]*ioConn, 64),
		stop:       make(chan struct{}),
		wakeupFd:   int32(wakeupFd),
		done:       make(chan struct{}),
	}, nil
}

// run is the reactor's main loop.  Returns when the listener fd
// returns ENOTSOCK/EBADF (closed) or the server is shutting down.
//
// Shutdown handshake:
//
//  1. ioReactor.close() is called.  It closes the stop chan and
//     writes 8 bytes to the wakeup eventfd.
//  2. The 8-byte write completes the IORING_OP_READ SQE armed
//     on the eventfd at startup.  The kernel posts a CQE.
//  3. ring.enter() returns; drainCQ delivers the wakeup CQE
//     via handleCQE (opTagWakeup -> no-op).
//  4. Next loop iteration the stop chan read fires and run()
//     returns nil.
//  5. The deferred close(r.done) signals Close() that ring
//     teardown is safe.
func (r *ioReactor) run() (retErr error) {
	defer func() {
		r.ring.close()
		// Close the eventfd under shutdownMu so it never
		// overlaps with a concurrent close() that is reading
		// r.wakeupFd to issue a wakeup poke.  See the
		// shutdownMu doc-comment on the struct for the race
		// the mutex prevents.
		r.shutdownMu.Lock()
		if r.wakeupFd >= 0 {
			_ = unix.Close(int(r.wakeupFd))
			r.wakeupFd = -1
		}
		r.shutdownMu.Unlock()
		close(r.done)
	}()

	// Arm the wakeup READ SQE before the first enter().
	if err := r.ring.prepRead(r.wakeupFd, r.wakeupBuf[:], packUserData(opTagWakeup, 0)); err != nil {
		return fmt.Errorf("reactor prepRead(wakeupFd=%d): %w", r.wakeupFd, err)
	}

	// Submit the initial ACCEPT.  We always keep exactly one ACCEPT
	// outstanding; on each accept completion we re-arm.
	if err := r.ring.prepAccept(r.listenerFd, packUserData(opTagAccept, 0)); err != nil {
		return fmt.Errorf("reactor prepAccept(listenerFd=%d): %w", r.listenerFd, err)
	}

	for {
		select {
		case <-r.stop:
			return nil
		default:
		}

		toSubmit := r.ring.sqPending()
		if err := r.ring.enter(toSubmit, 1); err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("reactor enter(toSubmit=%d, minComplete=1): %w", toSubmit, err)
		}

		r.ring.drainCQ(r.handleCQE)
	}
}

// close signals the reactor to stop and wakes it out of its
// blocking ring.enter() call so it observes the stop signal
// on the next loop iteration.  Safe to call from any goroutine
// and safe to call multiple times.
//
// We take shutdownMu around the wakeupFd touch so the eventfd
// write cannot race the run() goroutine's deferred eventfd
// close.  Under the mutex, either:
//
//   - close() runs first: writes to the live eventfd; run()'s
//     defer later observes wakeupFd >= 0 and closes it.
//   - run() defer runs first: writes wakeupFd = -1; close()
//     observes that and skips the write entirely.
//
// In both orderings there is no concurrent fd recycling, which
// is the actual safety hazard (writing to a closed fd whose
// number was reused by some other subsystem).
func (r *ioReactor) close() {
	r.shutdownMu.Lock()
	defer r.shutdownMu.Unlock()
	select {
	case <-r.stop:
		return
	default:
		close(r.stop)
	}
	if r.wakeupFd >= 0 {
		var poke = [8]byte{1, 0, 0, 0, 0, 0, 0, 0}
		_, _ = unix.Write(int(r.wakeupFd), poke[:])
	}
}

// waitDone blocks until the reactor's run() loop has fully
// exited, or the supplied deadline elapses.  Returns true on a
// clean shutdown, false on timeout.  Safe to call after
// close().
func (r *ioReactor) waitDone(timeout time.Duration) bool {
	select {
	case <-r.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// waitInFlight blocks until every detached/spawned goroutine the
// reactor created (tracked via r.inFlight) has returned, or the
// timeout fires.  Mirrors http.Server's Shutdown semantics for
// the slow-path side of the reactor.
func (r *ioReactor) waitInFlight(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		r.inFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (r *ioReactor) handleCQE(userData uint64, res int32, _ uint32) {
	op, fd := unpackUserData(userData)
	switch op {
	case opTagAccept:
		r.onAccept(res)
	case opTagRecv:
		r.onRecv(fd, res)
	case opTagWrite:
		r.onWrite(fd, res)
	case opTagWakeup:
		// No-op; the CQE's only purpose is to unblock
		// ring.enter() so the next loop iteration can
		// observe r.stop.  We deliberately do NOT re-arm
		// the eventfd READ because by the time we see a
		// wakeup CQE, close() has already fired and the
		// run loop will exit before submitting more SQEs.
	case opTagClose:
		// nothing to do — fd is gone
	}
}

func (r *ioReactor) onAccept(res int32) {
	// Re-arm ACCEPT immediately so the listener keeps draining.
	_ = r.ring.prepAccept(r.listenerFd, packUserData(opTagAccept, 0))

	if res < 0 {
		// Likely ENFILE / EMFILE / ECANCELED on shutdown.  Skip this
		// accept; the next one will pick up if it's transient.
		return
	}
	connFd := res

	// Per-IP cap (mirror of acceptLoop's pre-Phase-5 gap).
	// The reactor previously omitted this check, leaving the
	// io_uring path open to single-host fd exhaustion even
	// when -max-conns-per-ip was set.  Documented as a known
	// asymmetry in docs/security.md; this
	// closes it.  When MaxConnsPerIP == 0 (cap disabled) the
	// helper short-circuits to a noop release and the slot is
	// effectively free.
	slot, admit := r.server.acquirePerIPSlotFromFd(int(connFd))
	if !admit {
		// Cap exceeded for this peer.  Close immediately with
		// no response; the std-lib acceptLoop does the same.
		// We bypass prepClose's CQE dance because the fd is
		// not yet tracked in r.conns -- a synchronous close
		// is the right call.
		_ = unix.Close(int(connFd))
		telemetry.IncProxyPerIPCapDropped()
		return
	}

	if r.server.SocketSendBuf > 0 {
		_ = unix.SetsockoptInt(int(connFd), unix.SOL_SOCKET, unix.SO_SNDBUF, r.server.SocketSendBuf)
	} else if r.server.SocketSendBuf == 0 {
		_ = unix.SetsockoptInt(int(connFd), unix.SOL_SOCKET, unix.SO_SNDBUF, defaultSocketSendBuf)
	}
	// TCP_NODELAY is required: io_uring WRITE for the header completes
	// before we issue the body sendfile(), and without NODELAY the
	// kernel's Nagle's algorithm inserts a ~40 ms delay between the
	// two segments on every warm response (observed: p50 ~50 ms,
	// throughput ~100 RPS).  Setting NODELAY collapses that gap.
	_ = unix.SetsockoptInt(int(connFd), unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

	// SO_SNDTIMEO bounds how long the BLOCKING reactor sendfile(2) waits
	// for send-buffer space before returning EAGAIN.  Because a partial
	// send returns the bytes shipped (not an error), this behaves as an
	// IDLE deadline: a client that keeps draining makes progress every
	// window and never trips it, while a client that stalls for the full
	// write-timeout window trips EAGAIN and reactorSendfile closes the
	// connection.  This is the reactor-path analogue of the cold-path
	// sliding SetWriteDeadline and the classic warm-path deadline pump;
	// without it a single stalled client would pin the reactor goroutine
	// in sendfile() and head-of-line-block every other connection on this
	// listener.
	if writeDl := r.effectiveWriteTimeout(); writeDl > 0 {
		tv := unix.NsecToTimeval(writeDl.Nanoseconds())
		_ = unix.SetsockoptTimeval(int(connFd), unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)
	}

	c := acquireIoConn(connFd)
	c.releaseSlot = slot
	r.conns[connFd] = c
	if err := r.ring.prepRecv(connFd, c.recvBuf, packUserData(opTagRecv, connFd)); err != nil {
		r.closeConn(c, "prep recv: "+err.Error())
		return
	}
}

func (r *ioReactor) onRecv(fd, res int32) {
	c := r.conns[fd]
	if c == nil {
		return
	}
	if res <= 0 {
		// 0 == clean EOF; <0 == errno.  Either way we're done.
		r.closeConn(c, "")
		return
	}
	if c.recvLen+int(res) > cap(c.recvBuf) {
		r.closeConn(c, "recv overflow")
		return
	}
	c.recvLen += int(res)

	// Try to parse a single request out of the accumulated buf.
	consumed, perr := parseRequestFromBuf(c.recvBuf[:c.recvLen], &c.req)
	if perr == errNeedMoreData {
		// Re-arm RECV at the current write offset.
		if err := r.ring.prepRecv(fd, c.recvBuf[c.recvLen:], packUserData(opTagRecv, fd)); err != nil {
			r.closeConn(c, "rearm recv: "+err.Error())
		}
		return
	}
	if perr != nil {
		// Mirror the std-lib serveConn parser-error counter
		// taxonomy so a deployment that toggles -iouring on
		// or off keeps producing the same monitoring signal.
		// See writeParserErrorResponse for the canonical
		// taxonomy (errBadRequest / errSmugglingSuspect /
		// errHeaderTooLarge).
		incParserErrorCounter(perr)
		r.closeConn(c, "parse: "+perr.Error())
		return
	}

	// We have a parsed request.  Slide leftover bytes left (HTTP/1.1
	// allows the client to pipeline the next request after the
	// header block, though our keep-alive bench does one in flight).
	leftover := c.recvLen - consumed
	if leftover > 0 {
		copy(c.recvBuf, c.recvBuf[consumed:c.recvLen])
	}
	c.recvLen = leftover

	if !r.gateAdmit(c) {
		return
	}

	if r.tryServeWarmReactor(c) {
		return
	}

	// Anything else falls through to the std-lib slow path on a
	// dup'd fd.  This is rare for the warm-hit bench but mandatory
	// for production correctness.
	r.detachToFallback(c)
}

// gateAdmit consults Server.AuthGate (if any) for the parsed request
// on c.  Returns true when the request may proceed; on rejection it
// writes a 401 response synchronously on the raw fd and closes the
// connection via the reactor's normal close path, then returns false.
//
// Writing the rejection synchronously (one unix.Write) instead of
// plumbing it through the io_uring state machine keeps the gate
// implementation tiny: the warm-hit response path's onWrite/
// reactorSendfile chain is set up for header+body fusion, which a
// one-shot 401 doesn't need.  The cost is one extra syscall on the
// reject path -- not on any throughput target.
func (r *ioReactor) gateAdmit(c *ioConn) bool {
	s := r.server
	if s == nil || s.AuthGate == nil {
		if s != nil && s.RequireAuth {
			// Fail closed: RequireAuth without a gate is a
			// misconfiguration; never serve an unauthenticated plane.
			buf := renderAuthReject(503, "data plane requires authentication but no gate is configured")
			_, _ = unix.Write(int(c.fd), buf)
			r.closeConn(c, "auth required, no gate")
			return false
		}
		return true
	}
	status, reason := s.AuthGate.Check(context.Background(), c.req.Auth, c.req.Path)
	if status == 0 {
		return true
	}
	buf := renderAuthReject(status, reason)
	_, _ = unix.Write(int(c.fd), buf)
	r.closeConn(c, "auth rejected")
	return false
}

// tryServeWarmReactor renders a warm-hit response: stages the header
// in c.hb, submits an io_uring WRITE, and remembers the body
// coordinates for after the WRITE CQE arrives.  Returns true if the
// request was a warm hit and the response is now in flight.
func (r *ioReactor) tryServeWarmReactor(c *ioConn) bool {
	s := r.server
	req := &c.req
	if !bytes.Equal(req.Method, []byte("GET")) {
		return false
	}
	host := defaultHost(s.Cfg)
	path := bytesToStringUnsafe(req.Path)
	if !classify.ArtifactGET(host, host, "GET", path) {
		return false
	}
	keyQuery := bytesToStringUnsafe(req.Query)
	if cache.IsContentAddressedHost(host) {
		keyQuery = ""
	}
	// Authorization is validated by gateAdmit, but it is NEVER part of the
	// content cache key: the cached artifact is byte-identical no matter
	// which caller's token fetched it.  This MUST match proxy.Handler's
	// keyAuth="" convention (see internal/proxy/handler.go) or every warm
	// lookup silently misses and the reactor detaches every request to the
	// slow fallback path.
	const keyAuth = ""
	keyHex := s.keyHexC.Get("GET", host, path, keyQuery, keyAuth)

	meta, err := s.Store.LoadMeta(keyHex)
	if err != nil || meta == nil || len(meta.Spans) == 0 {
		telemetry.IncCacheMiss()
		return false
	}
	total := int64(-1)
	if meta.Total != nil {
		total = *meta.Total
	}
	var wantStart, wantEnd int64
	if len(req.Range) == 0 {
		if total < 0 {
			telemetry.IncCacheMiss()
			return false
		}
		wantStart, wantEnd = 0, total
	} else {
		var ok bool
		wantStart, wantEnd, ok = cache.ParseSingleRange(bytesToStringUnsafe(req.Range), total)
		if !ok || wantEnd < 0 {
			telemetry.IncCacheMiss()
			return false
		}
	}
	if !cache.Covers(meta.Spans, wantStart, wantEnd) {
		telemetry.IncCacheMiss()
		return false
	}
	bh, release, err := s.Store.AcquireBody(keyHex)
	if err != nil {
		telemetry.IncCacheMiss()
		return false
	}
	telemetry.IncCacheHit()
	telemetry.IncIoUringFusedCall()

	inFd, fdErr := bh.Fd()
	if fdErr != nil {
		_ = release.Close()
		return false
	}

	n := wantEnd - wantStart
	hb := acquireHeaderBuf()
	if len(req.Range) == 0 {
		writeWarmHead200Buf(hb, n, meta, req.KeepAlive)
	} else {
		writeWarmHead206Buf(hb, wantStart, wantEnd, total, n, meta, req.KeepAlive)
	}

	c.hb = hb
	c.keepAlive = req.KeepAlive
	c.bodyFd = inFd
	c.bodyOff = wantStart
	c.bodyLen = n
	c.release = release
	c.state = csWriting

	if err := r.ring.prepSend(c.fd, hb.bytes(), packUserData(opTagWrite, c.fd)); err != nil {
		r.closeConn(c, "prep send: "+err.Error())
		return true
	}
	return true
}

func (r *ioReactor) onWrite(fd, res int32) {
	c := r.conns[fd]
	if c == nil {
		return
	}
	if res < 0 {
		r.closeConn(c, "write errno")
		return
	}
	if c.hb == nil {
		r.closeConn(c, "write without header staged")
		return
	}
	if int(res) != len(c.hb.bytes()) {
		// Short write on header — extremely rare for sub-MSS headers
		// on loopback, but we can't recover gracefully without
		// re-submitting; close.
		r.closeConn(c, "short header write")
		return
	}
	headerBytes := int64(len(c.hb.bytes()))
	releaseHeaderBuf(c.hb)
	c.hb = nil

	// Body via raw sendfile(2).  Loopback warm-hit completes in one
	// shot for payloads <= SO_SNDBUF (4 MiB).  Larger payloads may
	// EAGAIN, in which case we briefly block this reactor goroutine
	// on poll-then-retry — acceptable for cache-miss/elephant body
	// rates, but the saturate bench targets the single-shot path.
	written, sfErr := reactorSendfile(c.fd, int32(c.bodyFd), c.bodyOff, c.bodyLen)
	if c.release != nil {
		_ = c.release.Close()
		c.release = nil
	}
	c.bodyFd = 0
	if sfErr != nil || written != c.bodyLen {
		r.closeConn(c, "sendfile body")
		return
	}
	telemetry.AddClientBytesServed(headerBytes + written)

	if !c.keepAlive {
		r.closeConn(c, "")
		return
	}

	// Keep-alive: stay open for the next request.  Leftover bytes (if
	// the client pipelined) live in c.recvBuf[:c.recvLen]; otherwise
	// recvLen==0 and we arm a fresh RECV.
	c.state = csReading
	if c.recvLen > 0 {
		// Try parsing again synchronously — there might already be a
		// full request buffered from a pipelined client.
		consumed, perr := parseRequestFromBuf(c.recvBuf[:c.recvLen], &c.req)
		if perr == nil {
			leftover := c.recvLen - consumed
			if leftover > 0 {
				copy(c.recvBuf, c.recvBuf[consumed:c.recvLen])
			}
			c.recvLen = leftover
			if !r.gateAdmit(c) {
				return
			}
			if r.tryServeWarmReactor(c) {
				return
			}
			r.detachToFallback(c)
			return
		}
		if perr != errNeedMoreData {
			incParserErrorCounter(perr)
			r.closeConn(c, "parse pipeline")
			return
		}
	}
	if err := r.ring.prepRecv(c.fd, c.recvBuf[c.recvLen:], packUserData(opTagRecv, c.fd)); err != nil {
		r.closeConn(c, "rearm recv after write")
	}
}

func (r *ioReactor) closeConn(c *ioConn, _ string) {
	delete(r.conns, c.fd)
	// IORING_OP_CLOSE so the close itself is async; user_data uses
	// opTagClose so handleCQE just ignores the completion.
	_ = r.ring.prepClose(c.fd, packUserData(opTagClose, c.fd))
	releaseIoConn(c)
}

// detachToFallback removes c from the reactor, dups its fd to an
// *os.File + net.FileConn, and runs the slow path in a goroutine.
// Any bytes still in c.recvBuf are pre-pended via an io.MultiReader
// so the fallback sees the original wire stream.
func (r *ioReactor) detachToFallback(c *ioConn) {
	fd := c.fd
	leftover := make([]byte, c.recvLen)
	copy(leftover, c.recvBuf[:c.recvLen])
	rawHeader := make([]byte, len(c.req.Raw))
	copy(rawHeader, c.req.Raw)
	req := c.req
	req.Raw = rawHeader

	// Transfer per-IP slot ownership from the ioConn to the
	// detached goroutine.  Without this hand-off the slot would
	// be released by releaseIoConn() below, freeing it while the
	// fallback connection is still open -- defeating the cap for
	// any peer whose request happens to fall through to the
	// stdlib path (which is exactly the path a misbehaving
	// client is most likely to take, since their request often
	// fails the warm-hit predicate).
	slot := c.releaseSlot
	c.releaseSlot = nil

	delete(r.conns, fd)
	releaseIoConn(c)

	// Dup the fd into Go's net package without going through the
	// io_uring submission queue.  os.NewFile takes ownership; we
	// then use net.FileConn which dup's again so we can safely close
	// our half.
	f := os.NewFile(uintptr(fd), "iouring-detached")
	nc, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		if slot != nil {
			slot()
		}
		return
	}

	// Track the goroutine so Server.Close() can wait for it to
	// drain before declaring shutdown complete.  Add() MUST be
	// called before the `go` so a racing Close() observes the
	// counter.
	r.inFlight.Add(1)
	go r.runDetached(nc, &req, leftover, slot)
}

// runDetached drives one request (already parsed) plus its leftover
// bytes through the existing Fallback path.  After Fallback returns,
// the connection is closed: we do NOT loop, because pipelining edge
// cases on a detached conn are not worth the complexity for the rare
// miss-path.
//
// slot, when non-nil, releases this peer's per-IP cap reservation
// when the fallback connection finishes.  Ownership was transferred
// from the originating ioConn in detachToFallback().
func (r *ioReactor) runDetached(nc net.Conn, req *Request, leftover []byte, slot perIPSlot) {
	defer r.inFlight.Done()
	defer nc.Close()
	if slot != nil {
		defer slot()
	}

	// Bound the lifetime of this conn so a slow/dead client
	// cannot pin the goroutine indefinitely.  See the
	// detachedConnDefaultDeadline doc-comment for the rationale
	// and how this mirrors the cork+sendfile path's
	// SetReadDeadline / SetWriteDeadline behaviour.
	writeDl := r.applyDetachedDeadlines(nc)

	if r.server.Fallback == nil {
		bw := bufio.NewWriterSize(nc, bufWriteSize)
		writeStatus(bw, 503, "Service Unavailable")
		_ = bw.Flush()
		return
	}

	// The Fallback signature expects a *bufio.Reader positioned just
	// after the header block.  leftover holds whatever followed the
	// header block (typically empty for keep-alive GETs).  We stitch
	// leftover ahead of the conn's stream so the underlying read
	// position is correct from the handler's point of view.
	var src io.Reader = nc
	if len(leftover) > 0 {
		src = io.MultiReader(bytes.NewReader(leftover), nc)
	}
	br := bufio.NewReaderSize(src, bufReadSize)
	bw := bufio.NewWriterSize(nc, bufWriteSize)
	if err := r.server.Fallback(context.Background(), nc, writeDl, bw, br, req); err != nil {
		return
	}
	_ = bw.Flush()
}

// applyDetachedDeadlines installs read + write deadlines on a
// detached fallback conn.  Prefers the Server's configured
// ReadTimeout / WriteTimeout (or the Cfg-side defaults); falls
// back to detachedConnDefaultDeadline.  Silently no-ops if the
// conn does not implement SetReadDeadline / SetWriteDeadline
// (test fakes, in-memory pipes, etc.).  Returns the write deadline
// budget it armed so the fallback can slide it forward as the body
// streams (see FallbackFunc).
func (r *ioReactor) applyDetachedDeadlines(nc net.Conn) time.Duration {
	readDl := r.server.ReadTimeout
	if readDl == 0 && r.server.Cfg != nil {
		readDl = r.server.Cfg.ReadTimeout
	}
	if readDl == 0 {
		readDl = detachedConnDefaultDeadline
	}
	writeDl := r.effectiveWriteTimeout()
	now := time.Now()
	_ = nc.SetReadDeadline(now.Add(readDl))
	_ = nc.SetWriteDeadline(now.Add(writeDl))
	return writeDl
}

// effectiveWriteTimeout resolves the write-timeout budget for this
// reactor: the Server's WriteTimeout, then the Cfg default, then the
// hard detachedConnDefaultDeadline floor.  Used both to arm detached
// fallback conns and as the SO_SNDTIMEO idle budget on warm reactor
// sockets (see onAccept / reactorSendfile).
func (r *ioReactor) effectiveWriteTimeout() time.Duration {
	writeDl := r.server.WriteTimeout
	if writeDl == 0 && r.server.Cfg != nil {
		writeDl = r.server.Cfg.WriteTimeout
	}
	if writeDl == 0 {
		writeDl = detachedConnDefaultDeadline
	}
	return writeDl
}

// errReactorClientStalled signals that the blocking reactor sendfile(2)
// hit its SO_SNDTIMEO idle window with no send-buffer progress: the
// client stopped draining for the full write-timeout.  The caller closes
// the connection rather than retrying.
var errReactorClientStalled = errors.New("coreserver: reactor sendfile client stalled (SO_SNDTIMEO)")

// reactorSendfile performs a blocking sendfile(2) inside the reactor
// goroutine.  Because IORING_OP_ACCEPT is configured WITHOUT
// SOCK_NONBLOCK, the accepted socket is in default blocking mode and
// sendfile() blocks in the kernel until the body drains.  This keeps the
// reactor on one syscall per body, no userspace poll loop, no busy-wait.
//
// The socket carries SO_SNDTIMEO (= the effective write timeout, set in
// onAccept), so a client that stalls cannot pin sendfile() forever: the
// call returns EAGAIN once the idle window elapses with no progress.  A
// partial send returns the bytes shipped (not an error), so a steadily
// draining client keeps looping without ever tripping the timeout.
func reactorSendfile(outFd, inFd int32, off, n int64) (int64, error) {
	var total int64
	for total < n {
		remaining := n - total
		offset := off + total
		batch := maxSendfileSlice(n, remaining)
		wrote, errno := unix.Sendfile(int(outFd), int(inFd), &offset, int(batch))
		if wrote > 0 {
			total += int64(wrote)
			telemetry.IncSendfileBodyOnlyCall()
		}
		switch errno {
		case nil:
			if wrote == 0 {
				return total, io.ErrUnexpectedEOF
			}
		case unix.EINTR:
			continue
		case unix.EAGAIN:
			// SO_SNDTIMEO elapsed without send-buffer progress.
			// (unix.EWOULDBLOCK aliases EAGAIN on Linux.)
			telemetry.IncSendfileEAGAIN()
			return total, errReactorClientStalled
		default:
			return total, errno
		}
	}
	return total, nil
}

// startReactors wires reactor mode onto a set of (already opened)
// listeners.  Returns nil + a slice of reactor handles on success.
// On any error the partially-constructed reactors are torn down.
func (s *Server) startReactors(lns []net.Listener) ([]*ioReactor, error) {
	rs := make([]*ioReactor, 0, len(lns))
	for i, ln := range lns {
		fd, err := listenerFdFor(ln)
		if err != nil {
			for _, r := range rs {
				r.close()
			}
			return nil, fmt.Errorf("listener[%d] dup fd: %w", i, err)
		}
		r, err := newIoReactor(s, fd)
		if err != nil {
			for _, prior := range rs {
				prior.close()
			}
			return nil, fmt.Errorf("listener[%d] newIoReactor(fd=%d): %w", i, fd, err)
		}
		rs = append(rs, r)
	}
	return rs, nil
}

// listenerFdFor returns a non-blocking, dup'd fd suitable for use
// with io_uring ACCEPT.  The original net.Listener stays alive on the
// caller's side; closing it ALSO closes the kernel-side listener
// (since both fds refer to the same open file description after dup).
// We set our dup nonblocking explicitly so ACCEPT returns -EAGAIN
// when the accept queue is empty.
func listenerFdFor(ln net.Listener) (int, error) {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return -1, errors.New("io_uring reactor: listener is not *net.TCPListener")
	}
	f, err := tl.File()
	if err != nil {
		return -1, err
	}
	fd := int(f.Fd())
	// File() returns a dup'd, blocking fd.  Mark it nonblocking.
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = f.Close()
		return -1, err
	}
	// We must keep the *os.File alive so the runtime finalizer
	// doesn't close our fd out from under us.  Stash it on a package
	// global keyed by fd; the reactor's close() drops the reference.
	listenerFiles.Store(int32(fd), f)
	return fd, nil
}

// listenerFiles keeps a strong reference to each *os.File we extract
// via TCPListener.File(), keyed by the integer fd we use with
// io_uring.  Without this, Go's finalizer would close the fd
// asynchronously.
var listenerFiles fileMap

type fileMap struct {
	mu sync.Mutex
	m  map[int32]*os.File
}

func (f *fileMap) Store(fd int32, file *os.File) {
	f.mu.Lock()
	if f.m == nil {
		f.m = make(map[int32]*os.File)
	}
	f.m[fd] = file
	f.mu.Unlock()
}

func (f *fileMap) Delete(fd int32) {
	f.mu.Lock()
	if f.m != nil {
		if file := f.m[fd]; file != nil {
			_ = file.Close()
		}
		delete(f.m, fd)
	}
	f.mu.Unlock()
}

// acquirePerIPSlotFromFd is the io_uring-side counterpart of
// Server.acquirePerIPSlot (which takes a net.Addr).  The reactor
// holds a raw connection fd before it has been wrapped in a
// net.Conn, so we resolve the peer address via unix.Getpeername
// here.  Same semantics as the std-lib helper:
//
//   - (release, true)  admit; caller MUST call release exactly once
//     on close (we store it on ioConn.releaseSlot and hand it off
//     to the detached fallback if applicable).
//   - (nil, false)     cap exceeded; caller must close fd and
//     increment the drop counter.
//   - (noop, true)     MaxConnsPerIP == 0 (cap disabled), or peer
//     address unparseable (non-IP socket); identical behaviour to
//     the std-lib helper's tcp-cast-failed branch.
func (s *Server) acquirePerIPSlotFromFd(fd int) (perIPSlot, bool) {
	if s.MaxConnsPerIP <= 0 {
		return func() {}, true
	}
	sa, err := unix.Getpeername(fd)
	if err != nil {
		// getpeername(2) failed: socket may have been torn
		// down between accept and now.  Admit (best-effort);
		// the recv path will close cleanly.  We deliberately
		// do NOT count this as a drop because the cap is
		// inapplicable, not over.
		return func() {}, true
	}
	var addr netip.Addr
	switch v := sa.(type) {
	case *unix.SockaddrInet4:
		addr = netip.AddrFrom4(v.Addr)
	case *unix.SockaddrInet6:
		addr = netip.AddrFrom16(v.Addr).Unmap()
	default:
		// AF_UNIX or other; not a TCP peer we want to cap.
		return func() {}, true
	}

	s.connsPerIPMu.Lock()
	if s.connsPerIP == nil {
		s.connsPerIP = make(map[netip.Addr]int, 64)
	}
	if s.connsPerIP[addr] >= s.MaxConnsPerIP {
		s.connsPerIPMu.Unlock()
		return nil, false
	}
	s.connsPerIP[addr]++
	s.connsPerIPMu.Unlock()

	return func() {
		s.connsPerIPMu.Lock()
		n := s.connsPerIP[addr] - 1
		if n <= 0 {
			delete(s.connsPerIP, addr)
		} else {
			s.connsPerIP[addr] = n
		}
		s.connsPerIPMu.Unlock()
	}, true
}
