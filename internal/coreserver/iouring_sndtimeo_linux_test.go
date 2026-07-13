// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux

package coreserver

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestReactorSendfileStalledClientReturnsStalled drives reactorSendfile
// against a blocking socket whose peer never reads, with a short
// SO_SNDTIMEO set exactly as onAccept arms it.  Once the send buffer
// fills, sendfile(2) blocks until the timeout elapses and returns EAGAIN,
// which reactorSendfile must surface as errReactorClientStalled (so the
// reactor closes the connection instead of pinning its goroutine).
func TestReactorSendfileStalledClientReturnsStalled(t *testing.T) {
	if testing.Short() {
		t.Skip("slow timing test")
	}

	// Connected, BLOCKING socket pair — mirrors the reactor's accepted
	// sockets (IORING_OP_ACCEPT without SOCK_NONBLOCK).
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unsupported: %v", err)
	}
	sender, receiver := fds[0], fds[1]
	defer unix.Close(sender)
	defer unix.Close(receiver)

	// Shrink both buffers so a modest file is guaranteed to outrun them
	// and force sendfile to block.
	_ = unix.SetsockoptInt(sender, unix.SOL_SOCKET, unix.SO_SNDBUF, 64<<10)
	_ = unix.SetsockoptInt(receiver, unix.SOL_SOCKET, unix.SO_RCVBUF, 64<<10)

	const sndTimeout = 300 * time.Millisecond
	tv := unix.NsecToTimeval(sndTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(sender, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv); err != nil {
		t.Fatalf("set SO_SNDTIMEO: %v", err)
	}

	f, err := os.CreateTemp(t.TempDir(), "sendfile-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const fileSize = 8 << 20 // far larger than the 64 KiB buffers
	if err := f.Truncate(fileSize); err != nil {
		t.Fatal(err)
	}

	// Peer never reads from `receiver`, so the send blocks once buffers
	// fill and SO_SNDTIMEO trips.
	start := time.Now()
	n, sfErr := reactorSendfile(int32(sender), int32(f.Fd()), 0, fileSize)
	elapsed := time.Since(start)

	if sfErr != errReactorClientStalled {
		t.Fatalf("err = %v, want errReactorClientStalled", sfErr)
	}
	if n >= fileSize {
		t.Fatalf("transferred %d >= fileSize %d; expected a partial, stalled send", n, fileSize)
	}
	if elapsed < sndTimeout/2 {
		t.Fatalf("returned after %v, well before SO_SNDTIMEO %v; timeout not enforced", elapsed, sndTimeout)
	}
}
