// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package coreserver

import (
	"io"
	"net"
	"sync/atomic"
	"time"
)

// warmDeadlinePumpThreshold is the body size at or below which the warm
// sendfile path does NOT spawn a write-deadline pump goroutine.  Small
// and mid-tier warm hits complete well inside the absolute WriteTimeout
// even for a moderately slow client, so paying a goroutine + ticker per
// request on the hot path would be pure overhead.  Above the threshold
// the transfer can plausibly outlast WriteTimeout against a slow client,
// so we engage the pump to convert the absolute deadline into an idle
// one.  Matches the elephant sendfile slice tier (see sendfile_unix.go).
const warmDeadlinePumpThreshold = int64(32 << 20)

// startWriteDeadlinePump launches a watchdog goroutine that slides
// conn's write deadline forward to now+writeTimeout whenever the byte
// counter at progress advances.  This turns the absolute WriteTimeout
// armed by serveConn into an idle deadline for large warm transfers: as
// long as the client keeps draining bytes (sendfile makes progress) the
// transfer may run arbitrarily long, but a client that stalls for
// writeTimeout still gets cut by the last-armed deadline.
//
// The returned stop func MUST be called when the transfer finishes; it
// signals the goroutine and blocks until it has exited, guaranteeing no
// SetWriteDeadline call races a subsequent request on the same
// keep-alive connection.
//
// Returns a no-op stop when there is nothing to manage (no conn, no
// positive timeout, or no progress counter).
func startWriteDeadlinePump(conn net.Conn, writeTimeout time.Duration, progress *int64) func() {
	if conn == nil || writeTimeout <= 0 || progress == nil {
		return func() {}
	}
	interval := extensionInterval(writeTimeout)
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		last := atomic.LoadInt64(progress)
		for {
			select {
			case <-done:
				return
			case <-t.C:
				cur := atomic.LoadInt64(progress)
				if cur != last {
					last = cur
					_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// progressWriter wraps an io.Writer so the io.CopyBuffer fallback in
// tryServeWarm reports forward progress to the write-deadline pump.  It
// is only used on the rare non-sendfile fallback for large bodies, so it
// trades the writer's ReaderFrom fast path (already unavailable here) for
// pump visibility.  base seeds the counter with bytes already shipped by
// sendfile before the short-fall.
type progressWriter struct {
	w        io.Writer
	progress *int64
	total    int64
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	if n > 0 {
		pw.total += int64(n)
		atomic.StoreInt64(pw.progress, pw.total)
	}
	return n, err
}
