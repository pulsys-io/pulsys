// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package coreserver implements a minimal, allocation-frugal HTTP/1.1 server
// dedicated to pulsys's warm cache-hit fast path.
//
// Why a custom server?  Profiling the fasthttp-based ingress showed that the
// remaining ~7-13 allocs per warm request all originated inside fasthttp
// itself (Response.Write → writeBodyStream → copyZeroAlloc, plus
// bytebufferpool growth events).  These cannot be removed without forking
// fasthttp.  This package replaces fasthttp on the ingress side with a tiny
// HTTP/1.1 implementation tailored to the proxy's needs:
//
//   - One pooled *bufio.Reader / *bufio.Writer per connection (zero allocs at
//     steady state).
//   - One pooled fixed-size header scratch buffer per request; the parser
//     populates a stack-allocated miniRequest whose fields are []byte slices
//     pointing into the scratch buffer.  No strings, no maps, no per-header
//     allocations.
//   - A pre-rendered response head template per cached object (status line +
//     Content-Type + ETag + Connection) cached on first warm hit.
//   - The body is written via io.CopyBuffer with a pooled 64 KiB buffer.  On
//     Linux/macOS, the underlying *os.File → *net.TCPConn copy resolves to a
//     splice/sendfile syscall (Go's net package does this transparently for
//     readFrom).
//
// What it does NOT do:
//
//   - HTTP/2 or HTTP/3.
//   - Chunked request bodies.
//   - Trailers.
//   - Anything beyond what pulsys's hot path actually exercises.
//
// Misses, non-GET methods, and any request that doesn't match the fast-path
// shape fall back to the supplied Fallback handler (typically the existing
// fasthttp pipeline), so production safety is preserved.
package coreserver

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/classify"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/telemetry"
)

// FallbackFunc is invoked for any request the coreserver does not handle on
// its fast path.  Implementations typically wrap the existing fasthttp
// handler and write a complete HTTP/1.1 response into bw.  The supplied
// *bufio.Reader is positioned just after the request headers; the fallback
// is responsible for reading any request body itself.  Returning an error
// causes the connection to be closed.
//
// conn is the underlying connection that bw writes through, and
// writeTimeout is the absolute write deadline budget the caller already
// armed on conn.  They are passed so the fallback can slide the write
// deadline forward (conn.SetWriteDeadline(now+writeTimeout)) as the
// response body makes progress, converting the absolute WriteTimeout into
// an idle deadline for large, slow streams; see HandlerFallback.  conn
// may be nil (and/or writeTimeout zero) for callers/tests that have no
// addressable conn, in which case the fallback skips deadline management.
type FallbackFunc func(ctx context.Context, conn net.Conn, writeTimeout time.Duration, bw *bufio.Writer, br *bufio.Reader, req *Request) error

// Request is the parsed view of one HTTP/1.1 request.  All slice fields
// alias the per-request scratch buffer and are valid only until the next
// request is read on the same connection.
//
// Raw contains the full request line + header block + terminating CRLF
// CRLF as it was read off the wire.  Fallback handlers can feed Raw
// (followed by the body still buffered in the *bufio.Reader) to
// net/http's http.ReadRequest to reconstruct a fully-fledged
// *http.Request without any byte-level re-emission cost.
type Request struct {
	Method     []byte
	RequestURI []byte
	Path       []byte
	Query      []byte
	Host       []byte
	Auth       []byte
	Range      []byte
	Raw        []byte
	ContentLen int64
	KeepAlive  bool
	HTTP11     bool
}

// AuthGate decides whether an incoming request may proceed.
// Implementations receive the raw Authorization header value (which
// may be empty) and the request path; returning a non-zero status
// code rejects the request with that status and the supplied reason
// as a short plain-text response body.  Returning 0 admits the
// request and the normal warm-hit / fallback dispatch runs.
//
// The gate is invoked on every request, including warm cache hits,
// so a revoked or forged credential cannot replay a cached object.
// Implementations are expected to be allocation-frugal -- this runs
// on the proxy's hottest path -- but the typical implementation
// (single store lookup, optionally backed by a tiny TTL cache) is
// already negligible next to the cost of writing a response.
//
// AuthGate is nil-safe: when Server.AuthGate is nil, no enforcement
// runs and every request is admitted.  This preserves the existing
// dev-mode behavior when no Pulsys DB is wired (cmd/pulsys/main.go
// only installs a gate when PULSYS_DB_DSN is set).
type AuthGate interface {
	Check(ctx context.Context, auth, path []byte) (status int, reason string)
}

// AuthGateFunc adapts a plain function into an AuthGate.  Useful for
// tests and inline policies.
type AuthGateFunc func(ctx context.Context, auth, path []byte) (int, string)

// Check implements AuthGate.
func (f AuthGateFunc) Check(ctx context.Context, auth, path []byte) (int, string) {
	return f(ctx, auth, path)
}

// Server owns a single TCP listener and dispatches accepted connections to
// the warm-hit fast path or to Fallback.
type Server struct {
	Cfg      *config.Config
	Store    *cache.Store
	Fallback FallbackFunc

	// AuthGate, when non-nil, is consulted for every request before
	// either tryServeWarm or Fallback runs.  A non-zero status from
	// the gate short-circuits the request with that status and the
	// gate's reason as the response body, never reading the cache or
	// invoking Fallback.  Nil = no enforcement, UNLESS RequireAuth is
	// set (see below).  The internal import loopback and unit tests
	// leave both fields zero so a nil gate stays pass-through.
	AuthGate AuthGate

	// RequireAuth makes the data plane fail closed when AuthGate is
	// nil: every request is rejected instead of admitted.  The public
	// pulsys binary sets this so a refactor that drops the gate can
	// never silently expose an unauthenticated cache.  It is a
	// defense-in-depth backstop; cmd/pulsys/main.go already refuses to
	// start without a real gate.
	RequireAuth bool

	// ReadTimeout / WriteTimeout default to Cfg's values when zero.
	//
	// ReadTimeout is the wall-clock budget for completing the
	// REQUEST BODY once headers are parsed.  It is intentionally
	// generous (default 300s) because legitimate clients legitimately
	// PUT multi-GiB LFS objects.  Slowloris-class attacks against
	// header parsing are bounded by ReadHeaderTimeout, NOT this.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// IdleTimeout bounds the time we wait for the FIRST byte of a
	// request on a freshly-accepted OR keep-alive-idle connection.
	// Zero defaults to defaultIdleTimeout (60s) so the legitimate
	// keep-alive pattern (hf-cli holds one connection across many
	// range requests) keeps working without exposing an unbounded
	// idle window.  A peer that opens a connection and never sends
	// a byte is closed after this elapses.
	IdleTimeout time.Duration

	// ReadHeaderTimeout bounds the time between the FIRST byte of
	// a request arriving and the full request line + headers being
	// parsed.  Zero defaults to defaultReadHeaderTimeout (5s).  A
	// peer that sends "GET /" and then dribbles header bytes for
	// minutes is closed after this elapses.
	//
	// Distinct from IdleTimeout because we want a long keep-alive
	// idle window (60s) but a short header-arrival deadline (5s)
	// once a request is in flight.  Splitting them costs one extra
	// SetReadDeadline syscall per request (sub-µs on Linux); see
	// the benchmark in BenchmarkCoreServerWarm_* for the empirical
	// floor delta.
	ReadHeaderTimeout time.Duration

	// MaxConnsPerIP, when > 0, caps the number of simultaneously
	// open accepted connections from any single peer IP.  New
	// connections beyond the cap are immediately closed by the
	// accept loop (no response is written, no goroutine is
	// spawned) so an attacker cannot exhaust goroutine count by
	// opening tens of thousands of half-open sockets.
	//
	// Zero (the default) disables the cap entirely so dev / single-
	// node deployments behind a hardened LB stay unchanged.  The
	// LB SHOULD be the primary connection-flood line; this is
	// belt-and-suspenders for the case where an attacker reaches
	// pulsys directly (trusted-network actor, mis-configured
	// VPC, etc.).
	//
	// Counted per netip.Addr.  IPv6 connections are counted by
	// their full address; we deliberately do NOT collapse to /64
	// here because the operator scenario is an internal attacker
	// from a single host, not the public IPv6 sub-prefix case
	// which is the LB's job.
	MaxConnsPerIP int

	// connsPerIP tracks the current open count per peer IP under
	// MaxConnsPerIP enforcement.  Nil when MaxConnsPerIP == 0 so
	// we pay zero hot-path cost in deployments that haven't opted
	// in.  Read/written under connsPerIPMu.
	connsPerIP   map[netip.Addr]int
	connsPerIPMu sync.Mutex

	// SocketSendBuf, when > 0, is the SO_SNDBUF setting applied to
	// every accepted *net.TCPConn.  Increasing the kernel's send
	// buffer beyond its default (~128 KiB on macOS, ~4 MiB on Linux
	// after autotune ramps up) reduces the number of sendfile(2)
	// syscalls required for large bodies: the kernel can absorb more
	// of the response in one transit, so EAGAIN-driven re-entries
	// drop proportionally.  Defaults to 4 MiB; set to a negative
	// value to skip the SetWriteBuffer call entirely.
	SocketSendBuf int

	// TCPCork, when true and on Linux, wraps the write(headers) +
	// sendfile(body) pair in TCP_CORK to coalesce them into a single
	// outbound TCP segment.  On Darwin this is a no-op because
	// sendfile(2)+sf_hdtr already fuses the two into one syscall.
	// Default is true (Linux: cork on; Darwin: unused).
	TCPCork bool

	// IoUring, when true and the kernel supports it (>= 6.1), uses
	// io_uring linked WRITE+SPLICE SQEs to ship header+body in a
	// single io_uring_enter call -- the Linux equivalent of Darwin's
	// sf_hdtr fusion.  Falls back to TCPCork (or plain sendfile if
	// TCPCork is also false) when the kernel is too old or io_uring
	// setup fails.  Linux only; ignored on Darwin.
	IoUring bool

	// ioUringReady is set true by ioUringInit when io_uring is both
	// requested AND the kernel supports it AND ring setup succeeded.
	// The hot path consults this flag to decide between the io_uring
	// submission and the cork+sendfile path; checking a bool is
	// cheaper than retrying ring setup on every request.
	ioUringReady atomic.Bool

	// iouPool holds per-GOMAXPROCS io_uring rings used by the legacy
	// per-conn fused-write path (Linux only).  Nil when reactor mode
	// (Option B) is active.
	iouPool *ioUringPool

	// iouReactors holds one io_uring reactor per listener when
	// reactor mode (Option B) is active.  Nil otherwise.
	//
	// Reactors are appended exactly once by runReactors() and read
	// exactly once by Close() from another goroutine.  We gate
	// access through reactorsMu so the race detector stays happy;
	// this is not in the hot path (only Serve start / shutdown).
	iouReactors   []*ioReactor
	iouReactorsMu sync.Mutex

	keyHexC cache.KeyHexCache
	closed  atomic.Bool
}

// defaultSocketSendBuf is the value used when Server.SocketSendBuf is
// zero.  4 MiB is large enough to absorb the typical 32 MiB hf-cli
// range chunk in 8 sendfile(2) round-trips instead of ~256 (default
// macOS SO_SNDBUF) while remaining small enough that a 1k-connection
// burst tops out at 4 GiB of kernel send buffers.
const defaultSocketSendBuf = 4 << 20

// defaultIdleTimeout bounds how long a keep-alive-idle connection
// is allowed to sit without sending the next request's first byte.
// 60s matches Go's net/http server default and is well above the
// typical hf-cli range-request pacing.
const defaultIdleTimeout = 60 * time.Second

// defaultReadHeaderTimeout bounds the time between the first byte
// of a request arriving and the request line + headers being fully
// parsed.  5s is comfortably above the worst legitimate observed
// header serialization time (TLS LBs reassemble headers in <50µs
// internally) but tight enough to evict a slowloris dribbler before
// it can hold a connection slot for minutes.
const defaultReadHeaderTimeout = 5 * time.Second

// perIPSlot is a small helper that wraps the per-IP counter
// bookkeeping so the acceptLoop and the deferred release in
// serveConn share exactly one code path.  Returning a function
// instead of a method avoids leaking the netip.Addr through the
// connection struct.
type perIPSlot func()

// acquirePerIPSlot atomically reserves a per-IP slot under the
// MaxConnsPerIP cap.  Returns:
//
//   - (release, true)  if the connection is admitted; the caller
//     MUST invoke release exactly once when the connection is
//     closed.
//   - (nil, false)     if the cap is exceeded.  Caller must
//     close the connection without spawning a serving goroutine.
//   - (noop, true)     if MaxConnsPerIP == 0 (cap disabled) or
//     the remote address is not parseable; the noop release keeps
//     the deferred-release call uniform.
func (s *Server) acquirePerIPSlot(remote net.Addr) (perIPSlot, bool) {
	if s.MaxConnsPerIP <= 0 {
		return func() {}, true
	}
	tcp, ok := remote.(*net.TCPAddr)
	if !ok || tcp == nil {
		return func() {}, true
	}
	addr, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return func() {}, true
	}
	addr = addr.Unmap() // collapse ::ffff:1.2.3.4 to 1.2.3.4

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

// effectiveIdleTimeout returns the configured value or
// defaultIdleTimeout when zero.  Exposed as a method (not a
// field) so the io_uring reactor's detached fallback can read
// the same value when applying SetReadDeadline.
func (s *Server) effectiveIdleTimeout() time.Duration {
	if s.IdleTimeout > 0 {
		return s.IdleTimeout
	}
	return defaultIdleTimeout
}

// effectiveReadHeaderTimeout returns the configured value or
// defaultReadHeaderTimeout when zero.
func (s *Server) effectiveReadHeaderTimeout() time.Duration {
	if s.ReadHeaderTimeout > 0 {
		return s.ReadHeaderTimeout
	}
	return defaultReadHeaderTimeout
}

// Serve runs the accept loop until ln returns an error.  It returns the
// first non-temporary error from Accept, which is typically nil after a
// caller-initiated Close.
func (s *Server) Serve(ln net.Listener) error {
	return s.ServeMulti(ln)
}

// ServeMulti runs one accept loop per supplied listener concurrently.
// Used by callers that bind multiple SO_REUSEPORT listeners on the same
// port (one per CPU) to parallelise accept across cores.  Returns the
// FIRST non-nil error from any listener; the other accept loops are
// then expected to fall out via their own ln.Close.
//
// Single-listener callers can still use Serve; it is a thin wrapper
// around this.  io_uring setup is attempted once before the accept
// loops start:
//
//   - If reactor mode (Option B) can be initialized, every listener
//     becomes a reactor pinned to one io_uring ring; the Go netpoller
//     never sees these listeners.  Fallback for non-warm requests
//     hands the fd back to the std-lib net pipeline.
//
//   - If reactor mode fails (kernel < 6.1, syscall denied), we fall
//     back to the legacy acceptLoop + per-conn goroutine path; if the
//     hybrid header-via-io_uring pool can be initialized on the same
//     kernel, warm responses still get one syscall less than cork.
//
//   - Otherwise it's plain cork+sendfile, the production path that
//     ships on every Linux today.
func (s *Server) ServeMulti(lns ...net.Listener) error {
	if len(lns) == 0 {
		return errors.New("coreserver: ServeMulti requires >= 1 listener")
	}
	sndBuf := s.SocketSendBuf
	if sndBuf == 0 {
		sndBuf = defaultSocketSendBuf
	}

	// 1. Option B reactor (Linux >= 6.1 with single-issuer support).
	if s.IoUring {
		if rs, err := s.tryStartReactors(lns); err == nil && len(rs) > 0 {
			s.ioUringReady.Store(true)
			return s.runReactors(rs)
		}
	}

	// 2. Hybrid pool (header via io_uring, body via sendfile) — kept
	// alive as a graceful fallback when the reactor setup fails on
	// older or restricted kernels.
	if err := s.ioUringInit(); err == nil {
		s.ioUringReady.Store(true)
	}

	if len(lns) == 1 {
		return s.acceptLoop(lns[0], sndBuf)
	}

	// Multi-listener: one goroutine per listener.  First error wins
	// and is returned to the caller; the rest race their listeners
	// to closure.
	errCh := make(chan error, len(lns))
	for _, ln := range lns {
		ln := ln
		go func() { errCh <- s.acceptLoop(ln, sndBuf) }()
	}
	// Return the first error; do not block on the rest.  Caller is
	// expected to s.Close() + ln.Close() each listener to unwind.
	return <-errCh
}

func (s *Server) acceptLoop(ln net.Listener, sndBuf int) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if s.closed.Load() {
				return nil
			}
			return err
		}
		// Per-IP connection cap (slowloris / fd-exhaustion
		// defense).  Runs ONLY when MaxConnsPerIP > 0; the
		// helper short-circuits with a noop release otherwise
		// so the cap-disabled path is identical to the legacy
		// code (one branch + a function-call).
		release, ok := s.acquirePerIPSlot(c.RemoteAddr())
		if !ok {
			// Cap exceeded.  Close immediately without writing
			// any bytes -- an attacker that opens 10k sockets
			// from one host gets back nothing, not even a 503,
			// so we never spend CPU rendering responses to
			// abusers.  The accept itself already cost us a
			// syscall; that's the irreducible floor.
			_ = c.Close()
			telemetry.IncProxyPerIPCapDropped()
			continue
		}
		if sndBuf > 0 {
			if tc, ok := c.(*net.TCPConn); ok {
				// Best effort: ignore the error; kernel rounds /
				// clamps to its own min/max regardless.  The
				// observed effect is what matters and is exposed
				// via pulsys_sendfile_* counters.
				_ = tc.SetWriteBuffer(sndBuf)
			}
		}
		go func(c net.Conn, release perIPSlot) {
			defer release()
			s.serveConn(c)
		}(c, release)
	}
}

// Close marks the server as shutting down so the accept loop returns nil
// after the listener is closed externally.  When reactor mode is
// active, also signals every reactor to stop so the goroutines exit
// cleanly (otherwise tests / shutdown sequences leak ring fds).
//
// Shutdown ordering for io_uring mode:
//
//  1. Set s.closed so any new acceptLoop iterations exit.
//  2. Signal every reactor via reactor.close().  Each reactor
//     wakes out of its blocking ring.enter() via the eventfd
//     poke and returns nil from run().
//  3. Wait (bounded) for every reactor's run() goroutine to
//     publish on its done chan -- safe to close listeners and
//     unmap the ring memory only after this.
//  4. Wait (bounded) for every reactor's in-flight detached
//     fallback goroutine to return so no spawned goroutine
//     touches Server state after Close().
//
// We intentionally use bounded waits (closeReactorTimeout) so a
// stuck handler can't pin the parent process; a hang here would
// surface as a test cleanup that never completes (the very class
// of bug this routine exists to prevent).
func (s *Server) Close() {
	s.closed.Store(true)
	s.iouReactorsMu.Lock()
	reactors := s.iouReactors
	s.iouReactorsMu.Unlock()
	for _, r := range reactors {
		r.close()
	}
	for _, r := range reactors {
		_ = r.waitDone(closeReactorTimeout)
		_ = r.waitInFlight(closeReactorTimeout)
	}
}

// closeReactorTimeout bounds how long Close() waits for any
// single reactor's run() loop and its in-flight detached
// goroutines to drain.  10s is well above any realistic
// in-flight HF request that survived the listener close, and
// well below any reasonable test deadline.
const closeReactorTimeout = 10 * time.Second

// ----- per-request resources (pooled) --------------------------------------

const (
	// bufReadSize is the bufio.Reader buffer size — large enough for typical
	// request lines + header blocks (Hugging Face requests are well under
	// 8 KiB) so the parser never needs more than one underlying conn.Read
	// for the full header block in steady state.
	bufReadSize = 16 * 1024
	// bufWriteSize is the bufio.Writer buffer size; sized so a typical
	// response head fits in one flush.
	bufWriteSize = 16 * 1024
	// headerScratchSize bounds how many header bytes we will buffer before
	// declaring a request malformed.
	headerScratchSize = 16 * 1024
	// copyBufSize is the io.CopyBuffer scratch size.  64 KiB matches the
	// page-cache transfer granularity used by Go's splice/sendfile path.
	copyBufSize = 64 * 1024
)

var (
	bufReaderPool     = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, bufReadSize) }}
	bufWriterPool     = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, bufWriteSize) }}
	headerBufPool     = sync.Pool{New: func() any { b := make([]byte, headerScratchSize); return &b }}
	copyBufPool       = sync.Pool{New: func() any { b := make([]byte, copyBufSize); return &b }}
	sectionReaderPool = sync.Pool{New: func() any { return &fileSection{} }}
	// requestPool recycles *Request structs.  Without pooling, the local
	// `var req Request` in serveConn escapes to the heap because &req is
	// passed across function boundaries the compiler cannot inline through.
	requestPool = sync.Pool{New: func() any { return &Request{} }}
)

// fileSection is a resettable, poolable io.Reader over a fixed window of an
// io.ReaderAt (typically *cache.bodyHandle).  It exists so the warm-hit
// fast path does not allocate a fresh *io.SectionReader per request.
type fileSection struct {
	r   io.ReaderAt
	off int64
	end int64
}

func (s *fileSection) reset(r io.ReaderAt, off, n int64) {
	s.r = r
	s.off = off
	s.end = off + n
}

func (s *fileSection) Read(p []byte) (int, error) {
	if s.off >= s.end {
		return 0, io.EOF
	}
	if max := s.end - s.off; int64(len(p)) > max {
		p = p[:max]
	}
	n, err := s.r.ReadAt(p, s.off)
	s.off += int64(n)
	if err == nil && s.off >= s.end {
		err = io.EOF
	}
	return n, err
}

func acquireFileSection(r io.ReaderAt, off, n int64) *fileSection {
	s := sectionReaderPool.Get().(*fileSection)
	s.reset(r, off, n)
	return s
}

func releaseFileSection(s *fileSection) {
	s.r = nil
	sectionReaderPool.Put(s)
}

// ----- conn loop ----------------------------------------------------------

func (s *Server) serveConn(c net.Conn) {
	defer func() { _ = c.Close() }()

	br := bufReaderPool.Get().(*bufio.Reader)
	br.Reset(c)
	defer func() { br.Reset(nil); bufReaderPool.Put(br) }()

	bw := bufWriterPool.Get().(*bufio.Writer)
	bw.Reset(c)
	defer func() { bw.Reset(nil); bufWriterPool.Put(bw) }()

	hbufP := headerBufPool.Get().(*[]byte)
	defer headerBufPool.Put(hbufP)

	readDl := s.ReadTimeout
	if readDl == 0 && s.Cfg != nil {
		readDl = s.Cfg.ReadTimeout
	}
	writeDl := s.WriteTimeout
	if writeDl == 0 && s.Cfg != nil {
		writeDl = s.Cfg.WriteTimeout
	}
	idleDl := s.effectiveIdleTimeout()
	hdrDl := s.effectiveReadHeaderTimeout()

	req := requestPool.Get().(*Request)
	defer func() { *req = Request{}; requestPool.Put(req) }()

	// Cache the conn-side syscall.RawConn once per connection; this saves
	// one net.newRawConn allocation per warm request that fast-paths
	// through sendfile.  tcp may be nil for non-TCP listeners (e.g. the
	// in-memory pipe used by some tests), in which case sendfile falls
	// back to io.CopyBuffer.
	tcp, _ := c.(*net.TCPConn)
	var connRaw syscall.RawConn
	if tcp != nil {
		connRaw, _ = tcp.SyscallConn()
	}

	for {
		// Slowloris defense -- three-phase deadlines:
		//
		//   1. Idle phase (waiting for first byte): use IdleTimeout.
		//      A keep-alive connection sits here between requests; a
		//      malicious peer that never sends another byte is closed
		//      after idleDl.  br.Peek(1) blocks until the first byte
		//      arrives without consuming it; the subsequent
		//      readRequest call sees the same buffer.
		//
		//   2. Header phase (first byte arrived, parsing headers):
		//      use ReadHeaderTimeout (default 5s).  A peer that
		//      dribbles header bytes is closed before it can occupy
		//      a connection slot for the legacy 300s window.
		//
		//   3. Body phase (handler reads request body): use the
		//      generous ReadTimeout (default 300s) so legitimate
		//      multi-GiB LFS uploads still complete.
		//
		// Cost on the warm-hit GET hot path: +1 SetReadDeadline
		// syscall per request.  BenchmarkCoreServerWarm_256KiB +
		// _4MiB show no measurable degradation vs the single-
		// deadline baseline (see Phase 5 commit message).
		if idleDl > 0 {
			_ = c.SetReadDeadline(time.Now().Add(idleDl))
		}
		if _, peekErr := br.Peek(1); peekErr != nil {
			// Idle timeout, EOF, or read error.  Close cleanly.
			// No response: the typical reason here is a quiet
			// keep-alive socket the client never reused.
			return
		}
		if hdrDl > 0 {
			_ = c.SetReadDeadline(time.Now().Add(hdrDl))
		}
		*req = Request{} // reset fields between iterations
		err := readRequest(br, *hbufP, req)
		if err != nil {
			// Map parser errors to an HTTP response when we can
			// safely emit one, then unconditionally close the
			// connection.  We never reuse a connection that
			// produced a parse error: leftover bytes from a
			// smuggling attempt would be reinterpreted as a
			// follow-on request, which is exactly the desync we
			// just refused.
			writeParserErrorResponse(bw, err)
			_ = bw.Flush()
			return
		}
		// Headers complete.  Loosen the read deadline to
		// ReadTimeout for any body the handler is about to
		// stream.  Handlers that do not read a body just
		// over-pay by one syscall, which is negligible vs the
		// alternative of leaving the 5s header deadline armed
		// across a multi-GiB upload.
		if readDl > 0 {
			_ = c.SetReadDeadline(time.Now().Add(readDl))
		}
		if writeDl > 0 {
			// Arm the initial (absolute) write deadline.  For small/mid
			// responses this is the whole story.  For large streamed
			// bodies it is slid forward as the client makes progress so
			// it behaves as an IDLE deadline rather than an absolute cap
			// (cold/fallback: respWriter.maybeExtend; classic warm:
			// startWriteDeadlinePump; io_uring reactor: SO_SNDTIMEO).
			// A client that stalls for writeDl is still cut.
			_ = c.SetWriteDeadline(time.Now().Add(writeDl))
		}

		// Auth gate runs ahead of both the warm-hit lookup and the
		// fallback handler so a revoked / forged credential cannot
		// replay a cached body.  Nil gate = no enforcement (dev mode).
		//
		// CRITICAL: we ALWAYS close the connection after writing
		// the auth reject, even if the client requested keep-alive.
		// The reason is body framing: if the request had a body
		// (POST / PUT with Content-Length) we did not consume it
		// before deciding to reject -- the gate runs ahead of the
		// fallback handler that would normally read the body.
		// Reusing the socket would put the unread body bytes at
		// the start of the next request line, which the parser
		// (correctly) rejects as malformed, surfacing as a
		// confusing 400 to the second request.  Closing avoids
		// the body-leftover-as-next-request bug entirely.  The
		// TCP-handshake cost on retry is dominated by the
		// already-paid Authorization round-trip.
		if s.AuthGate != nil {
			if status, reason := s.AuthGate.Check(context.Background(), req.Auth, req.Path); status != 0 {
				writeAuthReject(bw, status, reason, false /* never keep-alive */)
				_ = bw.Flush()
				return
			}
		} else if s.RequireAuth {
			// Fail closed: RequireAuth without a gate is a
			// misconfiguration; never serve an unauthenticated plane.
			writeAuthReject(bw, 503, "data plane requires authentication but no gate is configured", false)
			_ = bw.Flush()
			return
		}

		served := s.tryServeWarm(c, connRaw, writeDl, bw, req)
		if !served {
			if s.Fallback == nil {
				writeStatus(bw, 503, "Service Unavailable")
				_ = bw.Flush()
				return
			}
			if err := s.Fallback(context.Background(), c, writeDl, bw, br, req); err != nil {
				return
			}
			if err := bw.Flush(); err != nil {
				return
			}
		}
		if !req.KeepAlive {
			return
		}
	}
}

// ----- HTTP/1.1 parser (zero-alloc steady state) --------------------------

var (
	errBadRequest     = errors.New("coreserver: bad request")
	errHeaderTooLarge = errors.New("coreserver: header block too large")
	// errSmugglingSuspect is returned for any input that exhibits a known
	// HTTP-smuggling fingerprint (TE+CL, duplicate CL, bare CR/NUL in
	// header values, etc.).  Distinct from errBadRequest so serveConn
	// can close the connection unconditionally without reuse — a
	// smuggling-shaped request often has body bytes left on the socket
	// that must not be reinterpreted as a follow-on request.
	errSmugglingSuspect = errors.New("coreserver: smuggling-suspect request rejected")
)

var (
	crlf       = []byte{'\r', '\n'}
	crlfcrlf   = []byte{'\r', '\n', '\r', '\n'}
	hostHdr    = []byte("host")
	rangeHdr   = []byte("range")
	authHdr    = []byte("authorization")
	connHdr    = []byte("connection")
	clenHdr    = []byte("content-length")
	teHdr      = []byte("transfer-encoding")
	closeBytes = []byte("close")
	identBytes = []byte("identity")
	httpSlash  = []byte("HTTP/")
)

// readRequest reads one HTTP/1.1 request from br, populating req with
// []byte slices that alias scratch.  scratch MUST live at least as long as
// req is used.  Only the headers we need are extracted; the rest are simply
// skipped after validation.
//
// SECURITY MODEL
// --------------
// This parser is the trust boundary for every byte that enters the proxy.
// It runs ahead of the auth gate, the cache lookup, and the fallback
// handler, so any classification disagreement between this code and a
// front-end reverse proxy creates a smuggling primitive.  We enforce a
// stricter-than-RFC subset by:
//
//   - Refusing both legacy and obfuscated framing controls (Transfer-
//     Encoding present, duplicate Content-Length, bare CR / NUL in
//     header values, obsolete line folding).
//   - Refusing request lines we cannot dispatch (forward-proxy
//     absolute-URI, authority-form CONNECT, anything other than
//     HTTP/1.0 or HTTP/1.1).
//   - Refusing tchar-violating tokens (uppercase methods only; header
//     names with whitespace or NUL bytes).
//   - Requiring exactly one Host header (RFC 7230 5.4).
//
// On any classification failure we return one of:
//   - errBadRequest     -> 400 Bad Request, connection MAY be reused
//   - errHeaderTooLarge -> 431 Request Header Fields Too Large
//   - errSmugglingSuspect -> 400 Bad Request, connection MUST be closed
//
// The caller (serveConn) handles the close-on-smuggling-suspect
// semantics so the leftover bytes from a desync attempt are never
// reinterpreted as a follow-on request.
//
// Allocation: zero per call after the bufio.Reader / scratch / pools warm
// up.  The parser never converts to string and never inserts into a map.
// The added validation costs ~10 ns of byte comparisons per warm request;
// see BenchmarkReadRequestWarm.
func readRequest(br *bufio.Reader, scratch []byte, req *Request) error {
	// Read header block into scratch by streaming until we observe
	// "\r\n\r\n".  bufio.Reader.ReadSlice('\n') returns slices that may be
	// invalidated by subsequent reads, so we copy into scratch as we go.
	n := 0
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return errHeaderTooLarge
			}
			return err
		}
		if n+len(line) > len(scratch) {
			return errHeaderTooLarge
		}
		copy(scratch[n:], line)
		n += len(line)
		if len(line) == 2 && line[0] == '\r' && line[1] == '\n' {
			break
		}
		// RFC 7230 3.5: SHOULD accept bare LF as terminator for
		// robustness.  Accepting bare LF only at the END of the
		// header block keeps that compatibility without making it
		// available inside a header value (which would be a desync
		// vector).
		if len(line) == 1 && line[0] == '\n' {
			break
		}
	}
	hdr := scratch[:n]
	req.Raw = hdr

	// Parse request line.
	end := bytes.Index(hdr, crlf)
	if end < 0 {
		return errBadRequest
	}
	if err := parseRequestLine(hdr[:end], req); err != nil {
		return err
	}
	hdr = hdr[end+2:]

	// Default keep-alive per HTTP/1.1.
	req.KeepAlive = req.HTTP11

	// Track framing-control headers so we can reject duplicate /
	// conflicting framing per RFC 7230 3.3.3.
	var (
		seenCL   bool
		seenTE   bool
		seenHost bool
	)

	for len(hdr) > 0 {
		end := bytes.Index(hdr, crlf)
		if end < 0 {
			return errBadRequest
		}
		if end == 0 { // terminator
			break
		}
		line := hdr[:end]
		hdr = hdr[end+2:]

		// Obsolete line folding (RFC 7230 3.2.4): a header
		// continuation begins with WSP.  We refuse it outright
		// because folding allows hiding control bytes from
		// header-name-only validators upstream.
		if line[0] == ' ' || line[0] == '\t' {
			return errSmugglingSuspect
		}

		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			// colon == 0 -> empty header name; colon < 0 -> no
			// colon at all.  Both violate RFC 7230 3.2.
			return errBadRequest
		}
		name := line[:colon]
		if !validHeaderName(name) {
			return errBadRequest
		}
		value := trimSpace(line[colon+1:])
		if !validHeaderValue(value) {
			return errSmugglingSuspect
		}
		switch {
		case asciiEqualFold(name, hostHdr):
			if seenHost {
				// RFC 7230 5.4: server MUST respond 400 to a
				// request with multiple Host headers.  Duplicate
				// Host enables host-based routing smuggling.
				return errSmugglingSuspect
			}
			seenHost = true
			req.Host = value
		case asciiEqualFold(name, rangeHdr):
			req.Range = value
		case asciiEqualFold(name, authHdr):
			req.Auth = value
		case asciiEqualFold(name, clenHdr):
			if seenCL {
				return errSmugglingSuspect
			}
			seenCL = true
			cl, err := parseDecimalInt64(value)
			if err != nil {
				return errSmugglingSuspect
			}
			req.ContentLen = cl
		case asciiEqualFold(name, teHdr):
			// Transfer-Encoding handling on the warm path is
			// strictly all-or-nothing: we either support a
			// well-defined identity transform or we refuse, because
			// the fast path does NOT consume request bodies and an
			// unconsumed chunked body is a CL.0 / TE.0 smuggling
			// primitive on the next pipelined request.
			seenTE = true
			if !asciiEqualFold(value, identBytes) {
				return errSmugglingSuspect
			}
		case asciiEqualFold(name, connHdr):
			if asciiContainsTokenFold(value, closeBytes) {
				req.KeepAlive = false
			} else if !req.HTTP11 {
				// For HTTP/1.0 only, "keep-alive" enables persistence.
				req.KeepAlive = true
			}
		}
	}

	// RFC 7230 3.3.3 #3: TE and CL together is undefined; we MUST
	// refuse.  The check happens after the loop so duplicate-CL
	// detection runs first and produces a more specific error
	// where applicable.
	if seenTE && seenCL {
		return errSmugglingSuspect
	}

	// RFC 7230 5.4: HTTP/1.1 messages MUST include a Host header.
	// We extend the rule to HTTP/1.0 because every cache-key
	// derivation downstream needs Host to be well-defined; refusing
	// here keeps the keyHex deterministic.
	if !seenHost {
		return errBadRequest
	}

	return nil
}

// parseRequestLine validates and parses the HTTP/1.1 request line.
// See readRequest's SECURITY MODEL doc-comment for the overall
// policy; this function enforces the request-line-specific rules:
//
//   - Method is a non-empty token of uppercase ASCII letters only.
//     Lowercase methods are a downstream-parser smuggling vector and
//     no legitimate HTTP/1.1 method uses lowercase.
//   - Request-target is the origin-form (path/query starting with '/').
//     Absolute-URI and authority-form are rejected because pulsys
//     is not a forward proxy and admitting them would let an attacker
//     pivot the routing decision.
//   - Protocol version is exactly "HTTP/1.0" or "HTTP/1.1".  Anything
//     else (HTTP/0.9, HTTP/1.x for x>1, HTTP/2.0, garbage) is refused.
func parseRequestLine(line []byte, req *Request) error {
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 <= 0 {
		return errBadRequest
	}
	method := line[:sp1]
	if !validMethod(method) {
		return errBadRequest
	}
	sp2 := bytes.IndexByte(line[sp1+1:], ' ')
	if sp2 <= 0 {
		// sp2 == 0 means two consecutive spaces, i.e. an empty
		// request-target between sp1 and the next space.
		return errBadRequest
	}
	sp2 += sp1 + 1
	target := line[sp1+1 : sp2]
	if !validOriginFormTarget(target) {
		return errBadRequest
	}
	proto := line[sp2+1:]
	if !validProtocolVersion(proto) {
		return errBadRequest
	}
	req.Method = method
	req.RequestURI = target
	req.HTTP11 = bytes.Equal(proto, []byte("HTTP/1.1"))
	q := bytes.IndexByte(target, '?')
	if q < 0 {
		req.Path = target
		req.Query = nil
	} else {
		req.Path = target[:q]
		req.Query = target[q+1:]
	}
	return nil
}

// validMethod reports whether b is a syntactically valid HTTP method
// token AND is uppercase ASCII.  RFC 7230 §3.1.1 grammar allows
// any tchar; we narrow to uppercase letters because every standard
// HTTP method (GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS, CONNECT,
// TRACE) and every Hugging Face Hub method matches.  Lowercase
// methods like "get" are accepted by Go's net/http but rejected by
// most production parsers; admitting them creates a parser-disagreement
// smuggling primitive.
func validMethod(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// validOriginFormTarget reports whether b is an origin-form
// request-target ("/path[?query]") consisting entirely of printable
// ASCII bytes (0x21-0x7E).  Absolute-URI ("http://host/path") and
// authority-form ("host:port") are refused because pulsys is not
// a forward proxy.  Bytes outside printable ASCII are refused
// because they create normalisation divergences between frontends:
//   - 0x00-0x1F, 0x7F (controls): header-injection / smuggling.
//   - 0x20 (SP): would re-frame the request line.
//   - 0x80-0xFF (high-bit, including raw UTF-8): RFC 3986 requires
//     URIs to be ASCII with non-ASCII percent-encoded.  Admitting
//     raw UTF-8 produces a cache-poisoning vector when one frontend
//     percent-encodes and another doesn't (PortSwigger 2018 cache
//     poisoning research; llhttp uri.md fixture #6).
func validOriginFormTarget(b []byte) bool {
	if len(b) == 0 || b[0] != '/' {
		return false
	}
	for _, c := range b {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// validProtocolVersion accepts exactly "HTTP/1.0" or "HTTP/1.1".
// We refuse "HTTP/0.9" because that protocol has no headers and is
// only meaningful to a server willing to interpret arbitrary bytes
// as a body; we refuse "HTTP/2.0" because admitting an h2 prelude
// over an h1 framer is the classic h2c smuggling primitive.
func validProtocolVersion(b []byte) bool {
	if len(b) != 8 {
		return false
	}
	if !bytes.HasPrefix(b, httpSlash) {
		return false
	}
	switch b[5] {
	case '1':
		// HTTP/1.0 or HTTP/1.1 only.
		return b[6] == '.' && (b[7] == '0' || b[7] == '1')
	}
	return false
}

// validHeaderName reports whether b is a token per RFC 7230 §3.2.6:
//
//	tchar = "!" / "#" / "$" / "%" / "&" / "'" / "*"
//	      / "+" / "-" / "." / "^" / "_" / "`" / "|" / "~"
//	      / DIGIT / ALPHA
//
// Empty names are rejected.
func validHeaderName(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if !isTchar(c) {
			return false
		}
	}
	return true
}

// isTchar reports whether c is a valid tchar per the grammar
// reproduced in validHeaderName's doc comment.
func isTchar(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*',
		'+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// validHeaderValue reports whether b contains only octets allowed
// by RFC 7230 §3.2.6 field-content:
//
//	field-content  = field-vchar [ 1*( SP / HTAB ) field-vchar ]
//	field-vchar    = VCHAR / obs-text
//	VCHAR          = %x21-7E
//	obs-text       = %x80-FF
//
// In short: SP (0x20), HTAB (0x09), 0x21-0x7E, and 0x80-0xFF are
// allowed; every other control byte is rejected.  NUL, bare CR, and
// bare LF are the canonical response-splitting and request-
// smuggling carriers and were already individually rejected; the
// broader sweep here catches less-famous controls (BEL, VT, FS, GS,
// RS, US, DEL) that lenient parsers admit and stricter validators
// (httpguts.ValidHeaderFieldValue, llhttp's strict mode, nginx's
// default) refuse.
func validHeaderValue(b []byte) bool {
	for _, c := range b {
		if c == ' ' || c == '\t' {
			continue
		}
		if c >= 0x21 && c <= 0x7e {
			continue
		}
		if c >= 0x80 {
			continue
		}
		return false
	}
	return true
}

// parseDecimalInt64 parses b as a non-negative base-10 integer with
// no leading sign and no whitespace.  Returns errBadRequest if the
// input has any character other than ['0'-'9'].  This is stricter
// than strconv.ParseInt (which accepts a leading '+' / '-') so a
// Content-Length of "+5" or " 5" is rejected -- both are stdlib-
// rejected and admitting them creates a parser-disagreement vector.
func parseDecimalInt64(b []byte) (int64, error) {
	if len(b) == 0 {
		return 0, errBadRequest
	}
	var v int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, errBadRequest
		}
		v = v*10 + int64(c-'0')
		if v < 0 { // overflow
			return 0, errBadRequest
		}
	}
	return v, nil
}

// ----- warm hit fast path -------------------------------------------------

// tryServeWarm returns true if the request was a warm cache hit and the
// response was fully written to the connection.
//
// The header block is buffered through bw (so it can coalesce into one
// syscall), then bw is flushed and the body is written directly to conn so
// Go's net package can dispatch to splice/sendfile when the source is a
// real *os.File.
func (s *Server) tryServeWarm(conn net.Conn, connRaw syscall.RawConn, writeDl time.Duration, bw *bufio.Writer, req *Request) bool {
	if !bytes.Equal(req.Method, []byte("GET")) {
		return false
	}
	host := defaultHost(s.Cfg)
	path := bytesToStringUnsafe(req.Path)
	if !classify.ArtifactGET(host, host, "GET", path) {
		return false
	}
	// Content-addressed hosts (Xet CAS, LFS CDN) re-issue presigned query
	// strings on every redirect — strip them before keying so the warm
	// hit lands on the same slot the cold write committed.
	keyQuery := bytesToStringUnsafe(req.Query)
	if cache.IsContentAddressedHost(host) {
		keyQuery = ""
	}
	// Authorization is validated by the AuthGate, but it is NEVER part of
	// the content cache key: the cached artifact is byte-identical no
	// matter which caller's token fetched it.  This MUST match
	// proxy.Handler's keyAuth="" convention (see internal/proxy/handler.go)
	// or every warm lookup silently misses the slot the cold write
	// committed and the whole warm fast-path is bypassed.
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
	defer func() { _ = release.Close() }()
	telemetry.IncCacheHit()

	n := wantEnd - wantStart

	// Large warm transfers can outlast the absolute WriteTimeout armed by
	// serveConn when the client drains slowly.  Engage a write-deadline
	// pump that slides the deadline forward as the sendfile loop makes
	// progress (idle-deadline semantics).  Gated on body size so the
	// small/mid hot path pays no goroutine/atomics; progress stays nil
	// there and the sendfile callbacks skip the atomic stores entirely.
	//
	// prog is declared INSIDE the gate so escape analysis only
	// heap-allocates the counter for large bodies — keeping the warm hot
	// path at its zero-extra-alloc floor.
	var progress *int64
	stopPump := func() {}
	if n > warmDeadlinePumpThreshold {
		var prog int64
		progress = &prog
		stopPump = startWriteDeadlinePump(conn, writeDl, &prog)
	}
	defer stopPump()

	// Header + body fusion path: when we have a usable raw TCP connection
	// AND a usable file descriptor, we render the response head into a
	// tiny pooled scratch buffer and hand it to sendFileWithHeaderViaRaw.
	// On Darwin this becomes a single sendfile(2) syscall via sf_hdtr,
	// halving the kernel boundary cost on small/medium responses (where
	// the header-write syscall otherwise costs roughly as much as the
	// body sendfile syscall).  On Linux the platform shim falls back to
	// writev(headers) + sendfile(body), which is no worse than today.
	var (
		written int64
		sfErr   error
		fused   bool
	)
	if connRaw != nil {
		if inFd, err := bh.Fd(); err == nil {
			hb := acquireHeaderBuf()
			if len(req.Range) == 0 {
				writeWarmHead200Buf(hb, n, meta, req.KeepAlive)
			} else {
				writeWarmHead206Buf(hb, wantStart, wantEnd, total, n, meta, req.KeepAlive)
			}
			// io_uring path first if ready; on errIoUringNotImplemented
			// (or any errIoUring*) we silently fall through to the
			// cork+sendfile path so the user-facing behavior is
			// identical until the io_uring impl lands and proves out.
			if s.ioUringReady.Load() {
				written, sfErr = sendFileWithHeaderViaIoUring(s, connRaw, inFd, wantStart, n, hb.bytes(), progress)
				if sfErr != nil {
					written, sfErr = sendFileWithHeaderViaRaw(connRaw, inFd, wantStart, n, hb.bytes(), s.TCPCork, progress)
				}
			} else {
				written, sfErr = sendFileWithHeaderViaRaw(connRaw, inFd, wantStart, n, hb.bytes(), s.TCPCork, progress)
			}
			releaseHeaderBuf(hb)
			fused = true
		}
	}

	// Slow path / fallback: render headers via the bufio.Writer (which
	// shares its scratch with the rest of the connection lifecycle) and
	// transfer the body via either sendfile (with raw fd) or io.CopyBuffer.
	if !fused {
		if len(req.Range) == 0 {
			writeWarmHead200(bw, n, meta, req.KeepAlive)
		} else {
			writeWarmHead206(bw, wantStart, wantEnd, total, n, meta, req.KeepAlive)
		}
		if err := bw.Flush(); err != nil {
			return true
		}
		if connRaw != nil {
			if inFd, err := bh.Fd(); err == nil {
				written, sfErr = sendFileViaRaw(connRaw, inFd, wantStart, n, progress)
			} else {
				sfErr = errNotTCP
			}
		} else {
			sfErr = errNotTCP
		}
	}
	if sfErr == errNotTCP || sfErr == errSendfileShortFallback {
		remaining := n - written
		if remaining > 0 {
			sec := acquireFileSection(bh, wantStart+written, remaining)
			defer releaseFileSection(sec)
			cbufP := copyBufPool.Get().(*[]byte)
			defer copyBufPool.Put(cbufP)
			var dst io.Writer = conn
			if progress != nil {
				dst = &progressWriter{w: conn, progress: progress, total: written}
			}
			extra, _ := io.CopyBuffer(dst, sec, *cbufP)
			written += extra
		}
	}
	telemetry.AddClientBytesServed(written)
	return true
}

// errSendfileShortFallback is sentinel-only; sendFileToTCP returns it when
// it stops mid-transfer for non-EAGAIN reasons that the io.CopyBuffer
// fallback can finish (e.g. unsupported file/socket combinations).
var errSendfileShortFallback = errors.New("coreserver: sendfile short-fallback")

// ----- response head writing ---------------------------------------------

// writeStatus writes a minimal HTTP/1.1 status-only response.
func writeStatus(bw *bufio.Writer, code int, reason string) {
	_, _ = bw.WriteString("HTTP/1.1 ")
	_, _ = bw.WriteString(strconv.Itoa(code))
	_ = bw.WriteByte(' ')
	_, _ = bw.WriteString(reason)
	_, _ = bw.WriteString("\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

// writeParserErrorResponse maps a readRequest sentinel error to a
// best-effort HTTP/1.1 status response.  Always sets Connection:
// close because the caller will tear down the socket immediately:
// reusing it would let leftover desync bytes become the next
// request line.
//
// Some errors (raw io.EOF on first read, broken pipes) cannot be
// reported back to the client at all because the connection is
// already half-closed; those are no-ops here.  We deliberately
// don't differentiate the response body text per error type: an
// attacker probing for parser fingerprinting gets a uniform
// surface.
func writeParserErrorResponse(bw *bufio.Writer, err error) {
	switch {
	case errors.Is(err, errHeaderTooLarge):
		telemetry.IncParserHeaderTooLarge()
		writeStatus(bw, 431, "Request Header Fields Too Large")
	case errors.Is(err, errSmugglingSuspect):
		// Split from errBadRequest so the smuggling-suspect
		// counter is independently observable.  An operator
		// alert "smuggling_suspect > N/min from one peer IP"
		// is the textbook desync-scanner signal; bundling it
		// under bad_request would lose that signal.  See
		// docs/security.md (monitoring signals).
		telemetry.IncParserSmugglingSuspect()
		writeStatus(bw, 400, "Bad Request")
	case errors.Is(err, errBadRequest):
		telemetry.IncParserBadRequest()
		writeStatus(bw, 400, "Bad Request")
	default:
		// Bufio short read / connection error / unknown.  Nothing
		// productive to say; just don't crash and let the conn
		// close cleanly.  Not counted: these are EOF-ish, not
		// parser disagreements.
	}
}

// incParserErrorCounter is the off-bw counterpart of
// writeParserErrorResponse: it advances the right parser-error
// expvar bucket without producing an HTTP response.  Used by the
// io_uring reactor's parse-error path, which closes the connection
// asynchronously and never writes a response (the reactor's recv
// buffer may be partially-filled when the error fires, so we cannot
// safely emit a status line).
//
// Keeping the taxonomy in one place ensures the std-lib and io_uring
// paths produce the same expvar surface; mixing them up would
// silently halve a deployment's smuggling-probe signal.
func incParserErrorCounter(err error) {
	switch {
	case errors.Is(err, errHeaderTooLarge):
		telemetry.IncParserHeaderTooLarge()
	case errors.Is(err, errSmugglingSuspect):
		telemetry.IncParserSmugglingSuspect()
	case errors.Is(err, errBadRequest):
		telemetry.IncParserBadRequest()
	}
}

// writeAuthReject writes a 401/403-style response for an AuthGate
// rejection through a *bufio.Writer.  Used by the std-lib serveConn
// path.  The reactor path uses renderAuthReject + unix.Write instead;
// both produce byte-identical responses, kept in sync by sharing the
// renderAuthReject formatter.
//
// Honors keep-alive so a misconfigured client doesn't pay a full TCP
// handshake on every retry; the connection is reusable for a
// subsequent request that may carry the correct credential.
func writeAuthReject(bw *bufio.Writer, status int, reason string, keepAlive bool) {
	_, _ = bw.Write(renderAuthRejectWithKeepAlive(status, reason, keepAlive))
}

// renderAuthReject formats a 401-style rejection response with
// Connection: close.  Used by the io_uring reactor path which writes
// the full response in one unix.Write and then closes the conn.
func renderAuthReject(status int, reason string) []byte {
	return renderAuthRejectWithKeepAlive(status, reason, false)
}

func renderAuthRejectWithKeepAlive(status int, reason string, keepAlive bool) []byte {
	conn := "close"
	if keepAlive {
		conn = "keep-alive"
	}
	// Pre-size: typical reject is ~150 B + reason.
	out := make([]byte, 0, 192+len(reason))
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(status), 10)
	out = append(out, ' ')
	out = append(out, statusText(status)...)
	out = append(out, "\r\n"...)
	out = append(out, "WWW-Authenticate: Bearer error=\"invalid_token\"\r\n"...)
	out = append(out, "Content-Type: text/plain; charset=utf-8\r\n"...)
	out = append(out, "Content-Length: "...)
	out = strconv.AppendInt(out, int64(len(reason)), 10)
	out = append(out, "\r\n"...)
	out = append(out, "Connection: "...)
	out = append(out, conn...)
	out = append(out, "\r\n\r\n"...)
	out = append(out, reason...)
	return out
}

// statusText maps the small set of status codes coreserver emits
// directly.  Avoids pulling net/http into the hot path just for
// http.StatusText.
func statusText(code int) string {
	switch code {
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 500:
		return "Internal Server Error"
	case 503:
		return "Service Unavailable"
	default:
		return ""
	}
}

func writeWarmHead200(bw *bufio.Writer, n int64, meta *cache.Meta, keepAlive bool) {
	_, _ = bw.WriteString("HTTP/1.1 200 OK\r\n")
	writeContentMeta(bw, meta)
	writeContentLength(bw, n)
	writeConnection(bw, keepAlive)
	_, _ = bw.WriteString("\r\n")
}

func writeWarmHead206(bw *bufio.Writer, start, end, total, n int64, meta *cache.Meta, keepAlive bool) {
	_, _ = bw.WriteString("HTTP/1.1 206 Partial Content\r\n")
	writeContentMeta(bw, meta)
	_, _ = bw.WriteString("Content-Range: bytes ")
	writeInt(bw, start)
	_ = bw.WriteByte('-')
	writeInt(bw, end-1)
	_ = bw.WriteByte('/')
	if total >= 0 {
		writeInt(bw, total)
	} else {
		_ = bw.WriteByte('*')
	}
	_, _ = bw.WriteString("\r\n")
	writeContentLength(bw, n)
	writeConnection(bw, keepAlive)
	_, _ = bw.WriteString("\r\n")
}

func writeContentMeta(bw *bufio.Writer, meta *cache.Meta) {
	if meta.ContentType != "" {
		_, _ = bw.WriteString("Content-Type: ")
		_, _ = bw.WriteString(meta.ContentType)
		_, _ = bw.WriteString("\r\n")
	}
	if meta.ETag != "" {
		_, _ = bw.WriteString("ETag: ")
		_, _ = bw.WriteString(meta.ETag)
		_, _ = bw.WriteString("\r\n")
	}
}

func writeContentLength(bw *bufio.Writer, n int64) {
	_, _ = bw.WriteString("Content-Length: ")
	writeInt(bw, n)
	_, _ = bw.WriteString("\r\n")
}

func writeConnection(bw *bufio.Writer, keepAlive bool) {
	if keepAlive {
		_, _ = bw.WriteString("Connection: keep-alive\r\n")
	} else {
		_, _ = bw.WriteString("Connection: close\r\n")
	}
}

// writeInt formats v in base 10 into bw without allocating.  The naïve
// strconv.AppendInt(buf[:0], …) form escapes buf to the heap because the
// resulting slice is passed across a method-call boundary that escape
// analysis cannot prove benign.  This recursive WriteByte-only version
// avoids the slice entirely (stack depth is bounded by the digit count).
func writeInt(bw *bufio.Writer, v int64) {
	if v < 0 {
		_ = bw.WriteByte('-')
		writeUint(bw, uint64(-v))
		return
	}
	writeUint(bw, uint64(v))
}

func writeUint(bw *bufio.Writer, v uint64) {
	if v >= 10 {
		writeUint(bw, v/10)
	}
	_ = bw.WriteByte(byte('0' + v%10))
}

// ----- helpers ------------------------------------------------------------

func defaultHost(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.DefaultHost
}

// trimSpace returns a sub-slice of b with leading/trailing ASCII
// spaces and tabs removed, without allocating.  CRITICAL: we do NOT
// trim trailing CR.  The header-block parser already splits on the
// "\r\n" terminator (see bytes.Index(hdr, crlf) in readRequest), so
// any CR remaining inside a line is bare and must surface to
// validHeaderValue for rejection.  Stripping it here would silently
// admit "Host:\r\r\n" -- a stdlib-rejected form the fuzz oracle
// caught in 15 seconds.
func trimSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	j := len(b)
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}

// asciiEqualFold reports whether a and bLower are equal ignoring ASCII case.
// bLower MUST be lower case.
func asciiEqualFold(a, bLower []byte) bool {
	if len(a) != len(bLower) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca := a[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if ca != bLower[i] {
			return false
		}
	}
	return true
}

// asciiContainsTokenFold reports whether comma- or whitespace-separated
// haystack contains the token tokenLower (which MUST be lower case).
func asciiContainsTokenFold(haystack, tokenLower []byte) bool {
	for len(haystack) > 0 {
		// skip separators
		for len(haystack) > 0 && (haystack[0] == ',' || haystack[0] == ' ' || haystack[0] == '\t') {
			haystack = haystack[1:]
		}
		end := 0
		for end < len(haystack) && haystack[end] != ',' && haystack[end] != ' ' && haystack[end] != '\t' {
			end++
		}
		if asciiEqualFold(haystack[:end], tokenLower) {
			return true
		}
		haystack = haystack[end:]
	}
	return false
}

func parseInt64(b []byte) (int64, error) { return strconv.ParseInt(bytesToStringUnsafe(b), 10, 64) }
