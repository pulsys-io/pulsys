// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package hfhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestListFilesAndDownload(t *testing.T) {
	const body = "hello-world-payload-12345"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/main"):
			_ = json.NewEncoder(w).Encode([]treeEntry{
				{Type: "file", Path: "weights.bin", Size: int64(len(body))},
			})
		case strings.Contains(r.URL.Path, "/resolve/main/weights.bin"):
			if r.Header.Get("Range") != "" {
				http.Error(w, "unexpected range", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	files, err := c.ListFiles(context.Background(), "bench/tiny", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "weights.bin" {
		t.Fatalf("files: %+v", files)
	}

	dir := t.TempDir()
	n, err := c.Download(context.Background(), "bench/tiny", DownloadOpts{
		LocalDir: dir,
		Workers:  4,
		Revision: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(body)) {
		t.Fatalf("bytes=%d want %d", n, len(body))
	}
	got, err := os.ReadFile(filepath.Join(dir, "weights.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("content %q", got)
	}
}

func TestWarmCommitPinnedTree(t *testing.T) {
	const commit = "060db6499f32faf8b98477b0a26969ef7d8b9987"
	var treeCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/resolve/main/weights.bin"):
			w.Header().Set("X-Repo-Commit", commit)
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/tree/main"):
			treeCalls.Add(1)
			_ = json.NewEncoder(w).Encode([]treeEntry{
				{Type: "file", Path: "weights.bin", Size: 10},
			})
		case strings.Contains(r.URL.Path, "/tree/"+commit):
			treeCalls.Add(1)
			_ = json.NewEncoder(w).Encode([]treeEntry{
				{Type: "file", Path: "weights.bin", Size: 10},
			})
		case strings.Contains(r.URL.Path, "/resolve/main/weights.bin"):
			_, _ = w.Write([]byte("0123456789"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	files, err := c.ListFiles(context.Background(), "org/model", "main")
	if err != nil {
		t.Fatal(err)
	}
	c.warmCommitPinnedTree(context.Background(), "org/model", DownloadOpts{Revision: "main", RepoType: RepoTypeModel}, files)
	if treeCalls.Load() != 2 {
		t.Fatalf("tree calls=%d want 2 (main + commit sha)", treeCalls.Load())
	}
}

func TestDownloadSink(t *testing.T) {
	const chunk = 1 << 20
	payload := make([]byte, chunk*2+123)
	for i := range payload {
		payload[i] = byte(i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tree/main") {
			_ = json.NewEncoder(w).Encode([]treeEntry{
				{Type: "file", Path: "big.bin", Size: int64(len(payload))},
			})
			return
		}
		if !strings.Contains(r.URL.Path, "/resolve/main/big.bin") {
			http.NotFound(w, r)
			return
		}
		rng := r.Header.Get("Range")
		if rng == "" {
			_, _ = w.Write(payload)
			return
		}
		var start, end int64
		if err := parseRangeHeader(rng, int64(len(payload)), &start, &end); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	devnull, closeFn, err := OpenSink(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	n, err := c.Download(context.Background(), "bench/big", DownloadOpts{
		Sink:       devnull,
		Workers:    8,
		Revision:   "main",
		RangeChunk: chunk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("bytes=%d want %d", n, len(payload))
	}
}

// TestDownloadParallelRangeSingleOpen exercises the filePool: many ranges
// targeting the same file must all share one *os.File and the final
// on-disk contents must match the upstream payload byte-for-byte.
//
// The httptest server counts *open* GETs (we approximate "fd opens" via
// per-task GET hits because the test only ever uses one *os.File per
// path); for our purposes we just need bytewise correctness with
// parallel WriteAt at random offsets.
func TestDownloadParallelRangeSingleOpen(t *testing.T) {
	const (
		fileSize = 4 << 20  // 4 MiB
		chunk    = 64 << 10 // 64 KiB ranges -> 64 parallel tasks
	)
	payload := make([]byte, fileSize)
	for i := range payload {
		payload[i] = byte(i * 31)
	}

	var concurrentGETs atomic.Int64
	var maxConcurrentGETs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/main"):
			_ = json.NewEncoder(w).Encode([]treeEntry{
				{Type: "file", Path: "deep/nested/big.bin", Size: int64(len(payload))},
			})
		case strings.Contains(r.URL.Path, "/resolve/main/deep/nested/big.bin"):
			n := concurrentGETs.Add(1)
			for {
				old := maxConcurrentGETs.Load()
				if n <= old || maxConcurrentGETs.CompareAndSwap(old, n) {
					break
				}
			}
			defer concurrentGETs.Add(-1)
			rng := r.Header.Get("Range")
			if rng == "" {
				_, _ = w.Write(payload)
				return
			}
			var start, end int64
			if err := parseRangeHeader(rng, int64(len(payload)), &start, &end); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[start : end+1])
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	dir := t.TempDir()
	n, err := c.Download(context.Background(), "bench/big", DownloadOpts{
		LocalDir:   dir,
		Workers:    16,
		Revision:   "main",
		RangeChunk: chunk,
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("bytes=%d want %d", n, len(payload))
	}
	if maxConcurrentGETs.Load() < 2 {
		t.Fatalf("expected parallel GETs (saw max=%d); filePool may be serializing", maxConcurrentGETs.Load())
	}
	got, err := os.ReadFile(filepath.Join(dir, "deep", "nested", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("file size=%d want %d", len(got), len(payload))
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte mismatch at %d: got %d want %d", i, got[i], payload[i])
		}
	}
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	const body = "retry-me-please"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tree/main") {
			_ = json.NewEncoder(w).Encode([]treeEntry{{Type: "file", Path: "w.bin", Size: int64(len(body))}})
			return
		}
		// First two attempts: 429.  Third: success.
		if hits.Add(1) <= 2 {
			http.Error(w, "throttled", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &Client{
		Base: srv.URL, HTTP: srv.Client(),
		MaxRetries: 5, RetryBaseDelay: time.Millisecond, RetryMaxDelay: 5 * time.Millisecond,
	}
	devnull, closeFn, _ := OpenSink(os.DevNull)
	defer closeFn()
	n, err := c.Download(context.Background(), "org/m", DownloadOpts{
		Sink:    devnull,
		Workers: 1,
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("bytes=%d want %d", n, len(body))
	}
	if hits.Load() < 3 {
		t.Fatalf("expected at least 3 attempts, got %d", hits.Load())
	}
}

func TestRetryDoesNotRetry404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := &Client{
		Base: srv.URL, HTTP: srv.Client(),
		MaxRetries: 5, RetryBaseDelay: time.Millisecond, RetryMaxDelay: 2 * time.Millisecond,
	}
	_, err := c.ListFiles(context.Background(), "org/missing", "main")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	var he *httpError
	if !errors.As(err, &he) || he.status != http.StatusNotFound {
		t.Fatalf("expected 404 httpError, got %v", err)
	}
}

func TestFilterFiles(t *testing.T) {
	files := []FileEntry{
		{Path: "config.json", Size: 1024},
		{Path: "tokenizer.model", Size: 2048},
		{Path: "model-00001-of-00002.safetensors", Size: 1 << 30},
		{Path: "model-00002-of-00002.safetensors", Size: 1 << 30},
		{Path: "pytorch_model.bin", Size: 2 << 30},
		{Path: "nested/extra.bin", Size: 4096},
	}
	cases := []struct {
		name string
		opts DownloadOpts
		want []string
	}{
		{
			name: "passthrough",
			opts: DownloadOpts{},
			want: []string{"config.json", "tokenizer.model", "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors", "pytorch_model.bin", "nested/extra.bin"},
		},
		{
			name: "include safetensors only",
			opts: DownloadOpts{Include: []string{"*.safetensors"}},
			want: []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"},
		},
		{
			name: "exclude bin",
			opts: DownloadOpts{Exclude: []string{"*.bin"}},
			want: []string{"config.json", "tokenizer.model", "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"},
		},
		{
			name: "explicit Files",
			opts: DownloadOpts{Files: []string{"config.json", "tokenizer.model"}},
			want: []string{"config.json", "tokenizer.model"},
		},
		{
			name: "include + exclude precedence (exclude wins)",
			opts: DownloadOpts{Include: []string{"*.safetensors", "*.bin"}, Exclude: []string{"*.bin"}},
			want: []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// filterFiles trims to opts.Files by exact path; ensure we
			// pass a fresh slice each iteration so the in-place
			// re-slicing doesn't leak across tests.
			in := append([]FileEntry(nil), files...)
			got, err := filterFiles(in, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range got {
				names = append(names, e.Path)
			}
			if !equalStringSlice(names, tc.want) {
				t.Fatalf("got %v want %v", names, tc.want)
			}
		})
	}
}

func TestRepoTypeResolveURL(t *testing.T) {
	c := &Client{Base: "https://hf.local"}
	cases := []struct {
		rt   RepoType
		want string
	}{
		{RepoTypeModel, "https://hf.local/org/m/resolve/main/file.bin"},
		{RepoTypeDataset, "https://hf.local/datasets/org/m/resolve/main/file.bin"},
		{RepoTypeSpace, "https://hf.local/spaces/org/m/resolve/main/file.bin"},
	}
	for _, tc := range cases {
		got := c.resolveURL("org/m", "main", "file.bin", tc.rt)
		if got != tc.want {
			t.Fatalf("rt=%s: got %s want %s", tc.rt, got, tc.want)
		}
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func parseRangeHeader(hdr string, total int64, start, end *int64) error {
	if !strings.HasPrefix(hdr, "bytes=") {
		return errBadRange
	}
	rest := strings.TrimPrefix(hdr, "bytes=")
	i := strings.IndexByte(rest, '-')
	if i < 0 {
		return errBadRange
	}
	var err error
	*start, err = parseInt64(rest[:i])
	if err != nil {
		return err
	}
	*end, err = parseInt64(rest[i+1:])
	if err != nil {
		return err
	}
	if *end <= 0 || *end >= total {
		*end = total - 1
	}
	return nil
}

var errBadRange = errString("bad range")

type errString string

func (e errString) Error() string { return string(e) }

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errBadRange
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}
