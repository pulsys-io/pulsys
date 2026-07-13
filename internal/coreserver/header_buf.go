// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package coreserver

import (
	"sync"

	"github.com/pulsys-io/pulsys/internal/cache"
)

// headerBuf is a fixed-size, allocation-free response-header scratch
// buffer used by the warm-hit fast path.
//
// On Darwin it lets us pass the response head as the iovec[0] argument
// of sendfile(2) (struct sf_hdtr) so the entire response (status line,
// headers, file body) leaves the process in a single syscall, instead
// of the usual two (write headers; sendfile body).
//
// 512 bytes is enough for pulsys's warm responses: the status line,
// Content-Type, ETag, Content-Length, Connection header, and an
// optional Content-Range together never exceed ~256 B in practice.
// We leave 2x slack so we never have to grow.
type headerBuf struct {
	b [512]byte
	n int
}

// reset clears the buffer for re-use without freeing the array.
func (h *headerBuf) reset() { h.n = 0 }

// bytes returns a slice aliasing the populated region of the buffer.
// The slice is only valid until the next reset()/write.
func (h *headerBuf) bytes() []byte { return h.b[:h.n] }

// writeString appends s.  Callers must keep the cumulative size below
// cap(headerBuf.b); the writeWarmHead*Buf helpers stay well under cap.
func (h *headerBuf) writeString(s string) {
	h.n += copy(h.b[h.n:], s)
}

// writeByte appends a single byte.
func (h *headerBuf) writeByte(c byte) {
	if h.n < len(h.b) {
		h.b[h.n] = c
		h.n++
	}
}

// writeInt formats v in base 10 (no allocation).  Mirrors the
// recursive bufio.Writer-based version in server.go to avoid the heap
// escape that strconv.AppendInt(buf[:0], ...) would induce.
func (h *headerBuf) writeInt(v int64) {
	if v < 0 {
		h.writeByte('-')
		h.writeUint(uint64(-v))
		return
	}
	h.writeUint(uint64(v))
}

func (h *headerBuf) writeUint(v uint64) {
	if v >= 10 {
		h.writeUint(v / 10)
	}
	h.writeByte(byte('0' + v%10))
}

var respHeaderBufPool = sync.Pool{
	New: func() any { return &headerBuf{} },
}

func acquireHeaderBuf() *headerBuf {
	hb := respHeaderBufPool.Get().(*headerBuf)
	hb.reset()
	return hb
}

func releaseHeaderBuf(hb *headerBuf) { respHeaderBufPool.Put(hb) }

// The following writeWarmHead*Buf functions mirror the bufio.Writer
// variants in server.go byte-for-byte so the wire format stays
// identical regardless of which serve path renders the response.

func writeWarmHead200Buf(h *headerBuf, n int64, meta *cache.Meta, keepAlive bool) {
	h.writeString("HTTP/1.1 200 OK\r\n")
	writeContentMetaBuf(h, meta)
	writeContentLengthBuf(h, n)
	writeConnectionBuf(h, keepAlive)
	h.writeString("\r\n")
}

func writeWarmHead206Buf(h *headerBuf, start, end, total, n int64, meta *cache.Meta, keepAlive bool) {
	h.writeString("HTTP/1.1 206 Partial Content\r\n")
	writeContentMetaBuf(h, meta)
	h.writeString("Content-Range: bytes ")
	h.writeInt(start)
	h.writeByte('-')
	h.writeInt(end - 1)
	h.writeByte('/')
	if total >= 0 {
		h.writeInt(total)
	} else {
		h.writeByte('*')
	}
	h.writeString("\r\n")
	writeContentLengthBuf(h, n)
	writeConnectionBuf(h, keepAlive)
	h.writeString("\r\n")
}

func writeContentMetaBuf(h *headerBuf, meta *cache.Meta) {
	if meta.ContentType != "" {
		h.writeString("Content-Type: ")
		h.writeString(meta.ContentType)
		h.writeString("\r\n")
	}
	if meta.ETag != "" {
		h.writeString("ETag: ")
		h.writeString(meta.ETag)
		h.writeString("\r\n")
	}
}

func writeContentLengthBuf(h *headerBuf, n int64) {
	h.writeString("Content-Length: ")
	h.writeInt(n)
	h.writeString("\r\n")
}

func writeConnectionBuf(h *headerBuf, keepAlive bool) {
	if keepAlive {
		h.writeString("Connection: keep-alive\r\n")
	} else {
		h.writeString("Connection: close\r\n")
	}
}
