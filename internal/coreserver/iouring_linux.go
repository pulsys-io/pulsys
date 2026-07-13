// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux

package coreserver

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/pulsys-io/pulsys/internal/telemetry"
	"golang.org/x/sys/unix"
)

// errIoUringUnavailable is returned by ioUringInit on kernels too old
// or when ring setup fails.  Callers fall back to the cork+sendfile
// path silently.
var errIoUringUnavailable = errors.New("coreserver: io_uring requires Linux >= 6.1 with IORING_SETUP_DEFER_TASKRUN support")

// errIoUringNotImplemented is kept for tests that expect the sentinel.
var errIoUringNotImplemented = errors.New("coreserver: io_uring linked-SQE submission not yet implemented; falling back to cork+sendfile")

var (
	kernelChecked atomic.Bool
	kernelMin61   atomic.Bool
)

func ensureKernelChecked() {
	if kernelChecked.Load() {
		return
	}
	kernelMin61.Store(kernelGE(6, 1))
	kernelChecked.Store(true)
}

func kernelGE(major, minor int) bool {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return false
	}
	rel := nullTerminated(u.Release[:])
	parts := strings.SplitN(rel, ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	miStr := parts[1]
	for i, ch := range miStr {
		if ch < '0' || ch > '9' {
			miStr = miStr[:i]
			break
		}
	}
	mi, err := strconv.Atoi(miStr)
	if err != nil {
		return false
	}
	if ma > major {
		return true
	}
	return ma == major && mi >= minor
}

func nullTerminated(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

const (
	iouringSetupSingleIssuer = 1 << 12
	iouringSetupDeferTaskRun = 1 << 13
	iouringSetupCoopTaskRun  = 1 << 9

	sysIoUringSetup    uintptr = 425
	sysIoUringEnter    uintptr = 426
	sysIoUringRegister uintptr = 427
)

// ioUringInit allocates per-CPU io_uring rings when IoUring is requested.
func (s *Server) ioUringInit() error {
	if !s.IoUring {
		return errIoUringUnavailable
	}
	ensureKernelChecked()
	if !kernelMin61.Load() {
		return errIoUringUnavailable
	}
	flags := uint32(iouringSetupSingleIssuer | iouringSetupDeferTaskRun | iouringSetupCoopTaskRun)
	pool := &ioUringPool{}
	if err := pool.init(flags); err != nil {
		return err
	}
	s.iouPool = pool
	return nil
}

// tryStartReactors builds one io_uring reactor per listener (Option
// B).  Returns the reactor handles or an error; callers fall back to
// the legacy acceptLoop path on any error.  Kernel version is
// checked here so callers don't have to.
func (s *Server) tryStartReactors(lns []net.Listener) ([]*ioReactor, error) {
	if !s.IoUring {
		return nil, errIoUringUnavailable
	}
	ensureKernelChecked()
	if !kernelMin61.Load() {
		return nil, errIoUringUnavailable
	}
	return s.startReactors(lns)
}

// runReactors fires one goroutine per reactor and returns the first
// non-nil error any of them produces.  Mirrors the contract of the
// legacy multi-acceptLoop fan-out.  Publishes the reactor handles
// onto Server.iouReactors under iouReactorsMu so Server.Close()
// observes the published slice without racing the publish.
func (s *Server) runReactors(rs []*ioReactor) error {
	s.iouReactorsMu.Lock()
	s.iouReactors = rs
	s.iouReactorsMu.Unlock()
	errCh := make(chan error, len(rs))
	for _, r := range rs {
		r := r
		go func() { errCh <- r.run() }()
	}
	return <-errCh
}

func tcpConnFd(connRaw syscall.RawConn) (int, error) {
	var fd int
	err := connRaw.Control(func(f uintptr) {
		fd = int(f)
	})
	if err != nil {
		return 0, err
	}
	if fd < 0 {
		return 0, syscall.EBADF
	}
	return fd, nil
}

// sendFileWithHeaderViaIoUring ships the warm-hit response using io_uring
// for the HTTP head and the existing sendfile fast path for the body.
//
// v1 avoids IORING_OP_SPLICE to socket: on metal it drove ~1.2k RPS vs
// ~1.13M for sendfile alone.  One io_uring_enter for the header write still
// removes cork setsockopt and batches the header syscall with the ring's
// task-run deferral; the body uses sendFileViaRaw (zero-copy page cache).
func sendFileWithHeaderViaIoUring(s *Server, connRaw syscall.RawConn, inFd int, off, n int64, header []byte, progress *int64) (int64, error) {
	if s.iouPool == nil {
		return 0, errIoUringUnavailable
	}
	sockFd, err := tcpConnFd(connRaw)
	if err != nil {
		return 0, err
	}

	return s.iouPool.withRing(func(ring *ioUringRing) (int64, error) {
		telemetry.IncIoUringFusedCall()
		var total int64
		if len(header) > 0 {
			w, err := ring.sendHeaderOnly(sockFd, header)
			if err != nil {
				return 0, err
			}
			if w != len(header) {
				return int64(w), ioErrShortWrite
			}
			total += int64(w)
		}
		body, err := sendFileViaRaw(connRaw, inFd, off, n, progress)
		if err != nil {
			return total, err
		}
		total += body
		if total != int64(len(header))+n {
			return total, ioErrShortWrite
		}
		return total, nil
	})
}
