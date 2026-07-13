// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestObject(t *testing.T, s *Store, key, path string, total int64) {
	t.Helper()
	body := bytes.Repeat([]byte("x"), int(total))
	_, err := s.WriteFullFromStream(key, 200, "huggingface.co", path, "", "e", "application/octet-stream", bytes.NewReader(body), total)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.LoadMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	_, closer, err := s.AcquireBody(key)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
}

func TestPurgeKeys_RemovesMatchingObjects(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	writeTestObject(t, s, "key1", "/Org/Model/resolve/main/a.bin", 100)
	writeTestObject(t, s, "key2", "/Org/Model/resolve/main/b.bin", 200)
	writeTestObject(t, s, "key3", "/Other/X/resolve/main/c.bin", 50)

	purged, freed, err := s.PurgeKeys(func(_ string, m *Meta) bool {
		return strings.HasPrefix(m.Path, "/Org/Model/")
	})
	if err != nil {
		t.Fatal(err)
	}
	if purged != 2 || freed != 300 {
		t.Fatalf("purged=%d freed=%d", purged, freed)
	}
	if _, err := os.Stat(filepath.Join(dir, "v1", "objects", "key1")); !os.IsNotExist(err) {
		t.Fatal("key1 should be gone")
	}
	if _, err := os.Stat(filepath.Join(dir, "v1", "objects", "key3")); err != nil {
		t.Fatal("key3 should remain")
	}
}

func TestPurgeKeys_SkipsCorruptMeta(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	writeTestObject(t, s, "good", "/m/resolve/main/x", 10)
	badDir := filepath.Join(dir, "v1", "objects", "bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "meta.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	purged, _, err := s.PurgeKeys(func(_ string, m *Meta) bool {
		return strings.HasPrefix(m.Path, "/m/")
	})
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Fatalf("purged=%d", purged)
	}
	if _, err := os.Stat(badDir); err != nil {
		t.Fatal("corrupt dir should remain")
	}
}

func TestPurgeKeys_AlsoDropsFromLRU(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	writeTestObject(t, s, "k1", "/a/b/resolve/main/f", 64)
	if s.metaCache.Len() != 1 || s.bodyHandles.Len() != 1 {
		t.Fatalf("meta=%d body=%d", s.metaCache.Len(), s.bodyHandles.Len())
	}

	_, _, err = s.PurgeKeys(func(_ string, m *Meta) bool {
		return m.Path == "/a/b/resolve/main/f"
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.metaCache.Len() != 0 || s.bodyHandles.Len() != 0 {
		t.Fatalf("LRU not cleared: meta=%d body=%d", s.metaCache.Len(), s.bodyHandles.Len())
	}
}

func TestPurgeKeys_MatchesNone(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	writeTestObject(t, s, "k1", "/x/resolve/main/y", 10)
	purged, freed, err := s.PurgeKeys(func(_ string, _ *Meta) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if purged != 0 || freed != 0 {
		t.Fatalf("purged=%d freed=%d", purged, freed)
	}
}

func TestSegment_OriginPathRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	const origin = "/Qwen/Qwen2.5-0.5B/resolve/main/model.safetensors"
	w, err := s.BeginSegment("k1", SegmentParams{
		Status:       200,
		UpstreamHost: "cas-bridge.xethub.hf.co",
		Path:         "/xet-bridge-us/abc/sha",
		Length:       4,
		Total:        4,
		OriginPath:   origin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := s.LoadMeta("k1")
	if err != nil || m == nil {
		t.Fatalf("load meta: %v %v", m, err)
	}
	if m.OriginPath != origin {
		t.Fatalf("origin_path=%q want %q", m.OriginPath, origin)
	}
	if len(m.OriginPaths) != 1 || m.OriginPaths[0] != origin {
		t.Fatalf("origin_paths=%v want [%q]", m.OriginPaths, origin)
	}
}

// Two distinct HF repos collide on a Xet chunk URL: the second
// writer must register as an additional owner without clobbering
// the first writer's attribution.
func TestSegment_OriginPaths_DedupAppend(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	const (
		ownerA = "/Qwen/Qwen2.5-0.5B/resolve/main/model.safetensors"
		ownerB = "/Acme/Qwen2.5-0.5B-Q4/resolve/main/model.safetensors"
	)
	write := func(owner string) {
		w, err := s.BeginSegment("k1", SegmentParams{
			Status: 200, UpstreamHost: "cas-bridge.xethub.hf.co",
			Path: "/xet-bridge-us/abc/sha", Length: 4, Total: 4,
			OriginPath: owner,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("abcd")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	write(ownerA)
	write(ownerB)
	write(ownerA) // duplicate, must not double-add

	m, err := s.LoadMeta("k1")
	if err != nil || m == nil {
		t.Fatalf("load meta: %v %v", m, err)
	}
	if m.OriginPath != ownerA {
		t.Fatalf("origin_path=%q want first-writer %q", m.OriginPath, ownerA)
	}
	if len(m.OriginPaths) != 2 {
		t.Fatalf("origin_paths len=%d want 2: %v", len(m.OriginPaths), m.OriginPaths)
	}
	if m.OriginPaths[0] != ownerB || m.OriginPaths[1] != ownerA {
		t.Fatalf("origin_paths=%v want sorted [B, A]", m.OriginPaths)
	}
}

func TestSegment_OriginPaths_Cap(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxOriginPaths+5; i++ {
		owner := "/Org" + strings.Repeat("X", i+1) + "/Repo/resolve/main/x"
		w, err := s.BeginSegment("k1", SegmentParams{
			Status: 200, UpstreamHost: "cas-bridge.xethub.hf.co",
			Path: "/xet-bridge-us/abc/sha", Length: 1, Total: 1,
			OriginPath: owner,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	m, err := s.LoadMeta("k1")
	if err != nil || m == nil {
		t.Fatalf("load meta: %v %v", m, err)
	}
	if len(m.OriginPaths) != MaxOriginPaths {
		t.Fatalf("origin_paths len=%d want %d", len(m.OriginPaths), MaxOriginPaths)
	}
}

// PurgeOrTrim's Trim branch must rewrite meta.json and keep the
// body file on disk; PurgeKeys-style Remove still wipes the dir.
func TestPurgeOrTrim_TrimAndRemove(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	// Shared body owned by two HF repos.
	w, err := s.BeginSegment("kShared", SegmentParams{
		Status: 200, UpstreamHost: "cas-bridge.xethub.hf.co",
		Path: "/xet-bridge-us/abc/sha", Length: 4, Total: 4,
		OriginPath: "/A/M/resolve/main/x",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("abcd"))
	_ = w.Close()
	w, err = s.BeginSegment("kShared", SegmentParams{
		Status: 200, UpstreamHost: "cas-bridge.xethub.hf.co",
		Path: "/xet-bridge-us/abc/sha", Length: 4, Total: 4,
		OriginPath: "/B/M/resolve/main/x",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("abcd"))
	_ = w.Close()
	// Solo body that the decide func will mark for Remove.
	writeTestObject(t, s, "kSolo", "/C/Z/resolve/main/y", 16)

	res, err := s.PurgeOrTrim(func(_ string, m *Meta) (PurgeDecision, *Meta) {
		switch {
		case m.OriginPath == "/A/M/resolve/main/x" || contains(m.OriginPaths, "/A/M/resolve/main/x"):
			// Trim: drop owner A but leave the body for B.
			trimmed := *m
			trimmed.OriginPath = "/B/M/resolve/main/x"
			trimmed.OriginPaths = []string{"/B/M/resolve/main/x"}
			return DecisionTrim, &trimmed
		case strings.HasPrefix(m.Path, "/C/Z/"):
			return DecisionRemove, nil
		}
		return DecisionKeep, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Trimmed != 1 || res.Purged != 1 || res.BytesFreed != 16 {
		t.Fatalf("res=%+v", res)
	}
	// kShared body must still be on disk and meta refreshed.
	if _, err := os.Stat(filepath.Join(dir, "v1", "objects", "kShared", "body")); err != nil {
		t.Fatalf("kShared body missing after Trim: %v", err)
	}
	m, err := s.LoadMeta("kShared")
	if err != nil || m == nil {
		t.Fatalf("kShared meta missing: %v %v", m, err)
	}
	if m.OriginPath != "/B/M/resolve/main/x" || len(m.OriginPaths) != 1 || m.OriginPaths[0] != "/B/M/resolve/main/x" {
		t.Fatalf("kShared meta not trimmed: %+v", m)
	}
	if _, err := os.Stat(filepath.Join(dir, "v1", "objects", "kSolo")); !os.IsNotExist(err) {
		t.Fatal("kSolo should be gone")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestPurgeKeys_MatchesAll(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	writeTestObject(t, s, "k1", "/a/resolve/main/1", 10)
	writeTestObject(t, s, "k2", "/b/resolve/main/2", 20)
	purged, freed, err := s.PurgeKeys(func(_ string, _ *Meta) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if purged != 2 || freed != 30 {
		t.Fatalf("purged=%d freed=%d", purged, freed)
	}
}
