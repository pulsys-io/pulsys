// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package coreserver

// SO_REUSEPORT listener helpers (Track A5 in
// docs/internals.md).
//
// SO_REUSEPORT lets multiple sockets bind to the same (host, port) pair;
// the kernel then load-balances accept() calls across them using a hash
// of the 4-tuple.  This eliminates the bottleneck of a single accept
// queue at high connection-rate.  Best paired with one accept goroutine
// per listener and (when io_uring is in play) one ring per listener.
//
// Cross-platform: SO_REUSEPORT is available on Linux 3.9+ and on most
// BSDs including macOS (since 10.5, but only with slightly different
// semantics around load-balancing fairness).  We feature-test by
// attempting to set the option and falling back if the kernel rejects.

import (
	"context"
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// NewReuseportListeners creates count TCP listeners bound to the same
// (network, addr) pair with SO_REUSEPORT enabled.  Returns the list of
// listeners and the first error encountered; on partial failure, the
// already-created listeners are closed before returning.
//
// count <= 1 returns exactly one ordinary net.Listen listener (no
// SO_REUSEPORT option set), so callers can pass cfg.Listeners directly
// without an explicit branch.
func NewReuseportListeners(network, addr string, count int) ([]net.Listener, error) {
	if count <= 1 {
		ln, err := net.Listen(network, addr)
		if err != nil {
			return nil, err
		}
		return []net.Listener{ln}, nil
	}
	lc := net.ListenConfig{Control: setReusePort}
	lns := make([]net.Listener, 0, count)
	for i := 0; i < count; i++ {
		ln, err := lc.Listen(context.Background(), network, addr)
		if err != nil {
			// Roll back so the caller does not leak fds.
			for _, prior := range lns {
				_ = prior.Close()
			}
			return nil, err
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// setReusePort sets SO_REUSEPORT on the listening socket before bind.
// Called via net.ListenConfig.Control, which fires exactly between
// socket() and bind().  Errors are returned to abort the listen.
func setReusePort(network, address string, c syscall.RawConn) error {
	_ = network
	_ = address
	var sockoptErr error
	ctlErr := c.Control(func(fd uintptr) {
		sockoptErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
	if ctlErr != nil {
		return ctlErr
	}
	if sockoptErr != nil {
		return errors.New("coreserver: SO_REUSEPORT not supported by this kernel: " + sockoptErr.Error())
	}
	return nil
}
