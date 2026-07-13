// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// io_uring per-IP cap regression (Phase 5 / CVE-monitoring).
// Linux only.
//
// PURPOSE
//
// The std-lib accept loop already enforces Server.MaxConnsPerIP
// (see slowloris_test.go::TestSlowloris_PerIPCap_RejectsExcess).
// Prior to this commit the io_uring reactor's onAccept path
// completely bypassed that check, leaving a Linux deployment
// with -iouring open to single-host fd exhaustion even when
// -max-conns-per-ip was set -- a documented asymmetry in
// docs/security.md that the CVE audit flagged
// as P1.
//
// This test pins the new contract: the reactor MUST honor
// MaxConnsPerIP identically to the std-lib path.  When
// PULSYS_TEST_IOURING is not set it is a no-op skip so the
// existing Darwin/dev workflow stays green; the compose
// security-tests-linux harness sets it.

//go:build linux

package sectest

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	hffixtures "github.com/pulsys-io/pulsys/internal/hfhub/fixtures"
	"github.com/pulsys-io/pulsys/internal/hfhub/mockhub"
	"github.com/pulsys-io/pulsys/internal/telemetry"
	"github.com/pulsys-io/pulsys/internal/testserver"
)

// TestIoUring_PerIPCap_RejectsExcess asserts that when the
// reactor is forced on (PULSYS_TEST_IOURING) and MaxConnsPerIP
// is set, the (cap+1)th connection from one peer is dropped at
// accept time and the drop counter advances -- byte-identical
// to the std-lib path's TestSlowloris_PerIPCap_RejectsExcess
// contract.
func TestIoUring_PerIPCap_RejectsExcess(t *testing.T) {
	if !envSet("PULSYS_TEST_IOURING") {
		t.Skip("PULSYS_TEST_IOURING not set; io_uring per-IP cap test is compose-only")
	}
	t.Parallel()

	const cap = 3
	stack := testserver.New(t, testserver.Config{
		Mock: mockhub.Config{
			Repos: []mockhub.RepoSpec{{
				Name:         "acme/widget",
				InitialFiles: hffixtures.TinyModelFiles("acme/widget"),
			}},
		},
		IdleTimeout:       5 * time.Second,
		ReadHeaderTimeout: time.Second,
		MaxConnsPerIP:     cap,
	})
	addr := stripAddr(stack.ProxyURL())

	before := telemetry.ProxyPerIPCapDroppedSnapshot()

	// Hold `cap` connections idle.
	held := make([]net.Conn, 0, cap)
	for i := 0; i < cap; i++ {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("hold dial %d: %v", i, err)
		}
		held = append(held, c)
		t.Cleanup(func() { _ = c.Close() })
	}

	// Reactor accept loop is async on Linux; give it a tick
	// to register all cap connections in the per-IP map
	// before the (cap+1)th dial races it.  Without this the
	// last hold dial may not yet be counted and the overflow
	// dial wrongly succeeds.
	time.Sleep(100 * time.Millisecond)

	// (cap+1)th dial: TCP handshake completes, then the
	// reactor's per-IP gate fires inside onAccept and
	// closes the fd synchronously via unix.Close -- no
	// HTTP bytes ever traverse the connection.
	overflow, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("overflow dial: %v", err)
	}
	t.Cleanup(func() { _ = overflow.Close() })

	_ = overflow.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, readErr := overflow.Read(buf)
	if readErr == nil || n != 0 {
		t.Fatalf("expected EOF / RST on iouring per-IP cap reject; got n=%d err=%v body=%q",
			n, readErr, buf[:n])
	}
	if ne, ok := readErr.(net.Error); ok && ne.Timeout() {
		t.Fatalf("server did not close the over-cap io_uring conn within 2s")
	}
	if !errors.Is(readErr, io.EOF) && !isConnReset(readErr) {
		t.Fatalf("expected io.EOF or RST on iouring over-cap conn; got %v", readErr)
	}

	after := telemetry.ProxyPerIPCapDroppedSnapshot()
	if after-before < 1 {
		t.Fatalf("io_uring reactor did not advance per-IP cap drop counter: before=%d after=%d",
			before, after)
	}
}
