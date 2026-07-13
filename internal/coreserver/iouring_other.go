// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build !linux

package coreserver

import (
	"errors"
	"net"
	"syscall"
	"time"
)

// ioUringPool is a stub on non-Linux so Server compiles everywhere.
type ioUringPool struct{}

// ioReactor is a stub on non-Linux so Server compiles everywhere.
type ioReactor struct{}

// close is a no-op stub so cross-platform Server.Close compiles.
func (*ioReactor) close() {}

// waitDone is a no-op stub so cross-platform Server.Close compiles.
// On non-Linux the reactor is never instantiated so this is
// unreachable; we return true to mirror the "clean shutdown"
// signal the Linux implementation emits.
func (*ioReactor) waitDone(_ time.Duration) bool { return true }

// waitInFlight is a no-op stub so cross-platform Server.Close
// compiles.  Same rationale as waitDone above.
func (*ioReactor) waitInFlight(_ time.Duration) bool { return true }

// errIoUringUnavailable is the cross-platform sentinel that signals
// "io_uring is not usable; fall back to the per-OS sendfile path".
// On non-Linux platforms it is always returned from ioUringInit.
var errIoUringUnavailable = errors.New("coreserver: io_uring is Linux-only")
var errIoUringNotImplemented = errIoUringUnavailable

// ioUringInit is a no-op stub on non-Linux platforms.
func (s *Server) ioUringInit() error { return errIoUringUnavailable }

// tryStartReactors is a no-op stub on non-Linux platforms.
func (s *Server) tryStartReactors(_ []net.Listener) ([]*ioReactor, error) {
	return nil, errIoUringUnavailable
}

// runReactors is unreachable on non-Linux (tryStartReactors always
// errors), but kept here so ServeMulti compiles cross-platform.
func (s *Server) runReactors(_ []*ioReactor) error { return errIoUringUnavailable }

// sendFileWithHeaderViaIoUring is a no-op stub on non-Linux platforms.
// Callers receive errIoUringNotImplemented and fall back to the platform
// sendFileWithHeaderViaRaw (sf_hdtr on Darwin; cork+sendfile on Linux).
func sendFileWithHeaderViaIoUring(_ *Server, _ syscall.RawConn, _ int, _, _ int64, _ []byte, _ *int64) (int64, error) {
	return 0, errIoUringNotImplemented
}
