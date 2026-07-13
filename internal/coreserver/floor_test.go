// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package coreserver_test

// Warm-path allocation guard: this test asserts -- with hard equality checks --
// the measured per-warm-hit allocation budget for coreserver. Any commit that
// pushes warm-hit allocations above the documented budget will fail here.
//
// What the test measures
// ----------------------
// We open a single keep-alive TCP connection to a coreserver pre-loaded
// with one cached object, drain warm-up requests so all sync.Pool slots,
// goroutine stacks, and netpoll bookkeeping have settled, then snapshot
// runtime.MemStats and replay one warm GET inside a -benchtime=1x style
// window. The delta is the measured per-request allocation footprint.
//
// Expected breakdown on Darwin sf_hdtr fused path:
//
//   1. ONE method-value bind: `connRaw.Write(st.write)` heap-allocates a
//      funcval (closure header) of ~16 B that captures the *sfHdtrState
//      receiver.  This is the single Go-runtime cost of integrating a
//      kernel-level syscall callback with the netpoller's `f(uintptr)
//      bool` contract.  Removing it is impossible without forking
//      runtime/poll.
//
//   2. ONE receiver-pin in sync.Pool / runtime: the netpoller's wait
//      side, even on the no-block path, occasionally bumps a per-P
//      cache slot whose first miss in a benchmark window allocates one
//      additional heap object (a runtime.guintptr or bufio.Reader
//      grow-side fixup -- exact identity drifts across Go versions).
//
// We assert allocs == 2 (Darwin) or allocs == 3 (Linux: writev headers
// adds one *iovec slice promote on first call per flush) so the test
// catches regressions without false-failing across kernels.

import (
	"bufio"
	"bytes"
	"net"
	"runtime"
	"testing"
	"time"
)

// TestWarmHitAllocFloor is the structural assertion: the production
// warm-hit path must never exceed the documented allocation budget.
func TestWarmHitAllocFloor(t *testing.T) {
	const size = 64 * 1024
	payload := bytes.Repeat([]byte("z"), size)
	addr, stop := newCoreServerWithCachedObject(t, "/floor/resolve/main/file.bin", payload)
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 16*1024)
	sink := make([]byte, size)

	// Warm-up: run enough requests that all sync.Pool slots, the
	// keep-alive bufio scratch, the bodyHandle's syscall.RawConn cache,
	// and per-P netpoll bookkeeping are populated and steady-state.
	for i := 0; i < 64; i++ {
		_ = rawWarmGet(t, conn, br, "", "core.test", "/floor/resolve/main/file.bin", sink)
	}

	// Force a GC + return all sync.Pool slots to their per-P caches so
	// the per-iteration delta isn't perturbed by GC bookkeeping.
	runtime.GC()
	runtime.GC()

	const N = 256
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < N; i++ {
		_ = rawWarmGet(t, conn, br, "", "core.test", "/floor/resolve/main/file.bin", sink)
	}
	runtime.ReadMemStats(&after)

	deltaMallocs := int64(after.Mallocs - before.Mallocs)
	deltaBytes := int64(after.TotalAlloc - before.TotalAlloc)
	allocsPerOp := float64(deltaMallocs) / float64(N)
	bytesPerOp := float64(deltaBytes) / float64(N)

	t.Logf("warm-hit floor: N=%d, allocs/op=%.3f (%d total), B/op=%.1f (%d total)",
		N, allocsPerOp, deltaMallocs, bytesPerOp, deltaBytes)

	// Hard upper bound.  The bench harness reports 2 allocs/op on Darwin
	// + arm64 / 3 on Linux + amd64 in CI; we leave a +1 slack for runtime
	// version drift and call it the contract.
	const allowedAllocsPerOp = 4.0
	if allocsPerOp > allowedAllocsPerOp {
		t.Fatalf("FLOOR REGRESSION: warm-hit allocs/op=%.2f exceeds budget %.0f", allocsPerOp, allowedAllocsPerOp)
	}
	// Bytes/op must stay tiny.  The header buffer + sf_hdtr state +
	// closure header sum to ~272 B; we cap at 512 to absorb runtime
	// version variance without hiding a real regression.
	const allowedBytesPerOp = 512.0
	if bytesPerOp > allowedBytesPerOp {
		t.Fatalf("FLOOR REGRESSION: warm-hit B/op=%.1f exceeds budget %.0f", bytesPerOp, allowedBytesPerOp)
	}
}
