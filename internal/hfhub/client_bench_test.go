// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package hfhub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
)

// newBenchServer returns a fake HF server that responds to ListFiles + GET
// with optional Range.  Body bytes are a zero-filled slab sized to match
// fileSize; supports 200 (no Range) and 206 (Range).
//
// The server keeps zero allocations on its own data path (a single shared
// payload slab and SectionReader-style slicing) so any allocations the
// benchmark measures originate inside the client we're testing.
func newBenchServer(b *testing.B, fileSize int64, repo, name string) *httptest.Server {
	b.Helper()
	payload := make([]byte, fileSize)
	mux := http.NewServeMux()
	treePath := "/api/models/" + repo + "/tree/main"
	resolvePath := "/" + repo + "/resolve/main/" + name

	mux.HandleFunc(treePath, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]treeEntry{
			{Type: "file", Path: name, Size: fileSize},
		})
	})
	mux.HandleFunc(resolvePath, func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", fileSize))
			_, _ = w.Write(payload)
			return
		}
		var start, end int64
		if err := parseRangeHeader(rng, fileSize, &start, &end); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	})
	return httptest.NewServer(mux)
}

// BenchmarkDownloadSink_64MiB streams 64 MiB through the client with the
// /dev/null sink and reports bytes/alloc + allocs/op via b.ReportAllocs.
//
// Run:
//
//	go test ./internal/hfhub -bench BenchmarkDownloadSink -benchmem -benchtime=10x
//
// Interpretation: with N=10 iterations × 64 MiB the per-op AllocedBytesPerOp
// divided by 64 MiB is "bytes per downloaded byte" -- target <1e-4 for a
// tuned client.  AllocsPerOp is the absolute malloc count per 64 MiB run.
func BenchmarkDownloadSink_64MiB(b *testing.B) {
	benchSink(b, 64<<20, 16<<20, 8) // 64 MiB total, 16 MiB chunks, 8 workers
}

func BenchmarkDownloadSink_256MiB(b *testing.B) {
	benchSink(b, 256<<20, 16<<20, 16)
}

func BenchmarkDownloadSink_1GiB(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 1 GiB bench in -short")
	}
	benchSink(b, 1<<30, 16<<20, 32)
}

func benchSink(b *testing.B, total, chunk int64, workers int) {
	srv := newBenchServer(b, total, "bench/big", "weights.bin")
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	devnull, closeFn, err := OpenSink(os.DevNull)
	if err != nil {
		b.Fatal(err)
	}
	defer closeFn()

	// Warmup: prime the HTTP client's connection pool so the first op
	// doesn't carry a tcp-dial allocation that all subsequent ops avoid.
	_, _ = c.Download(context.Background(), "bench/big", DownloadOpts{
		Sink:       devnull,
		Workers:    workers,
		Revision:   "main",
		RangeChunk: chunk,
	})
	runtime.GC()

	// Manual MemStats snapshot so we can report bytes-allocated / bytes-
	// downloaded (the headline efficiency metric) in addition to the
	// stdlib's B/op + allocs/op (which we keep via ReportAllocs).
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(total)
	for i := 0; i < b.N; i++ {
		n, err := c.Download(context.Background(), "bench/big", DownloadOpts{
			Sink:       devnull,
			Workers:    workers,
			Revision:   "main",
			RangeChunk: chunk,
		})
		if err != nil {
			b.Fatalf("download: %v", err)
		}
		if n != total {
			b.Fatalf("got %d bytes, want %d", n, total)
		}
	}
	b.StopTimer()
	runtime.ReadMemStats(&after)

	totalBytesDL := float64(total) * float64(b.N)
	allocBytes := float64(after.TotalAlloc - before.TotalAlloc)
	allocObjs := float64(after.Mallocs - before.Mallocs)
	// Headline metrics; show up under "Extra" in BenchmarkResult and on
	// each line of `go test -bench` output.
	b.ReportMetric(allocBytes/totalBytesDL, "bytesAlloc/byteDL")
	b.ReportMetric(allocObjs/(totalBytesDL/(1024*1024)), "allocs/MiB")
}

// BenchmarkDownloadDisk_* exercises the real product path (--local-dir
// against a fresh tempdir per iteration).  The metric we publish here
// is the same shape as Sink (bytesAlloc/byteDL + allocs/MiB) so the
// two are directly comparable; the disk version necessarily allocates
// more (file open + path traversal once per iteration) but the
// per-range cost should be flat after the filePool change.
func BenchmarkDownloadDisk_64MiB(b *testing.B) {
	benchDisk(b, 64<<20, 16<<20, 8)
}

func BenchmarkDownloadDisk_256MiB(b *testing.B) {
	benchDisk(b, 256<<20, 16<<20, 16)
}

func BenchmarkDownloadDisk_1GiB(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 1 GiB disk bench in -short")
	}
	benchDisk(b, 1<<30, 16<<20, 32)
}

func benchDisk(b *testing.B, total, chunk int64, workers int) {
	srv := newBenchServer(b, total, "bench/big", "weights.bin")
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}

	// Warmup pass (separate tempdir) primes the connection pool.
	warmDir := b.TempDir()
	if _, err := c.Download(context.Background(), "bench/big", DownloadOpts{
		LocalDir:   warmDir,
		Workers:    workers,
		Revision:   "main",
		RangeChunk: chunk,
	}); err != nil {
		b.Fatal(err)
	}
	runtime.GC()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(total)
	for i := 0; i < b.N; i++ {
		dir := b.TempDir() // fresh dir each iter so we don't measure overwrite cost only
		n, err := c.Download(context.Background(), "bench/big", DownloadOpts{
			LocalDir:   dir,
			Workers:    workers,
			Revision:   "main",
			RangeChunk: chunk,
		})
		if err != nil {
			b.Fatalf("download: %v", err)
		}
		if n != total {
			b.Fatalf("got %d bytes, want %d", n, total)
		}
	}
	b.StopTimer()
	runtime.ReadMemStats(&after)

	totalBytesDL := float64(total) * float64(b.N)
	allocBytes := float64(after.TotalAlloc - before.TotalAlloc)
	allocObjs := float64(after.Mallocs - before.Mallocs)
	b.ReportMetric(allocBytes/totalBytesDL, "bytesAlloc/byteDL")
	b.ReportMetric(allocObjs/(totalBytesDL/(1024*1024)), "allocs/MiB")
}
