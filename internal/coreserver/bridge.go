// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Bridge: turn an http.Handler into a coreserver FallbackFunc.
//
// In production, internal/coreserver is the primary listener.  Warm
// cache-hit GETs are served by the zero-allocation fast path
// (tryServeWarm + sendfile + sf_hdtr fusion on Darwin).  Anything else
// -- cache misses, non-GET methods, multi-host /_p/ routes, metadata
// JSON, redirects, errors -- falls through to the standard
// http.Handler that runs the rest of the proxy semantics.
//
// HandlerFallback is the small adapter that connects the two worlds:
//
//   - It re-emits the original request bytes via http.ReadRequest, so
//     the slow-path handler sees a fully-fledged *http.Request with
//     ALL headers (coreserver's parser only extracts the few it cares
//     about for fast-path key derivation).
//
//   - It wraps the connection's *bufio.Writer in a minimal but
//     correct net/http.ResponseWriter implementation that supports
//     keep-alive, Content-Length-framed bodies, and chunked transfer
//     encoding.
//
// The conn-level read/write deadlines, request pooling, and the
// underlying TCP socket are all owned by coreserver.serveConn -- the
// bridge just hands off control for one request and returns.
package coreserver

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// HandlerFallback returns a FallbackFunc that dispatches the slow path
// to the supplied http.Handler.
func HandlerFallback(h http.Handler) FallbackFunc {
	return func(ctx context.Context, conn net.Conn, writeTimeout time.Duration, bw *bufio.Writer, br *bufio.Reader, req *Request) error {
		// Re-emit headers + remaining body bytes so net/http can parse
		// the request as if it had read it directly off the wire.
		mr := io.MultiReader(newPrefixReader(req.Raw), br)
		hr := bufio.NewReader(mr)
		r, err := http.ReadRequest(hr)
		if err != nil {
			writeStatus(bw, http.StatusBadRequest, "Bad Request")
			_ = bw.Flush()
			return err
		}
		r = r.WithContext(ctx)
		// http.ReadRequest leaves the body on a *body type that closes
		// the underlying connection on Close; we don't want that here
		// (the conn is owned by coreserver).  Wrap to no-op Close.
		if r.Body != nil {
			r.Body = nopCloser{r.Body}
		}

		rw := acquireRespWriter(bw, req, conn, writeTimeout)
		defer releaseRespWriter(rw)
		h.ServeHTTP(rw, r)
		if err := rw.finalize(); err != nil {
			return err
		}
		return nil
	}
}

// newPrefixReader returns an io.Reader that yields b's bytes once and
// then io.EOF.  Unlike bytes.NewReader the returned reader is a tiny
// pointer-backed value -- escape analysis still puts it on the heap as
// it crosses the io.Reader interface boundary, but the cost is one
// 24-byte allocation per fallback request, paid only on the slow
// (cache-miss / non-GET) path.
func newPrefixReader(b []byte) io.Reader { return &prefixReader{b: b} }

type prefixReader struct{ b []byte }

func (p *prefixReader) Read(buf []byte) (int, error) {
	if len(p.b) == 0 {
		return 0, io.EOF
	}
	n := copy(buf, p.b)
	p.b = p.b[n:]
	return n, nil
}

// nopCloser wraps io.Reader as io.ReadCloser whose Close is a no-op.
// We use this for r.Body so handler-side Close calls don't propagate
// into closing the underlying connection (that's coreserver's job
// after the fallback returns).
type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

// ----- Response writer ---------------------------------------------------

// rwMode tracks the framing the writer settled on after WriteHeader.
type rwMode uint8

const (
	rwModeUnknown rwMode = iota
	rwModeContentLength
	rwModeChunked
)

// respWriter is a minimal net/http.ResponseWriter implementation over a
// *bufio.Writer.  It is correct for all pulsys slow-path responses:
//
//   - When Content-Length is set explicitly by the handler, body bytes
//     are written through to bw verbatim.
//
//   - When Content-Length is absent and the handler does not call
//     WriteHeader before Write, the writer encodes the body using
//     chunked transfer encoding (HTTP/1.1) so cache-miss artifact
//     downloads with chunked upstream framing pass through correctly.
//
//   - Connection: close vs keep-alive is forced based on the parsed
//     coreserver.Request flag, overriding any handler-supplied value
//     (the conn lifecycle is owned by coreserver.serveConn).
type respWriter struct {
	bw      *bufio.Writer
	hdr     http.Header
	mode    rwMode
	wroteHd bool
	keep    bool
	status  int
	cl      int64 // -1 if unknown, else expected content length

	// Sliding write-deadline state.  conn is the underlying
	// connection bw writes through (nil when the caller has no
	// addressable conn, e.g. tests); writeTimeout is the per-write
	// idle budget; lastExt throttles SetWriteDeadline syscalls so a
	// multi-GiB streamed body does not pay one per Write.  See
	// maybeExtend.
	conn         net.Conn
	writeTimeout time.Duration
	extEvery     time.Duration
	lastExt      time.Time
}

func acquireRespWriter(bw *bufio.Writer, req *Request, conn net.Conn, writeTimeout time.Duration) *respWriter {
	rw := &respWriter{
		bw:           bw,
		hdr:          make(http.Header, 8),
		keep:         req.KeepAlive,
		status:       200,
		cl:           -1,
		conn:         conn,
		writeTimeout: writeTimeout,
	}
	rw.extEvery = extensionInterval(writeTimeout)
	return rw
}

func releaseRespWriter(rw *respWriter) {
	rw.bw = nil
	rw.hdr = nil
	rw.conn = nil
}

// extensionInterval picks how often maybeExtend re-arms the write
// deadline.  It must be strictly less than writeTimeout (otherwise the
// deadline would fire before the next extension), and large enough that
// streaming a multi-GiB body costs only a handful of SetWriteDeadline
// syscalls.  We aim for ~four extensions per window, capped at 60s so a
// long production WriteTimeout (300s) still re-arms a few times.
func extensionInterval(writeTimeout time.Duration) time.Duration {
	if writeTimeout <= 0 {
		return 0
	}
	iv := writeTimeout / 4
	if iv > 60*time.Second {
		iv = 60 * time.Second
	}
	if iv < 50*time.Millisecond {
		iv = 50 * time.Millisecond
	}
	return iv
}

// maybeExtend slides the connection's write deadline forward to
// now+writeTimeout, throttled to at most once per extEvery.  This turns
// the absolute WriteTimeout armed by the caller into an idle deadline:
// as long as the client keeps draining bytes (each Write makes
// progress), the transfer is allowed to run arbitrarily long; a client
// that stalls for writeTimeout still gets cut.  No-ops when there is no
// conn or no positive timeout.
func (rw *respWriter) maybeExtend() {
	if rw.conn == nil || rw.writeTimeout <= 0 {
		return
	}
	now := time.Now()
	if !rw.lastExt.IsZero() && now.Sub(rw.lastExt) < rw.extEvery {
		return
	}
	rw.lastExt = now
	_ = rw.conn.SetWriteDeadline(now.Add(rw.writeTimeout))
}

// Header satisfies http.ResponseWriter.
func (rw *respWriter) Header() http.Header { return rw.hdr }

// WriteHeader writes the status line and header block to bw.
func (rw *respWriter) WriteHeader(code int) {
	if rw.wroteHd {
		return
	}
	rw.wroteHd = true
	rw.status = code

	// Decide framing.
	if cl := rw.hdr.Get("Content-Length"); cl != "" {
		if v, err := strconv.ParseInt(cl, 10, 64); err == nil && v >= 0 {
			rw.cl = v
			rw.mode = rwModeContentLength
		}
	}
	if rw.mode == rwModeUnknown {
		rw.mode = rwModeChunked
		rw.hdr.Del("Content-Length")
		rw.hdr.Set("Transfer-Encoding", "chunked")
	}

	// Force the connection header to match coreserver's view.
	rw.hdr.Del("Connection")
	if rw.keep {
		rw.hdr.Set("Connection", "keep-alive")
	} else {
		rw.hdr.Set("Connection", "close")
	}

	bw := rw.bw
	_, _ = bw.WriteString("HTTP/1.1 ")
	_, _ = bw.WriteString(strconv.Itoa(code))
	_ = bw.WriteByte(' ')
	_, _ = bw.WriteString(http.StatusText(code))
	_, _ = bw.WriteString("\r\n")
	for k, vv := range rw.hdr {
		for _, v := range vv {
			_, _ = bw.WriteString(k)
			_, _ = bw.WriteString(": ")
			_, _ = bw.WriteString(v)
			_, _ = bw.WriteString("\r\n")
		}
	}
	_, _ = bw.WriteString("\r\n")
}

// Write writes body bytes through bw.  Implicitly calls WriteHeader(200)
// on first invocation, matching net/http semantics.
func (rw *respWriter) Write(p []byte) (int, error) {
	if !rw.wroteHd {
		rw.WriteHeader(http.StatusOK)
	}
	rw.maybeExtend()
	switch rw.mode {
	case rwModeContentLength:
		return rw.bw.Write(p)
	case rwModeChunked:
		if len(p) == 0 {
			return 0, nil
		}
		// chunk-size = HEX-DIGIT*  CRLF chunk-data CRLF
		_, _ = rw.bw.WriteString(strconv.FormatInt(int64(len(p)), 16))
		_, _ = rw.bw.WriteString("\r\n")
		n, err := rw.bw.Write(p)
		if err != nil {
			return n, err
		}
		if _, err := rw.bw.WriteString("\r\n"); err != nil {
			return n, err
		}
		return n, nil
	default:
		return 0, errors.New("coreserver: write before WriteHeader on unknown mode")
	}
}

// ReadFrom enables io.Copy(rw, src) to bypass the per-Write chunk
// framing for known-length sources, matching net/http's
// (*response).ReadFrom optimization.  When the writer is in
// content-length mode the source bytes can flow straight to bw
// (which itself is sitting on a *net.TCPConn, so Go's runtime can
// dispatch sendfile when the source is *os.File or *io.SectionReader
// over one).  When chunked, we fall back to the per-Write chunked
// framing path -- there's no general way to chunk-encode an arbitrary
// io.Reader without buffering, so we read in 64 KiB blocks and emit
// one chunk per block.
func (rw *respWriter) ReadFrom(src io.Reader) (int64, error) {
	if !rw.wroteHd {
		rw.WriteHeader(http.StatusOK)
	}
	if rw.mode == rwModeContentLength {
		return io.Copy(structWriter{rw: rw}, src)
	}
	// Chunked: copy in 64 KiB blocks via Write so chunks are emitted
	// correctly.  This is only used for the cache-miss-with-chunked
	// upstream path, which is rare on HF (most artifact responses
	// carry an explicit Content-Length).
	bp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bp)
	buf := *bp
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := rw.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// structWriter strips io.ReaderFrom from a *bufio.Writer so io.Copy
// uses Read+Write rather than recursing into our ReadFrom.  Without
// this wrapper io.Copy(rw.bw, src) would prefer bufio.Writer.ReadFrom
// (which already does the right thing) but io.Copy(rw, src) plus
// rw.ReadFrom delegating to rw.bw.ReadFrom risks an infinite-recursion
// confusion if anyone wraps rw in another ReaderFrom.  Keeping it
// explicit here documents intent.  It also slides the write deadline
// forward (rw.maybeExtend) on every block so a large content-length
// body streamed via io.Copy gets the same idle-deadline treatment as
// the per-Write path.
type structWriter struct {
	rw *respWriter
}

func (s structWriter) Write(p []byte) (int, error) {
	s.rw.maybeExtend()
	return s.rw.bw.Write(p)
}

// finalize completes the response: writes the trailing chunked
// terminator (if applicable), defaults any never-Wrote responses to
// 200 OK with empty body, and flushes the bufio.Writer.
func (rw *respWriter) finalize() error {
	if !rw.wroteHd {
		// Handler returned without writing anything; emit 200 with
		// zero-length body and the framing we choose.
		rw.hdr.Set("Content-Length", "0")
		rw.WriteHeader(http.StatusOK)
		return rw.bw.Flush()
	}
	if rw.mode == rwModeChunked {
		// Last-chunk + trailers (none) + final CRLF.
		if _, err := rw.bw.WriteString("0\r\n\r\n"); err != nil {
			return err
		}
	}
	return rw.bw.Flush()
}

// Hijack is intentionally unimplemented -- the slow-path handler
// should never need to hijack the connection from us.  If it ever did
// (e.g. websockets, which pulsys doesn't speak), we'd need to
// surrender bw + the underlying conn here.
