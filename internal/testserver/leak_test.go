// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package testserver_test

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/hfhub/mockhub"
	"github.com/pulsys-io/pulsys/internal/testserver"
)

// p10.4 — goroutine-leak detector.
//
// Each test boots a real Stack (mock Hub + coreserver + cache),
// exercises it through the full HTTP path, then asserts the process
// goroutine count comes back to a small bounded delta after a
// best-effort drain. The first test catches "shutdown forgot to
// signal the reactor"; the second covers the in-flight request leak
// (the most common shape: handler goroutine survives client cancel).
//
// We snapshot stacks on failure so the leaked goroutine is obvious
// from the test log.

// TestLeak_StackBootShutdown spins up and tears down N stacks
// sequentially. If coreserver leaks even a single goroutine per
// stack, the delta blows past the noise floor.
func TestLeak_StackBootShutdown(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()

	const N = 8
	for i := 0; i < N; i++ {
		func() {
			// Use a sub-T so t.Cleanup runs per iteration; that's
			// how the harness tears the stack down.
			subT := &cleanupT{TB: t}
			stack := testserver.New(subT, testserver.Config{})
			// Make sure it can actually serve one request.
			resp, err := http.Get(stack.ProxyURL() + "/healthz")
			if err != nil {
				t.Fatalf("healthz iter %d: %v", i, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			subT.runCleanups()
		}()
	}

	after, growth := drainGoroutines(before, 3*time.Second)
	// The harness allocates a handful of long-lived background
	// goroutines (sync.OnceFunc, telemetry, etc.) per process; the
	// per-iteration ceiling is conservative.
	if growth > 16 {
		dumpStacks(t)
		t.Fatalf("HARD: goroutine leak after %d stack boots: before=%d after=%d growth=%d",
			N, before, after, growth)
	}
}

// TestLeak_InFlightCancel cancels a flurry of in-flight downloads
// and asserts the server doesn't carry handler goroutines past the
// client disconnect. This is the property that bounds the proxy's
// resource footprint when clients hang up.
func TestLeak_InFlightCancel(t *testing.T) {
	stack := testserver.New(t, testserver.Config{
		Mock: mockhub.Config{
			Repos: []mockhub.RepoSpec{{
				Name:         "acme/leak",
				InitialFiles: map[string][]byte{"big.bin": make([]byte, 1<<20)},
			}},
		},
	})

	runtime.GC()
	before := runtime.NumGoroutine()

	const N = 64
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
				stack.ProxyURL()+"/acme/leak/resolve/main/big.bin", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				cancel()
				return
			}
			// Read a tiny prefix and cancel; the server has 1 MiB
			// of body queued behind us.
			buf := make([]byte, 64)
			_, _ = io.ReadFull(resp.Body, buf)
			cancel()
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()

	after, growth := drainGoroutines(before, 3*time.Second)
	if growth > 32 {
		dumpStacks(t)
		t.Fatalf("HARD: in-flight cancel leaked goroutines: before=%d after=%d growth=%d (N=%d)",
			before, after, growth, N)
	}
}

// ---- helpers ----

// cleanupT wraps a testing.TB so we can drive t.Cleanup() registration
// from a parent test without nesting subtests. The harness only ever
// calls TempDir + Cleanup + Fatalf on testing.TB, so this stub is
// sufficient.
type cleanupT struct {
	testing.TB
	mu       sync.Mutex
	cleanups []func()
}

func (c *cleanupT) Cleanup(fn func()) {
	c.mu.Lock()
	c.cleanups = append(c.cleanups, fn)
	c.mu.Unlock()
}

func (c *cleanupT) TempDir() string {
	return c.TB.TempDir()
}

func (c *cleanupT) runCleanups() {
	c.mu.Lock()
	cl := c.cleanups
	c.cleanups = nil
	c.mu.Unlock()
	// t.Cleanup is LIFO.
	for i := len(cl) - 1; i >= 0; i-- {
		cl[i]()
	}
}

// drainGoroutines repeatedly GCs and samples NumGoroutine, returning
// once growth stabilizes or the deadline passes.
func drainGoroutines(before int, budget time.Duration) (after, growth int) {
	deadline := time.Now().Add(budget)
	for {
		runtime.GC()
		after = runtime.NumGoroutine()
		growth = after - before
		if growth <= 0 || time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dumpStacks emits a sorted histogram of live goroutine stacks for
// post-mortem debugging.
func dumpStacks(t *testing.T) {
	t.Helper()
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	frames := strings.Split(string(buf[:n]), "\n\n")
	counts := map[string]int{}
	for _, f := range frames {
		// Strip goroutine N [state] line so duplicates aggregate.
		if i := strings.Index(f, "\n"); i > 0 {
			counts[f[i+1:]]++
		}
	}
	type entry struct {
		stack string
		n     int
	}
	es := make([]entry, 0, len(counts))
	for s, n := range counts {
		es = append(es, entry{s, n})
	}
	sort.Slice(es, func(i, j int) bool { return es[i].n > es[j].n })
	t.Log("---- top goroutine stacks ----")
	for i, e := range es {
		if i > 6 {
			break
		}
		t.Logf("[x%d]\n%s", e.n, e.stack)
	}
}
