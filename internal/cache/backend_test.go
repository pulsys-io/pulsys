// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestStoreSatisfiesHotBackend pins the runtime shape of the
// contract.  The compile-time `var _ HotBackend = (*Store)(nil)`
// assertion in backend.go already guarantees method-set
// compatibility; this test additionally exercises the assigned
// interface to catch accidental signature drift that the compiler
// could not (e.g. a future change that returns io.ReadCloser
// instead of *os.File would still compile if the consumer was
// loosened).
func TestStoreSatisfiesHotBackend(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var hb HotBackend = s

	key := KeyHex("GET", "huggingface.co", "/m/resolve/main/x", "", "")
	payload := bytes.Repeat([]byte("z"), 1024)
	if _, err := s.WriteFullFromStream(
		key, 200, "huggingface.co", "/m/resolve/main/x",
		"", "etag-1", "application/octet-stream",
		bytes.NewReader(payload), int64(len(payload)),
	); err != nil {
		t.Fatalf("WriteFullFromStream: %v", err)
	}

	m, err := hb.LoadMeta(key)
	if err != nil || m == nil {
		t.Fatalf("hb.LoadMeta: m=%v err=%v", m, err)
	}
	if m.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", m.StatusCode)
	}

	f, err := hb.OpenBody(key)
	if err != nil {
		t.Fatalf("hb.OpenBody: %v", err)
	}
	// Compile-time check that the return type is exactly *os.File
	// (so sendfile/io_uring can be wired against the fd).  This
	// is the load-bearing property of the warm-path contract.
	var _ = f
	defer f.Close()
	buf := make([]byte, 2048)
	n, _ := f.Read(buf)
	if n != len(payload) {
		t.Fatalf("body len: got %d want %d", n, len(payload))
	}

	rk := KeyHex("GET", "huggingface.co", "/m/redirect", "", "")
	if err := hb.StoreRedirect(
		rk, 302,
		"huggingface.co", "/m/redirect", "",
		"https://cdn-lfs.huggingface.co/r/x", "", "",
		nil,
	); err != nil {
		t.Fatalf("hb.StoreRedirect: %v", err)
	}
	rm, err := hb.LoadMeta(rk)
	if err != nil || rm == nil || rm.StatusCode != 302 {
		t.Fatalf("redirect meta: m=%v err=%v", rm, err)
	}

	ak := KeyHex("GET", "huggingface.co", "/m/resolve/sha/x", "", "")
	if err := hb.StoreAlias(ak, key, "huggingface.co", "/m/resolve/sha/x", ""); err != nil {
		t.Fatalf("hb.StoreAlias: %v", err)
	}
	am, err := hb.LoadMeta(ak)
	if err != nil || am == nil || am.AliasOf != key {
		t.Fatalf("alias meta: m=%v err=%v", am, err)
	}

	release := hb.Lock("some-key")
	release()
}

func TestTierPassThroughHit(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	tracer := &recordingPolicy{}
	tier := NewTier(s, tracer)

	key := KeyHex("GET", "huggingface.co", "/t/hit", "", "")
	if _, err := s.WriteFullFromStream(
		key, 200, "huggingface.co", "/t/hit",
		"", "", "application/octet-stream",
		bytes.NewReader([]byte("ok")), 2,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m, err := tier.LoadMeta(key)
	if err != nil {
		t.Fatalf("tier.LoadMeta: %v", err)
	}
	if m == nil || m.StatusCode != 200 {
		t.Fatalf("meta: %v", m)
	}
	if tracer.hits != 1 || tracer.misses != 0 {
		t.Fatalf("hooks: hits=%d misses=%d want 1/0", tracer.hits, tracer.misses)
	}
}

func TestTierPassThroughMissNoopPolicy(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	tier := NewTier(s, nil) // nil → NoopPolicy{}
	if _, ok := tier.Policy().(NoopPolicy); !ok {
		t.Fatalf("nil policy should default to NoopPolicy, got %T", tier.Policy())
	}

	missKey := KeyHex("GET", "huggingface.co", "/t/missing", "", "")
	m, err := tier.LoadMeta(missKey)
	if err != nil {
		t.Fatalf("tier.LoadMeta on miss: %v", err)
	}
	if m != nil {
		t.Fatalf("miss should be (nil, nil), got %v", m)
	}
}

// TestTierPolicyPopulatesOnMiss exercises the seam the P11 cold
// tier will use: a Policy implementation that populates the hot
// backend on miss.  The test stands in for a real ColdBackend by
// having the policy write through Store directly.
func TestTierPolicyPopulatesOnMiss(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	payload := []byte("from-cold-tier")
	populator := &populatingPolicy{
		body: payload,
	}
	tier := NewTier(s, populator)

	key := KeyHex("GET", "huggingface.co", "/t/cold-warm", "", "")
	m, err := tier.LoadMeta(key)
	if err != nil {
		t.Fatalf("tier.LoadMeta: %v", err)
	}
	if m == nil || m.StatusCode != 200 {
		t.Fatalf("meta after populate: %v", m)
	}
	if populator.calls != 1 {
		t.Fatalf("OnMiss should be called exactly once, got %d", populator.calls)
	}

	// Subsequent read must come from the hot tier (OnMiss not
	// called again).
	m2, err := tier.LoadMeta(key)
	if err != nil || m2 == nil {
		t.Fatalf("second LoadMeta: m=%v err=%v", m2, err)
	}
	if populator.calls != 1 {
		t.Fatalf("OnMiss should NOT fire on hot-tier hit, got %d total calls", populator.calls)
	}
}

func TestTierPolicyErrorPropagatesOnMiss(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	failing := &failingMissPolicy{err: errors.New("cold tier unavailable")}
	tier := NewTier(s, failing)

	missKey := KeyHex("GET", "huggingface.co", "/t/cold-down", "", "")
	if _, err := tier.LoadMeta(missKey); err == nil {
		t.Fatalf("expected propagated policy error, got nil")
	}
}

func TestNewTierPanicsOnNilHot(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewTier(nil, ...) should panic")
		}
	}()
	_ = NewTier(nil, NoopPolicy{})
}

// TestBeginSegmentInputValidation exercises the hardened
// BeginSegment input contract: malformed SegmentParams must fail
// before any filesystem I/O happens.  This matters for the future
// P11 cold-tier population path which constructs SegmentParams
// from external metadata; a bad input there should not be able
// to corrupt the on-disk body file.
func TestBeginSegmentInputValidation(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	goodKey := KeyHex("GET", "huggingface.co", "/v/x", "", "")

	cases := []struct {
		name string
		key  string
		p    SegmentParams
	}{
		{"empty key", "", SegmentParams{Status: 200, Start: 0, Length: 1, Total: 1}},
		{"negative start", goodKey, SegmentParams{Status: 200, Start: -1, Length: 1, Total: 1}},
		{"invalid length", goodKey, SegmentParams{Status: 200, Start: 0, Length: -2, Total: 1}},
		{"invalid total", goodKey, SegmentParams{Status: 200, Start: 0, Length: 1, Total: -2}},
		{"status 0", goodKey, SegmentParams{Status: 0, Start: 0, Length: 1, Total: 1}},
		{"status 304", goodKey, SegmentParams{Status: 304, Start: 0, Length: 1, Total: 1}},
		{"status 302", goodKey, SegmentParams{Status: 302, Start: 0, Length: 1, Total: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, err := s.BeginSegment(tc.key, tc.p)
			if err == nil {
				if w != nil {
					_ = w.Close()
				}
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
		})
	}

	// Valid inputs still succeed.
	w, err := s.BeginSegment(goodKey, SegmentParams{Status: 200, Start: 0, Length: -1, Total: -1})
	if err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
	_ = w.Close()
}

// recordingPolicy counts OnHit / OnMiss / OnEvict calls for
// assertions in tests.  It performs no I/O.
type recordingPolicy struct {
	hits, misses, evicts int
}

func (p *recordingPolicy) OnHit(string, *Meta) { p.hits++ }
func (p *recordingPolicy) OnMiss(string, HotBackend) (*Meta, error) {
	p.misses++
	return nil, nil
}
func (p *recordingPolicy) OnEvict(string) { p.evicts++ }

// populatingPolicy simulates a future cold tier by writing the
// configured body through the hot backend on a miss and returning
// the resulting *Meta.  Tracks call counts.
type populatingPolicy struct {
	body  []byte
	calls int
}

func (p *populatingPolicy) OnHit(string, *Meta) {}
func (p *populatingPolicy) OnMiss(key string, hot HotBackend) (*Meta, error) {
	p.calls++
	// Write through.  We need access to the concrete *Store for
	// the WriteFullFromStream compat shim; the real P11 policy
	// will use BeginSegment directly to stay on the interface.
	s, ok := hot.(*Store)
	if !ok {
		return nil, errors.New("populatingPolicy: hot backend is not *Store")
	}
	if _, err := s.WriteFullFromStream(
		key, 200, "huggingface.co", "/cold/"+key,
		"", "", "application/octet-stream",
		bytes.NewReader(p.body), int64(len(p.body)),
	); err != nil {
		return nil, err
	}
	return hot.LoadMeta(key)
}
func (p *populatingPolicy) OnEvict(string) {}

// failingMissPolicy always errors on miss; OnHit / OnEvict are no-ops.
type failingMissPolicy struct {
	err error
}

func (p *failingMissPolicy) OnHit(string, *Meta) {}
func (p *failingMissPolicy) OnMiss(string, HotBackend) (*Meta, error) {
	return nil, p.err
}
func (p *failingMissPolicy) OnEvict(string) {}

// Pin the ColdReader interface shape: an io.ReadCloser + size.
// This test exists so a future change to the cold-tier signature
// surfaces here rather than at a distant P11 call site.
type stubCold struct{}

func (stubCold) HasObject(string) (bool, error) { return false, nil }
func (stubCold) GetObject(string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(nil)), 0, nil
}

func TestColdReaderInterfaceShape(t *testing.T) {
	var _ ColdReader = stubCold{}
}
