// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestLocalPutAndOpen(t *testing.T) {
	s, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("hello, world")
	stat, err := s.Put(context.Background(), bytes.NewReader(body), PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stat.OID != sha256Hex(body) {
		t.Fatalf("oid=%s want %s", stat.OID, sha256Hex(body))
	}
	if stat.Size != int64(len(body)) {
		t.Fatalf("size=%d want %d", stat.Size, len(body))
	}
	if !strings.HasPrefix(stat.StorageURL, "file://") {
		t.Fatalf("storage url=%q", stat.StorageURL)
	}

	rc, st, err := s.Open(context.Background(), stat.OID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if st.Size != int64(len(body)) {
		t.Fatalf("open size=%d want %d", st.Size, len(body))
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Fatalf("body mismatch")
	}
}

func TestLocalPutVerifiesOID(t *testing.T) {
	s, _ := NewLocal(t.TempDir())
	_, err := s.Put(context.Background(), bytes.NewReader([]byte("abc")),
		PutOptions{ExpectedOID: strings.Repeat("0", 64)})
	if !errors.Is(err, ErrOIDMismatch) {
		t.Fatalf("err=%v want ErrOIDMismatch", err)
	}
}

func TestLocalPutVerifiesSize(t *testing.T) {
	s, _ := NewLocal(t.TempDir())
	_, err := s.Put(context.Background(), bytes.NewReader([]byte("abc")),
		PutOptions{ExpectedSize: 99})
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err=%v want ErrSizeMismatch", err)
	}
}

func TestLocalPutDeduplicates(t *testing.T) {
	root := t.TempDir()
	s, _ := NewLocal(root)
	body := []byte("dedup me")
	first, err := s.Put(context.Background(), bytes.NewReader(body), PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Put(context.Background(), bytes.NewReader(body), PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.OID != second.OID {
		t.Fatalf("oids differ: %s vs %s", first.OID, second.OID)
	}
	// Only one file on disk.
	matches, _ := filepath.Glob(filepath.Join(root, "blobs", first.OID[:2], "*"))
	if len(matches) != 1 {
		t.Fatalf("want 1 blob got %d", len(matches))
	}
	// No leftover temp files.
	tmps, _ := filepath.Glob(filepath.Join(root, "tmp", "*"))
	if len(tmps) != 0 {
		t.Fatalf("temp files leaked: %v", tmps)
	}
}

func TestLocalOpenNotFound(t *testing.T) {
	s, _ := NewLocal(t.TempDir())
	_, _, err := s.Open(context.Background(), strings.Repeat("a", 64))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestLocalOpenInvalidOID(t *testing.T) {
	s, _ := NewLocal(t.TempDir())
	for _, bad := range []string{"", "short", strings.Repeat("z", 64), "../etc/passwd"} {
		if _, _, err := s.Open(context.Background(), bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("oid=%q err=%v want ErrNotFound", bad, err)
		}
	}
}

func TestLocalDeleteIdempotent(t *testing.T) {
	s, _ := NewLocal(t.TempDir())
	stat, _ := s.Put(context.Background(), bytes.NewReader([]byte("x")), PutOptions{})
	if err := s.Delete(context.Background(), stat.OID); err != nil {
		t.Fatal(err)
	}
	// Second delete is a no-op.
	if err := s.Delete(context.Background(), stat.OID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(context.Background(), stat.OID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

func TestLocalRangeReadViaSeeker(t *testing.T) {
	s, _ := NewLocal(t.TempDir())
	body := bytes.Repeat([]byte("0123456789"), 100) // 1000 bytes
	stat, _ := s.Put(context.Background(), bytes.NewReader(body), PutOptions{})

	rc, _, err := s.Open(context.Background(), stat.OID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	// Seek to byte 500, read 10.
	if _, err := rc.Seek(500, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	n, err := io.ReadFull(rc, buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 || string(buf) != "0123456789" {
		t.Fatalf("got %q", buf)
	}

	// ReaderAt should also work.
	buf2 := make([]byte, 5)
	if _, err := rc.ReadAt(buf2, 7); err != nil {
		t.Fatal(err)
	}
	if string(buf2) != "78901" {
		t.Fatalf("readat=%q", buf2)
	}
}

func TestLocalConcurrentPuts(t *testing.T) {
	s, _ := NewLocal(t.TempDir())
	body := bytes.Repeat([]byte("X"), 4096)
	want := sha256Hex(body)

	const N = 30
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			st, err := s.Put(context.Background(), bytes.NewReader(body), PutOptions{ExpectedOID: want})
			if err != nil {
				t.Errorf("put: %v", err)
				return
			}
			if st.OID != want {
				t.Errorf("oid=%s want %s", st.OID, want)
			}
		}()
	}
	wg.Wait()
	if _, err := os.Stat(filepath.Join(s.blobDir, want[:2], want)); err != nil {
		t.Fatalf("blob missing on disk: %v", err)
	}
}

func TestLocalContextCancellation(t *testing.T) {
	s, _ := NewLocal(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Put(ctx, bytes.NewReader([]byte("abc")), PutOptions{})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
