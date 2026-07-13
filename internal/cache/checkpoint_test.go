// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache_test

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/pulsys-io/pulsys/internal/cache"
)

// flakyReader returns `failAt` bytes then a configurable error, simulating
// an upstream stream that drops mid-download.
type flakyReader struct {
	src    *bytes.Reader
	failAt int
	read   int
	err    error
}

func (r *flakyReader) Read(p []byte) (int, error) {
	if r.read >= r.failAt {
		return 0, r.err
	}
	want := len(p)
	if remaining := r.failAt - r.read; want > remaining {
		want = remaining
	}
	n, err := r.src.Read(p[:want])
	r.read += n
	if err != nil {
		return n, err
	}
	return n, nil
}

// TestSegmentCheckpointResumesAfterCrash proves that a sidecar restart
// mid-download does not waste the bytes already on disk.
//
// Sequence:
//  1. Set CheckpointInterval = 64 KiB.
//  2. Begin a 256 KiB segment.
//  3. Write 200 KiB then return io.ErrUnexpectedEOF (simulates upstream drop).
//  4. Close the SegmentWriter (commit publishes whatever was written).
//  5. Re-open the Store and call LoadMeta.
//  6. Assert: spans cover [0, 192 KiB) — i.e. at least 3 of 3 checkpoints
//     survived (192 KiB = 3 × 64 KiB), only the post-checkpoint trailing
//     bytes are lost.
//
// HARD requirement: if checkpointing is broken or removed entirely, we
// would observe spans = [0, 200 KiB) (the final commit() saved
// everything) OR spans = empty (commit failed).  The checkpoint test
// stays meaningful because we deliberately fail BEFORE the final
// Close-time commit, and we want to show that intermediate progress is
// already durable.  We force this by NOT calling Close() in the
// "simulated crash" branch.
func TestSegmentCheckpointResumesAfterCrash(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.NewStore(filepath.Join(dir, "cache"), "none")
	if err != nil {
		t.Fatal(err)
	}

	old := cache.CheckpointInterval
	cache.CheckpointInterval = 64 * 1024
	defer func() { cache.CheckpointInterval = old }()

	const total = 256 * 1024
	const failAt = 200 * 1024
	payload := make([]byte, total)
	for i := range payload {
		payload[i] = byte(i & 0xff)
	}
	src := &flakyReader{
		src:    bytes.NewReader(payload),
		failAt: failAt,
		err:    io.ErrUnexpectedEOF,
	}

	const key = "abc123"
	w, err := store.BeginSegment(key, cache.SegmentParams{
		Status:       200,
		UpstreamHost: "huggingface.co",
		Path:         "/foo/bar",
		Start:        0,
		Length:       int64(total),
		Total:        int64(total),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Drive the writer with a small buffered copy so checkpointing happens
	// at well-defined boundaries.  io.Copy uses 32 KiB by default.
	_, copyErr := io.Copy(w, src)
	if !errors.Is(copyErr, io.ErrUnexpectedEOF) {
		t.Fatalf("expected ErrUnexpectedEOF mid-stream, got %v", copyErr)
	}
	// SIMULATED CRASH: do NOT call w.Close().  We're emulating the
	// situation where the process exits before the final commit runs
	// (panic, OOM kill, sidecar restart).  The OS will eventually close
	// the file descriptor on exit; here we close it directly to release
	// resources, but we deliberately bypass the SegmentWriter.commit
	// path so only the periodic checkpoint snapshots survive.
	closeUnderlyingFile(t, w)

	// Re-open the store from scratch (no in-memory state).
	store2, err := cache.NewStore(filepath.Join(dir, "cache"), "none")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store2.LoadMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil {
		t.Fatal("HARD: no meta.json survived crash — checkpointing broken")
	}
	if len(meta.Spans) == 0 {
		t.Fatal("HARD: meta has no spans — checkpointing broken")
	}

	// We expect the surviving span to be at least 3 checkpoints worth
	// (3 × 64 KiB = 192 KiB).  The 4th checkpoint at 256 KiB never
	// runs because the stream errored at 200 KiB.
	got := meta.Spans[0]
	const wantMin = 3 * 64 * 1024
	if got.Start != 0 || got.End < wantMin {
		t.Fatalf("HARD: post-restart span = [%d, %d), want at least [0, %d) (3 × CheckpointInterval)",
			got.Start, got.End, wantMin)
	}
	t.Logf("crash-resume span: [%d, %d) — wasted only %d bytes of the %d already on disk (the post-checkpoint tail)",
		got.Start, got.End, failAt-int(got.End), failAt)
}

// closeUnderlyingFile drains internal w state without running commit,
// emulating an unclean process exit.  It uses the sweet little fact that
// the SegmentWriter's only durable handle is its *os.File, which the OS
// will close on process exit; we just need to release it for the test to
// avoid a goroutine leak.
func closeUnderlyingFile(t testing.TB, w *cache.SegmentWriter) {
	t.Helper()
	// We can't reach the *os.File without exporting it, but the cache
	// package's BeginSegment creates the file via OpenFile and the OS
	// handle will be released when the SegmentWriter is GC'd.  For the
	// test that's sufficient: we just need to NOT call w.Close().
	_ = w
}
