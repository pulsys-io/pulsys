// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"bytes"
	"errors"
	"testing"
)

func TestQuota_RejectsOverCeiling(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStoreWithOptions(dir, "none", StoreOptions{MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	writeTestObject(t, s, "k1", "/Org/Model/resolve/main/a.bin", 90)

	_, err = s.BeginSegment("k2", SegmentParams{
		Status:       200,
		UpstreamHost: "huggingface.co",
		Path:         "/Org/Model/resolve/main/b.bin",
		Length:       20,
		Total:        20,
	})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err=%v want ErrQuotaExceeded", err)
	}
}

func TestQuota_PurgeFreesBytes(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStoreWithOptions(dir, "none", StoreOptions{MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	writeTestObject(t, s, "k1", "/Org/Model/resolve/main/a.bin", 100)
	if _, err := s.BeginSegment("k2", SegmentParams{
		Status:       200,
		UpstreamHost: "huggingface.co",
		Path:         "/Org/Model/resolve/main/b.bin",
		Length:       1,
		Total:        1,
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err=%v want ErrQuotaExceeded", err)
	}

	purged, freed, err := s.PurgeKeys(func(_ string, m *Meta) bool {
		return m.Path == "/Org/Model/resolve/main/a.bin"
	})
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 || freed != 100 {
		t.Fatalf("purged=%d freed=%d", purged, freed)
	}

	w, err := s.BeginSegment("k2", SegmentParams{
		Status:       200,
		UpstreamHost: "huggingface.co",
		Path:         "/Org/Model/resolve/main/b.bin",
		Length:       100,
		Total:        100,
	})
	if err != nil {
		t.Fatalf("begin after purge: %v", err)
	}
	_ = w.Close()
}

func TestQuota_CounterWarmFromDisk(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	writeTestObject(t, s, "k1", "/Org/Model/resolve/main/a.bin", 10)
	writeTestObject(t, s, "k2", "/Org/Model/resolve/main/b.bin", 20)

	reopened, err := NewStoreWithOptions(dir, "none", StoreOptions{MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	stats := reopened.Stats()
	if stats.UsedBytes != 30 || stats.EntryCount != 2 || stats.QuotaBytes != 100 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestQuota_DisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFullFromStream("k1", 200, "huggingface.co", "/Org/Model/resolve/main/a.bin", "", "e", "application/octet-stream", bytes.NewReader(bytes.Repeat([]byte("x"), 1024)), 1024); err != nil {
		t.Fatal(err)
	}
	stats := s.Stats()
	if stats.QuotaBytes != 0 || stats.UsedBytes != 1024 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestQuota_TrimDoesNotDecrement(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStoreWithOptions(dir, "none", StoreOptions{MaxBytes: 200})
	if err != nil {
		t.Fatal(err)
	}
	writeTestObject(t, s, "shared", "/Org/Model/resolve/main/a.bin", 100)
	before := s.Stats()

	res, err := s.PurgeOrTrim(func(key string, m *Meta) (PurgeDecision, *Meta) {
		if key != "shared" {
			return DecisionKeep, nil
		}
		trimmed := *m
		trimmed.OriginPaths = []string{"/Other/Model/resolve/main/a.bin"}
		return DecisionTrim, &trimmed
	})
	if err != nil {
		t.Fatal(err)
	}
	after := s.Stats()
	if res.Trimmed != 1 || res.Purged != 0 || res.BytesFreed != 0 {
		t.Fatalf("res=%+v", res)
	}
	if after.UsedBytes != before.UsedBytes || after.EntryCount != before.EntryCount {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
}
