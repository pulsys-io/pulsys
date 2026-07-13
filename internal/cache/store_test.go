// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestParseSingleRange(t *testing.T) {
	s, e, ok := ParseSingleRange("bytes=0-9", 100)
	if !ok || s != 0 || e != 10 {
		t.Fatalf("%d %d", s, e)
	}
	s, e, ok = ParseSingleRange("bytes=0-", 100)
	if !ok || s != 0 || e != 100 {
		t.Fatalf("%d %d", s, e)
	}
}

func TestStoreFullRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	key := KeyHex("GET", "huggingface.co", "/m/resolve/main/x", "", "")
	body := bytes.Repeat([]byte("a"), 1024)
	meta, err := s.WriteFullFromStream(key, 200, "huggingface.co", "/m/resolve/main/x", "", "e", "application/octet-stream", bytes.NewReader(body), 1024)
	if err != nil || meta == nil {
		t.Fatal(err)
	}
	f, err := s.OpenBody(key)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 2048)
	n, _ := f.Read(buf)
	if n != 1024 {
		t.Fatalf("len %d", n)
	}
	_ = filepath.Join(dir, "x")
}
