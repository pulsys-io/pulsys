// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// io_uring exercise pin (Phase 4.5 OWASP smoke).  Linux only.
//
// PURPOSE
//   The Phase 0-4 security tests verify protocol-level
//   invariants (smuggling, traversal, SSRF, etc) against the
//   coreserver via its public network interface.  On Darwin,
//   that public interface dispatches through:
//
//     server.go: serveConn -> ServeHTTP
//
//   On Linux, when io_uring + DEFER_TASKRUN are available, the
//   SAME public network interface dispatches through:
//
//     iouring_reactor_linux.go: reactor.run() ->
//     iouring_parser_linux.go: readRequestIoUring() ->
//     ServeHTTP
//
//   These are TWO different parsers and TWO different connection
//   pipelines.  A bug in the iouring side would NOT be caught by
//   Darwin tests.  Phase 4.5 (this file + the
//   security-tests-linux compose harness) closes that gap by:
//
//     1. Forcing the coreserver into io_uring mode for every
//        test in the sectest package (via the
//        PULSYS_TEST_IOURING env var honored by testserver.New).
//
//     2. After the suite runs, asserting that the io_uring
//        fused-write counter is non-zero -- which proves the
//        Linux iouring path was actually exercised, not the
//        fallback cork+sendfile path.
//
//   When PULSYS_TEST_IOURING_REQUIRE=1, a zero counter is a
//   hard failure (kernel too old, ring setup denied, etc.).
//   Without _REQUIRE, a zero counter is a t.Skip so dev boxes
//   without io_uring can still run the suite at lower fidelity.
//
//   This is the missing piece between "passes on Darwin" and
//   "the production Linux deployment exercises the same code
//   path the security tests covered."

//go:build linux

package sectest

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/pulsys-io/pulsys/internal/telemetry"
)

// TestIoUring_PathWasExercised reads the io_uring fused-write
// counter AFTER running a warm-hit-ish request through the
// stack.  If the counter is zero, either:
//
//   - io_uring was not actually requested (the harness didn't
//     honor PULSYS_TEST_IOURING -- a harness bug), or
//   - io_uring ring setup failed (kernel too old or syscall
//     denied -- the coreserver silently fell back to
//     cork+sendfile).
//
// Either case means the Linux iouring path was NOT covered
// by the security suite, which defeats Phase 4.5's purpose.
// When PULSYS_TEST_IOURING_REQUIRE=1 we fail; otherwise we
// skip with a loud message.
func TestIoUring_PathWasExercised(t *testing.T) {
	// This test only matters when the env var was set; on a
	// developer's local `go test ./...` we skip silently.
	if !envSet("PULSYS_TEST_IOURING") {
		t.Skip("PULSYS_TEST_IOURING not set; this test is Phase-4.5 / compose-only")
	}

	// We can't observe ioUringReady directly (it's a private
	// atomic.Bool on coreserver.Server).  Instead we drive
	// a warm hit through the stack and read the public
	// telemetry counter.  If the counter increments, the
	// io_uring header-write path executed.
	stack := newStack(t)
	before := telemetry.IoUringFusedSnapshot()

	// Hit a small static path several times to give the warm
	// path a chance to fire.  The first hit will warm the
	// store; subsequent hits are the warm-cache path that
	// dispatches through io_uring on Linux.
	url := stack.ProxyURL() + "/healthz"
	client := &http.Client{}
	for i := 0; i < 8; i++ {
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("warm GET #%d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	after := telemetry.IoUringFusedSnapshot()
	delta := after - before

	required := envTruthy("PULSYS_TEST_IOURING_REQUIRE")
	if delta < 1 {
		msg := "io_uring fused-write counter did not increment under the security matrix " +
			"(kernel too old, CAP_SYS_NICE missing, or harness regression)"
		if required {
			t.Fatalf("Phase 4.5 invariant violated: %s\n  before=%d after=%d", msg, before, after)
		}
		t.Skipf("Phase 4.5 advisory: %s (delta=0; required=false)", msg)
	}
	t.Logf("io_uring fused-write counter advanced by %d under the security matrix", delta)
}

// envSet reports whether the named env var is set (even to "").
// envTruthy is the looser "non-zero non-false" check.
func envSet(name string) bool {
	_, ok := os.LookupEnv(name)
	return ok
}

func envTruthy(name string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	switch v {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}
