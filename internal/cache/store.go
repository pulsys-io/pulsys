// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/pulsys-io/pulsys/internal/telemetry"
)

// ErrQuotaExceeded is returned when a cache fill would exceed Store.MaxBytes.
// It is intentionally a hard failure rather than an eviction trigger: operators
// explicitly free space from the Models UI or raise -cache-max-bytes.
var ErrQuotaExceeded = errors.New("cache: storage quota exceeded")

// Meta describes cached object layout on disk.
//
// Status semantics:
//   - 200 / 206: Spans tracks which byte ranges of the body file are
//     populated; Total (when known) is the artifact's full size.
//   - 301 / 302 / 303 / 307 / 308: no body is stored; Location carries
//     the upstream redirect target as observed (pre-rewrite).  The
//     handler re-rewrites Location through PublicBaseURL on every warm
//     hit so the cache survives proxy URL changes.
type Meta struct {
	Version      int    `json:"version"`
	UpstreamHost string `json:"upstream_host"`
	Path         string `json:"path"`
	RawQuery     string `json:"raw_query"`
	Spans        []Span `json:"spans"`
	Total        *int64 `json:"total,omitempty"`
	ETag         string `json:"etag,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	StatusCode   int    `json:"status_code"`
	Location     string `json:"location,omitempty"`
	// ExtraHeaders preserves upstream response headers that downstream
	// clients (notably huggingface_hub) treat as authoritative metadata
	// independent of the body bytes -- e.g. X-Linked-Etag, X-Linked-Size,
	// X-Repo-Commit on /resolve/<rev>/<file> HEADs.  Without these,
	// huggingface_hub raises LocalEntryNotFoundError on warm/offline
	// HEAD probes even though the body is fully cached.  Keys are stored
	// in canonical http.Header form (e.g. "X-Linked-Etag").
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
	// AliasOf, when non-empty, names the canonical cache key whose body
	// + Location this entry mirrors.  Used to point /resolve/<sha>/<file>
	// at the same body as /resolve/<symbolic-rev>/<file>: huggingface_hub
	// downloads via /resolve/main/<file> on cold but validates via
	// /resolve/<X-Repo-Commit>/<file> on every subsequent run, and
	// without the alias the warm/offline HEAD 504s on its first
	// validation.  Lookups follow AliasOf one hop and serve from the
	// canonical entry.
	AliasOf string `json:"alias_of,omitempty"`
	// OriginPath, when set, records the user-facing request path that
	// originally triggered this download (e.g.
	// "/Qwen/Qwen2.5-0.5B/resolve/main/model.safetensors").  Set only on
	// content-addressed bodies (Xet / LFS CDN) where the cache key path
	// is just an opaque content hash and would otherwise lose all
	// model identity.  Threaded in via a __pulsys_origin query param on
	// rewritten cross-host Location URLs (stripped before forwarding to
	// upstream).  The admin UI uses this to attribute disk usage back
	// to the originating repo.
	//
	// Kept as a scalar for back-compat with on-disk entries written
	// before OriginPaths existed and with callers that read this
	// field directly (notably internal/proxy/xet_e2e_test).  New code
	// should consult OriginPaths via models.PrimaryAttribution /
	// models.OwnedBy so multi-owner sharing of Xet/LFS chunks is
	// handled correctly.
	OriginPath string `json:"origin_path,omitempty"`

	// OriginPaths is the full set of HF-style request paths that have
	// committed bytes against this cache key.  Xet/LFS chunks are
	// content-addressed, so multiple HF repos that share bytes (quant
	// variants, forks of a base model) legitimately collide on the
	// same cache key; each new writer appends its OriginPath here so
	// the admin "purge model" action can remove just the requested
	// owner and only delete the body when no owner remains.
	//
	// Bounded by MaxOriginPaths to keep meta.json from being
	// weaponised by an extreme-sharing scenario.  Entries are kept
	// sorted and deduped for deterministic on-disk output.
	OriginPaths []string `json:"origin_paths,omitempty"`
}

// MaxOriginPaths caps the number of distinct owners we track per
// shared cache body.  64 is an order of magnitude above plausible HF
// blob sharing (a base model + a handful of quant variants) and exists
// only to keep a malicious or pathological caller from growing
// meta.json without bound.  When the set is at capacity additional
// writers are silently dropped from the owner list; their bytes still
// serve, they just do not appear as owners and are not protected from
// a cascading purge of one of the recorded owners.
const MaxOriginPaths = 64

// appendOwner inserts owner into a sorted-deduped owners slice,
// capped at MaxOriginPaths.  Returns the (possibly unchanged) slice.
// Used by SegmentWriter.commit and .checkpoint to register each new
// writer as an owner of the cache body without touching the order
// of existing entries beyond what sort.SearchStrings + slices.Insert
// imply (deterministic for on-disk diffs).
func appendOwner(owners []string, owner string) []string {
	if owner == "" {
		return owners
	}
	i := sort.SearchStrings(owners, owner)
	if i < len(owners) && owners[i] == owner {
		return owners
	}
	if len(owners) >= MaxOriginPaths {
		// At capacity: refuse the insert rather than evict an older
		// owner.  An evict-oldest policy would let a noisy caller
		// erase legitimate owners and bypass shared-blob protection.
		return owners
	}
	owners = append(owners, "")
	copy(owners[i+1:], owners[i:])
	owners[i] = owner
	return owners
}

// IsCacheableRedirectStatus reports whether status is one of the 30x
// values that we persist (along with a Location) and replay verbatim on
// warm hits.  302 is the dominant case from huggingface.co's resolve
// endpoint; 301/303/307/308 are accepted for parity with arbitrary
// upstream behavior.  304 is intentionally excluded -- it depends on
// conditional headers and would require us to track validators per
// client, which is out of scope.
func IsCacheableRedirectStatus(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

// Store is the on-disk cache root.
//
// The store keeps a process-local in-memory copy of every committed *Meta in
// metaCache (key -> *Meta).  This eliminates an open(2) + read(2) +
// json.Unmarshal on every warm cache hit, which pprof showed to be the single
// biggest source of warm-path allocations.
//
// objectsPrefix is filepath.Join(root, "v1", "objects") + string(os.PathSeparator)
// computed once so bodyPath / metaPath / objectDir do not allocate via
// repeated filepath.Join calls on the warm hit path.
// MaxMetaCacheEntries caps the size of the in-memory *Meta LRU.  When the
// limit is exceeded, the least-recently-used entry is evicted (the on-disk
// meta.json remains; only the in-memory snapshot is dropped).  At ~1 KiB
// per Meta this caps RAM usage at ~10 MiB by default — a safe ceiling for
// a sidecar deployment.  Adjust before NewStore if a different bound is
// required.
var MaxMetaCacheEntries = 10000

// MaxBodyHandleEntries caps the number of open *os.File descriptors the
// store keeps for cached object bodies.  Eviction marks the handle as
// "closeOnLastRef" so any in-flight readers complete cleanly before the
// fd is released; subsequent AcquireBody re-opens the file.  Default
// (4096) sits well under typical RLIMIT_NOFILE values (10240 on Linux,
// 256 on macOS without ulimit -n bump) while still covering most
// realistic working sets.
var MaxBodyHandleEntries = 4096

type Store struct {
	root          string
	fsyncPolicy   string
	objectsPrefix string
	MaxBytes      int64 // 0 means unlimited

	mu          sync.Mutex // serializes meta writes per process
	keyLocks    sync.Map   // key -> *sync.Mutex (per-key singleflight lock; legacy, see AcquireRange)
	metaCache   *lru       // bounded LRU: key -> *Meta (atomic snapshot; never mutated in place)
	bodyHandles *lru       // bounded LRU: key -> *bodyHandle (see bodyhandle.go); evict marks for close-on-last-ref
	inflight    sync.Map   // key -> *inflightSet (in-flight upstream range coordination; see inflight.go)

	usedBytes     atomic.Int64 // logical bytes represented by committed meta totals
	reservedBytes atomic.Int64 // in-flight bytes admitted by quota but not yet committed
	entryCount    atomic.Int64 // count of meta.json entries warmed at startup and maintained on writes/purges
}

func NewStore(root, fsyncPolicy string) (*Store, error) {
	return NewStoreWithOptions(root, fsyncPolicy, StoreOptions{})
}

type StoreOptions struct {
	MaxBytes int64
}

func NewStoreWithOptions(root, fsyncPolicy string, opts StoreOptions) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "v1", "objects"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		root:          root,
		fsyncPolicy:   fsyncPolicy,
		objectsPrefix: filepath.Join(root, "v1", "objects") + string(os.PathSeparator),
		MaxBytes:      opts.MaxBytes,
	}
	s.metaCache = newLRU(MaxMetaCacheEntries, nil) // *Meta has no resource to clean up
	s.bodyHandles = newLRU(MaxBodyHandleEntries, evictBodyHandle)
	if s.MaxBytes > 0 {
		if err := s.warmUsageCounters(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// evictBodyHandle is called from the bodyHandles LRU when an entry is
// pushed out by capacity pressure.  We must not close the underlying
// *os.File here — there may still be active ReadAt callers holding the
// shared handle.  Instead we mark it as "evicted" so the last
// outstanding Close() call closes the fd; the next AcquireBody for this
// key re-opens the file.
func evictBodyHandle(_ string, val any) {
	bh, ok := val.(*bodyHandle)
	if !ok || bh == nil {
		return
	}
	bh.markEvicted()
}

type Stats struct {
	UsedBytes     int64
	QuotaBytes    int64
	FreeDiskBytes int64
	EntryCount    int
}

func (s *Store) Stats() Stats {
	if s.MaxBytes <= 0 {
		used, count, _ := s.scanUsageCounters()
		return Stats{
			UsedBytes:     used,
			QuotaBytes:    0,
			FreeDiskBytes: freeDiskBytes(s.root),
			EntryCount:    int(count),
		}
	}
	return Stats{
		UsedBytes:     s.usedBytes.Load(),
		QuotaBytes:    s.MaxBytes,
		FreeDiskBytes: freeDiskBytes(s.root),
		EntryCount:    int(s.entryCount.Load()),
	}
}

func freeDiskBytes(root string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func (s *Store) warmUsageCounters() error {
	used, count, err := s.scanUsageCounters()
	if err != nil {
		return err
	}
	s.usedBytes.Store(used)
	s.entryCount.Store(count)
	return nil
}

func (s *Store) scanUsageCounters() (int64, int64, error) {
	root := filepath.Join(s.root, "v1", "objects")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	var used int64
	var count int64
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, ent.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m Meta
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		count++
		used += metaTotal(&m)
	}
	return used, count, nil
}

func metaTotal(m *Meta) int64 {
	if m == nil || m.Total == nil || *m.Total <= 0 {
		return 0
	}
	return *m.Total
}

func (s *Store) reserveQuota(n int64) error {
	if s.MaxBytes <= 0 || n <= 0 {
		return nil
	}
	for {
		used := s.usedBytes.Load()
		reserved := s.reservedBytes.Load()
		if used >= s.MaxBytes || used+reserved+n > s.MaxBytes {
			return ErrQuotaExceeded
		}
		if s.reservedBytes.CompareAndSwap(reserved, reserved+n) {
			return nil
		}
	}
}

func (s *Store) releaseReservation(n int64) {
	if n <= 0 {
		return
	}
	s.reservedBytes.Add(-n)
}

func quotaReservationFor(p SegmentParams) int64 {
	if p.Total > 0 {
		return p.Total
	}
	if p.Length > 0 {
		return p.Length
	}
	return 0
}

func (s *Store) accountMetaTransition(oldTotal int64, oldExists bool, newMeta *Meta) int64 {
	if s.MaxBytes <= 0 {
		return 0
	}
	newTotal := metaTotal(newMeta)
	if newTotal != oldTotal {
		s.usedBytes.Add(newTotal - oldTotal)
	}
	if !oldExists && newMeta != nil {
		s.entryCount.Add(1)
	}
	if newTotal > oldTotal {
		return newTotal - oldTotal
	}
	return 0
}

// Lock acquires a per-key mutex for singleflight-style serialization on
// concurrent misses for the same cache key.  The returned function MUST be
// called once to release the lock (typically via defer or in a stream Close).
func (s *Store) Lock(key string) func() {
	v, _ := s.keyLocks.LoadOrStore(key, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// Path helpers.  These avoid filepath.Join on the hot path: with
// objectsPrefix already containing the trailing separator, a single string
// concatenation is enough and produces exactly one allocation per call (for
// the result string), versus filepath.Join's strings.Builder + Clean dance
// that pprof clocked at ~1.7% of allocs.
func (s *Store) objectDir(key string) string { return s.objectsPrefix + key }
func (s *Store) metaPath(key string) string {
	return s.objectsPrefix + key + string(os.PathSeparator) + "meta.json"
}
func (s *Store) bodyPath(key string) string {
	return s.objectsPrefix + key + string(os.PathSeparator) + "body"
}

// LoadMeta returns the cached *Meta for key, or reads + parses meta.json on a
// process-cold key.  The returned *Meta MUST be treated as read-only; callers
// requiring mutation must clone it (commit does this internally).  Returns
// (nil, nil) when the object is not in the cache and the on-disk meta.json
// does not exist.
func (s *Store) LoadMeta(key string) (*Meta, error) {
	if v, ok := s.metaCache.Get(key); ok {
		return v.(*Meta), nil
	}
	b, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	s.metaCache.Add(key, &m)
	return &m, nil
}

// OpenBody returns the body file for reading.
func (s *Store) OpenBody(key string) (*os.File, error) {
	return os.Open(s.bodyPath(key))
}

// SegmentParams describes one streaming write into the cache body file.
//
// Status: 200 means contiguous full body (start=0, total = ContentLength when
// known, otherwise unknown until EOF).  Status 206 means a single byte range
// [Start, Start+Length).  Total may be -1 when upstream returns "*" total.
type SegmentParams struct {
	Status       int
	UpstreamHost string
	Path         string
	RawQuery     string
	ETag         string
	ContentType  string
	Start        int64 // absolute offset into body file
	Length       int64 // expected bytes (-1 if unknown / chunked)
	Total        int64 // total artifact size (-1 if unknown)
	// ExtraHeaders captures upstream response headers that downstream
	// clients treat as authoritative metadata (e.g. X-Linked-Etag,
	// X-Linked-Size, X-Repo-Commit on /resolve HEAD/GET).  Persisted to
	// Meta.ExtraHeaders so warm/offline serves can replay them; without
	// this, huggingface_hub's HEAD validation raises
	// LocalEntryNotFoundError on warm reads.
	ExtraHeaders map[string]string
	// OriginPath is the user-facing request path that triggered this
	// download (only set for content-addressed bodies; see
	// Meta.OriginPath docs).
	OriginPath string
}

// BeginSegment opens a writer at SegmentParams.Start in the body file.  When
// the returned writer is Closed, meta.json is updated to reflect the bytes
// actually written (so partial transfers persist what was already pulled).
//
// Inputs are validated up front so a malformed SegmentParams (e.g. from a
// future ColdBackend population path) fails before any filesystem I/O.  The
// validation rules:
//
//   - key must be non-empty (callers should pass KeyHex output)
//   - Start must be >= 0 (negative seek offsets corrupt the body file)
//   - Length must be -1 (unknown) or >= 0
//   - Total  must be -1 (unknown) or >= 0
//   - Status must be 200 or 206 (the only span-bearing statuses;
//     30x redirects go through StoreRedirect, not BeginSegment)
func (s *Store) BeginSegment(key string, p SegmentParams) (*SegmentWriter, error) {
	if err := validateSegmentParams(key, p); err != nil {
		return nil, err
	}
	reserved := quotaReservationFor(p)
	if err := s.reserveQuota(reserved); err != nil {
		return nil, err
	}
	dir := s.objectDir(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.releaseReservation(reserved)
		return nil, err
	}
	bodyPath := s.bodyPath(key)
	f, err := os.OpenFile(bodyPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		s.releaseReservation(reserved)
		return nil, err
	}
	if p.Total > 0 {
		if err := f.Truncate(p.Total); err != nil {
			_ = f.Close()
			s.releaseReservation(reserved)
			return nil, err
		}
	}
	if _, err := f.Seek(p.Start, io.SeekStart); err != nil {
		_ = f.Close()
		s.releaseReservation(reserved)
		return nil, err
	}
	return &SegmentWriter{
		store:        s,
		key:          key,
		f:            f,
		params:       p,
		reserved:     reserved,
		dynamicQuota: reserved == 0 && s.MaxBytes > 0 && p.Total <= 0 && p.Length <= 0,
	}, nil
}

// validateSegmentParams enforces the BeginSegment input contract.
// It is exported via BeginSegment's behavior rather than as a
// public function so cold-tier population paths get the same
// guarantees as the proxy hot path.
func validateSegmentParams(key string, p SegmentParams) error {
	if key == "" {
		return fmt.Errorf("BeginSegment: empty key")
	}
	if p.Start < 0 {
		return fmt.Errorf("BeginSegment: negative Start %d", p.Start)
	}
	if p.Length < -1 {
		return fmt.Errorf("BeginSegment: invalid Length %d (must be -1 or >= 0)", p.Length)
	}
	if p.Total < -1 {
		return fmt.Errorf("BeginSegment: invalid Total %d (must be -1 or >= 0)", p.Total)
	}
	switch p.Status {
	case 200, 206:
		// ok
	default:
		return fmt.Errorf("BeginSegment: status %d not allowed (only 200 or 206; redirects use StoreRedirect)", p.Status)
	}
	return nil
}

// CheckpointInterval controls how many bytes a SegmentWriter buffers
// before publishing a partial meta.json snapshot to disk.  This bounds
// the work lost on a sidecar restart mid-download: at most
// CheckpointInterval bytes need to be re-fetched from upstream after a
// crash.
//
// The value is a deliberate trade-off:
//   - smaller -> tighter resume window, more meta.json writes per request
//   - larger  -> fewer writes, more bytes potentially re-fetched on crash
//
// 16 MiB matches typical HF LFS chunk sizes and gives ~250 ms of
// upstream loss at a 64 MiB/s download, which is below human perception.
// Set to 0 (or negative) to disable checkpointing entirely (legacy
// behavior: meta.json only updates on Close).
var CheckpointInterval int64 = 16 * 1024 * 1024

// SegmentWriter is an io.WriteCloser that tees upstream bytes to disk and
// finalizes meta.json on Close (success or partial).
//
// During long writes it also publishes a partial meta.json snapshot every
// CheckpointInterval bytes so that a sidecar restart mid-download only
// loses up to CheckpointInterval bytes of upstream traffic.  See the
// commit / checkpoint methods for details.
type SegmentWriter struct {
	store          *Store
	key            string
	f              *os.File
	params         SegmentParams
	written        int64
	lastCheckpoint int64 // value of `written` at the last checkpoint
	closed         bool
	reserved       int64
	dynamicQuota   bool
}

func (w *SegmentWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if w.dynamicQuota {
		if err := w.store.reserveQuota(int64(len(p))); err != nil {
			return 0, err
		}
		w.reserved += int64(len(p))
	}
	n, err := w.f.Write(p)
	if w.dynamicQuota && n < len(p) {
		w.releaseReserved(int64(len(p) - n))
	}
	if n > 0 {
		w.written += int64(n)
		telemetry.AddDiskBytesWritten(int64(n))
	}
	if err == nil && CheckpointInterval > 0 && w.written-w.lastCheckpoint >= CheckpointInterval {
		// Best-effort: failure to checkpoint must not abort the stream.
		// The next checkpoint or Close() will retry; in the worst case
		// the resume window grows by one CheckpointInterval.
		_ = w.checkpoint()
	}
	return n, err
}

// checkpoint publishes a partial meta.json snapshot reflecting the bytes
// written so far.  Called periodically by Write so a crash mid-download
// loses at most CheckpointInterval bytes of upstream work.
//
// We deliberately do NOT fsync the body file here even when fsyncPolicy
// is "per-file" — that would dwarf the throughput.  We rely on the
// kernel page cache survivor semantics (a clean process kill keeps page
// cache; only a host crash loses unsynced pages).  Sidecar restart
// (the common case) is fully covered.
func (w *SegmentWriter) checkpoint() error {
	if w.written == w.lastCheckpoint {
		return nil
	}
	w.store.mu.Lock()
	meta, _ := w.store.readMetaLocked(w.key)
	oldExists := meta != nil
	oldTotal := metaTotal(meta)
	if meta == nil {
		meta = &Meta{
			Version:      1,
			UpstreamHost: w.params.UpstreamHost,
			Path:         w.params.Path,
			RawQuery:     w.params.RawQuery,
			ETag:         w.params.ETag,
			ContentType:  w.params.ContentType,
			StatusCode:   w.params.Status,
			OriginPath:   w.params.OriginPath,
		}
	}
	if meta.OriginPath == "" && w.params.OriginPath != "" {
		meta.OriginPath = w.params.OriginPath
	}
	if w.params.OriginPath != "" {
		meta.OriginPaths = appendOwner(meta.OriginPaths, w.params.OriginPath)
	}
	end := w.params.Start + w.written
	meta.Spans = MergeSpans(append(meta.Spans, Span{Start: w.params.Start, End: end}))
	if w.params.Total > 0 {
		t := w.params.Total
		meta.Total = &t
	}
	mergeExtraHeaders(meta, w.params.ExtraHeaders)
	if err := w.store.writeMetaLocked(w.key, meta); err != nil {
		w.store.mu.Unlock()
		return err
	}
	w.store.metaCache.Add(w.key, meta)
	accounted := w.store.accountMetaTransition(oldTotal, oldExists, meta)
	w.releaseReserved(accounted)
	w.lastCheckpoint = w.written
	w.store.mu.Unlock()
	telemetry.IncCacheCheckpoint()
	return nil
}

// mergeExtraHeaders adds new ExtraHeaders entries into meta without
// dropping ones that were persisted by an earlier write (e.g. on a
// follow-up range fetch where the upstream omitted the linked-etag
// headers it had sent on the initial 200).
func mergeExtraHeaders(meta *Meta, extra map[string]string) {
	if len(extra) == 0 {
		return
	}
	if meta.ExtraHeaders == nil {
		meta.ExtraHeaders = make(map[string]string, len(extra))
	}
	for k, v := range extra {
		if v == "" {
			continue
		}
		meta.ExtraHeaders[k] = v
	}
}

// Bytes written so far (for accounting in tests).
func (w *SegmentWriter) Written() int64 { return w.written }

// Close flushes the file, then updates meta.json with the span actually
// written.  Safe to call multiple times.  When the segment was a 200 with
// known length (and we received it all), Total is set.  For partial writes
// the recorded span is [Start, Start+written), and Total is left untouched
// unless the segment params already specified a positive Total.
func (w *SegmentWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer w.releaseReserved(w.reserved)

	syncErr := w.maybeFsync()
	closeErr := w.f.Close()

	if err := w.commit(); err != nil {
		if syncErr == nil {
			syncErr = err
		}
	}
	if closeErr != nil {
		return closeErr
	}
	return syncErr
}

func (w *SegmentWriter) maybeFsync() error {
	if w.store.fsyncPolicy == "per-file" {
		return w.f.Sync()
	}
	return nil
}

func (w *SegmentWriter) releaseReserved(n int64) {
	if n <= 0 || w.reserved <= 0 {
		return
	}
	if n > w.reserved {
		n = w.reserved
	}
	w.store.releaseReservation(n)
	w.reserved -= n
}

func (w *SegmentWriter) commit() error {
	w.store.mu.Lock()
	defer w.store.mu.Unlock()

	meta, _ := w.store.readMetaLocked(w.key)
	oldExists := meta != nil
	oldTotal := metaTotal(meta)
	if meta == nil {
		meta = &Meta{
			Version:      1,
			UpstreamHost: w.params.UpstreamHost,
			Path:         w.params.Path,
			RawQuery:     w.params.RawQuery,
			ETag:         w.params.ETag,
			ContentType:  w.params.ContentType,
			StatusCode:   w.params.Status,
			OriginPath:   w.params.OriginPath,
		}
	}
	if meta.OriginPath == "" && w.params.OriginPath != "" {
		meta.OriginPath = w.params.OriginPath
	}
	if w.params.OriginPath != "" {
		meta.OriginPaths = appendOwner(meta.OriginPaths, w.params.OriginPath)
	}
	end := w.params.Start + w.written
	if w.written > 0 {
		meta.Spans = MergeSpans(append(meta.Spans, Span{Start: w.params.Start, End: end}))
	}
	if w.params.Total > 0 {
		t := w.params.Total
		meta.Total = &t
	} else if w.params.Status == 200 && w.params.Start == 0 && w.params.Length < 0 && w.written > 0 {
		// Chunked / unknown-length 200 fully consumed: total is what we wrote.
		t := w.written
		meta.Total = &t
	}
	if w.params.Status != 0 {
		// Prefer 200 over 206 once we have a complete body.
		if meta.StatusCode == 0 || (w.params.Status == 200 && meta.StatusCode == 206) {
			meta.StatusCode = w.params.Status
		}
	}
	if w.params.ETag != "" {
		meta.ETag = w.params.ETag
	}
	if w.params.ContentType != "" {
		meta.ContentType = w.params.ContentType
	}
	if meta.UpstreamHost == "" {
		meta.UpstreamHost = w.params.UpstreamHost
	}
	if meta.Path == "" {
		meta.Path = w.params.Path
	}
	if meta.RawQuery == "" {
		meta.RawQuery = w.params.RawQuery
	}
	mergeExtraHeaders(meta, w.params.ExtraHeaders)

	if err := w.store.writeMetaLocked(w.key, meta); err != nil {
		return err
	}
	if err := w.store.maybeFsyncDir(w.store.objectDir(w.key)); err != nil {
		return err
	}
	w.store.metaCache.Add(w.key, meta)
	accounted := w.store.accountMetaTransition(oldTotal, oldExists, meta)
	w.releaseReserved(accounted)
	return nil
}

func (s *Store) readMetaLocked(key string) (*Meta, error) {
	b, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) writeMetaLocked(key string, m *Meta) error {
	if err := os.MkdirAll(s.objectDir(key), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(s.objectDir(key), "meta.json.tmp")
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.metaPath(key))
}

func (s *Store) maybeFsyncDir(dir string) error {
	if s.fsyncPolicy != "dir" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	_ = d.Close()
	return err
}

// ParseSingleRange parses "bytes=start-end" or "bytes=start-" into [start,end)
// (half-open).  end may be -1 if total unknown.
func ParseSingleRange(v string, total int64) (start, end int64, ok bool) {
	v = strings.TrimSpace(v)
	const pfx = "bytes="
	if !strings.HasPrefix(strings.ToLower(v), pfx) {
		return 0, 0, false
	}
	v = v[len(pfx):]
	parts := strings.SplitN(v, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err1 != nil {
		return 0, 0, false
	}
	endStr := strings.TrimSpace(parts[1])
	if endStr == "" {
		if total <= 0 {
			return start, -1, true
		}
		return start, total, true
	}
	end, err2 := strconv.ParseInt(endStr, 10, 64)
	if err2 != nil {
		return 0, 0, false
	}
	return start, end + 1, true
}

// StoreRedirect persists a cached 30x response keyed by `key`.  No body
// file is created; only meta.json is written, recording the status code
// and the upstream (pre-rewrite) Location.  On warm hits the handler
// replays the status verbatim and re-rewrites Location through the
// current PublicBaseURL.
//
// Idempotent: calling repeatedly with the same key + Location is a no-op
// once meta.json exists.  A different Location for the same key updates
// the cached value (HF can rotate presigned URLs on the upstream side
// across our presign-cache TTL; we want the freshest one).
// StoreAlias writes a meta-only cache entry at `aliasKey` whose only
// purpose is to point at `canonicalKey`.  Lookups follow `Meta.AliasOf`
// one hop and serve from the canonical entry's meta + body.  Idempotent:
// if `aliasKey` already has a real (non-alias) meta we keep the existing
// one, on the principle that a concrete cached entry always wins over
// an alias.
func (s *Store) StoreAlias(aliasKey, canonicalKey, host, path, rawQuery string) error {
	if aliasKey == "" || canonicalKey == "" {
		return fmt.Errorf("StoreAlias: empty key")
	}
	if aliasKey == canonicalKey {
		return nil
	}
	if err := os.MkdirAll(s.objectDir(aliasKey), 0o755); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, _ := s.readMetaLocked(aliasKey)
	if existing != nil && existing.AliasOf == "" {
		// Already a concrete entry under aliasKey; do not clobber.
		return nil
	}
	meta := &Meta{
		Version:      1,
		UpstreamHost: host,
		Path:         path,
		RawQuery:     rawQuery,
		AliasOf:      canonicalKey,
	}
	if err := s.writeMetaLocked(aliasKey, meta); err != nil {
		return err
	}
	if err := s.maybeFsyncDir(s.objectDir(aliasKey)); err != nil {
		return err
	}
	s.metaCache.Add(aliasKey, meta)
	s.accountMetaTransition(metaTotal(existing), existing != nil, meta)
	return nil
}

func (s *Store) StoreRedirect(key string, status int, host, path, rawQuery, location, contentType, etag string, extraHeaders map[string]string) error {
	if !IsCacheableRedirectStatus(status) {
		return fmt.Errorf("status %d is not a cacheable redirect", status)
	}
	if location == "" {
		return fmt.Errorf("StoreRedirect: empty Location")
	}
	if err := os.MkdirAll(s.objectDir(key), 0o755); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, _ := s.readMetaLocked(key)
	oldExists := meta != nil
	oldTotal := metaTotal(meta)
	if meta == nil {
		meta = &Meta{Version: 1}
	}
	meta.UpstreamHost = host
	meta.Path = path
	meta.RawQuery = rawQuery
	meta.StatusCode = status
	meta.Location = location
	if contentType != "" {
		meta.ContentType = contentType
	}
	if etag != "" {
		meta.ETag = etag
	}
	mergeExtraHeaders(meta, extraHeaders)
	if err := s.writeMetaLocked(key, meta); err != nil {
		return err
	}
	if err := s.maybeFsyncDir(s.objectDir(key)); err != nil {
		return err
	}
	s.metaCache.Add(key, meta)
	s.accountMetaTransition(oldTotal, oldExists, meta)
	return nil
}

// Compatibility-only shims for existing tests; new code uses BeginSegment.
//
// WriteFullFromStream writes the entire body to the cache (200-style) by
// streaming via BeginSegment and io.Copy.
func (s *Store) WriteFullFromStream(key string, status int, host, path, rawQuery, etag, ct string, body io.Reader, totalHint int64) (*Meta, error) {
	if totalHint < 0 {
		totalHint = -1
	}
	w, err := s.BeginSegment(key, SegmentParams{
		Status: status, UpstreamHost: host, Path: path, RawQuery: rawQuery,
		ETag: etag, ContentType: ct,
		Start: 0, Length: totalHint, Total: totalHint,
	})
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(w, body); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return s.LoadMeta(key)
}

// WriteRangeFromStream writes one [start,end) range into the sparse body.
func (s *Store) WriteRangeFromStream(key string, start, end int64, total *int64, host, path, rawQuery, etag, ct string, body io.Reader) (*Meta, error) {
	if start < 0 || end <= start {
		return nil, fmt.Errorf("bad range %d-%d", start, end)
	}
	t := int64(-1)
	if total != nil {
		t = *total
	}
	w, err := s.BeginSegment(key, SegmentParams{
		Status: 206, UpstreamHost: host, Path: path, RawQuery: rawQuery,
		ETag: etag, ContentType: ct,
		Start: start, Length: end - start, Total: t,
	})
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(w, body); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return s.LoadMeta(key)
}

// PurgeDecision tells PurgeOrTrim what to do with one scanned entry.
// Trim is the affordance that makes shared Xet/LFS bodies safe: when
// one of several owners purges the model, we rewrite meta to remove
// that owner from the set and keep the body in place for the other
// owners.  Remove deletes the body outright (the all-owners-gone
// case); Keep is a no-op.
type PurgeDecision int

const (
	DecisionKeep PurgeDecision = iota
	DecisionTrim
	DecisionRemove
)

// PurgeResult collects what PurgeOrTrim did across the scan.  Purged
// is "object dirs removed from disk"; Trimmed is "meta.json rewritten
// with at least one owner removed"; BytesFreed sums *Meta.Total for
// removed entries only (trims do not free disk).
type PurgeResult struct {
	Purged     int
	Trimmed    int
	BytesFreed int64
}

// PurgeOrTrim walks every on-disk cache object and invokes decide for
// each readable meta.json.  decide returns the action to take and,
// for DecisionTrim, the new *Meta to persist (must be non-nil and
// have the same key identity).  Corrupt or missing meta.json entries
// are skipped without failing the whole walk; an IO error during the
// terminal action surfaces immediately so the operator sees a partial
// result rather than silent corruption.
func (s *Store) PurgeOrTrim(decide func(key string, m *Meta) (PurgeDecision, *Meta)) (PurgeResult, error) {
	if decide == nil {
		return PurgeResult{}, fmt.Errorf("PurgeOrTrim: nil decide func")
	}
	root := filepath.Join(s.root, "v1", "objects")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return PurgeResult{}, nil
		}
		return PurgeResult{}, err
	}

	var res PurgeResult
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		key := ent.Name()
		metaPath := filepath.Join(root, key, "meta.json")
		b, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var m Meta
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		decision, trimmed := decide(key, &m)
		switch decision {
		case DecisionKeep:
			continue
		case DecisionTrim:
			if trimmed == nil {
				return res, fmt.Errorf("PurgeOrTrim: nil meta returned for DecisionTrim on key %q", key)
			}
			s.mu.Lock()
			if err := s.writeMetaLocked(key, trimmed); err != nil {
				s.mu.Unlock()
				return res, err
			}
			s.metaCache.Add(key, trimmed)
			s.mu.Unlock()
			res.Trimmed++
		case DecisionRemove:
			var freed int64
			if m.Total != nil {
				freed = *m.Total
				res.BytesFreed += freed
			}
			dir := s.objectDir(key)
			if err := os.RemoveAll(dir); err != nil {
				return res, err
			}
			if s.MaxBytes > 0 && freed > 0 {
				s.usedBytes.Add(-freed)
			}
			if s.MaxBytes > 0 {
				s.entryCount.Add(-1)
			}
			s.metaCache.Delete(key)
			s.bodyHandles.Delete(key)
			s.inflight.Delete(key)
			s.keyLocks.Delete(key)
			res.Purged++
		default:
			return res, fmt.Errorf("PurgeOrTrim: unknown decision %d for key %q", decision, key)
		}
	}
	return res, nil
}

// PurgeKeys is the legacy "delete everything matching pred" entry
// point retained for callers that do not care about trimming shared
// bodies.  Implemented on top of PurgeOrTrim so the scan + LRU
// invalidation logic lives in one place.
//
// Returns (purgedCount, totalBytesFreed, error).  Corrupt or missing
// meta.json entries are skipped without failing the whole purge.
func (s *Store) PurgeKeys(pred func(key string, m *Meta) bool) (int, int64, error) {
	if pred == nil {
		return 0, 0, fmt.Errorf("PurgeKeys: nil predicate")
	}
	res, err := s.PurgeOrTrim(func(key string, m *Meta) (PurgeDecision, *Meta) {
		if pred(key, m) {
			return DecisionRemove, nil
		}
		return DecisionKeep, nil
	})
	return res.Purged, res.BytesFreed, err
}
