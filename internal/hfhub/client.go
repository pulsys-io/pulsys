// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package hfhub implements a minimal Hugging Face Hub HTTP client tuned for
// high-throughput downloads through pulsys (HF_ENDPOINT).
package hfhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultRangeChunk = 16 << 20  // 16 MiB — matches hf-cli large-chunk default
	copyBufSize       = 256 << 10 // 256 KiB — fewer syscall/loop iterations than the io.Copy default of 32 KiB
)

// copyBufPool returns reusable 256 KiB scratch buffers for the
// resp.Body → sink/file copy.  The default io.Copy heap-allocates a 32 KiB
// buffer per call, which on a 911-range Qwen-7B download accounts for
// ~30 MiB of allocations on the hot path; pooling drops that to zero
// steady-state.
var copyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, copyBufSize)
		return &b
	},
}

// Client talks to a Hub-compatible endpoint (huggingface.co or pulsys).
type Client struct {
	Base  string // HF_ENDPOINT without trailing slash
	Token string
	HTTP  *http.Client

	// MaxRetries controls how many times fetchTask + ListFiles re-issue
	// after a 429 / 5xx / network-reset.  Zero means default (5).
	MaxRetries int
	// RetryBaseDelay is the first backoff duration; each subsequent
	// retry doubles (capped at RetryMaxDelay).  Zero means default
	// (200ms / 5s).
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
}

// NewClient reads HF_ENDPOINT and HF_TOKEN from the environment and returns
// a Client with a tuned *http.Transport sized for pulsys on loopback.
//
// The default Go http.Transport (DefaultTransport) is fine for a few
// dozen requests, but for hundreds of parallel range GETs it
// (1) opens new connections instead of reusing the idle pool (default
//
//	MaxIdleConnsPerHost = 2), and (2) negotiates HTTP/2 against any
//
// server that advertises it, which gives unpredictable range behavior
// against pulsys.  Both are fixed here.
func NewClient() *Client {
	base := strings.TrimRight(os.Getenv("HF_ENDPOINT"), "/")
	if base == "" {
		base = "https://huggingface.co"
	}
	c := &Client{
		Base:  base,
		Token: strings.TrimSpace(os.Getenv("HF_TOKEN")),
	}
	c.HTTP = &http.Client{
		Transport: NewTransport(256),
		Timeout:   0, // per-request context deadlines instead
	}
	return c
}

// NewTransport builds the *http.Transport the parallel download workers share.
//
// The knobs that matter for warm-cache loopback throughput:
//   - DisableCompression: pulsys serves raw bodies; gzip on Accept-
//     Encoding round-trips would be a pure waste.
//   - MaxIdleConnsPerHost / MaxConnsPerHost = workers: keep one warm
//     TCP connection per goroutine so range GETs don't pay a connect+
//     TLS handshake on every fetchTask.
//   - ForceAttemptHTTP2 = false: HTTP/2 multiplexes range requests over
//     a single TCP stream which limits parallelism to whatever the
//     server allows (and changes Range semantics).  pulsys's io_uring
//     path is HTTP/1.1; we explicitly stay there.
//   - IdleConnTimeout: keep connections around for the lifetime of a
//     typical multi-GB download, but recycle if idle >5min.
//   - ResponseHeaderTimeout = 0 (NO transport-level header timeout).
//     Every range issues a GET that the proxy resolves via a 302 hop
//     to huggingface.co; under 192-way concurrency some of those hops
//     can queue behind upstream rate-limiting for tens of seconds.
//     A fixed transport timeout would deadline-kill innocent in-flight
//     requests and then retry the same load against the same upstream
//     -- death spiral.  Per-request bounds belong on the caller's
//     context.Context (e.g. --timeout flag), not on the transport.
func NewTransport(workers int) *http.Transport {
	if workers <= 0 {
		workers = 32
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		DisableCompression:    true,
		MaxIdleConns:          workers * 2,
		MaxIdleConnsPerHost:   workers,
		MaxConnsPerHost:       workers,
		IdleConnTimeout:       5 * time.Minute,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// ResponseHeaderTimeout: 0 -- intentional; see doc above.
	}
}

// FileEntry is one artifact in a model repo.
type FileEntry struct {
	Path string
	Size int64
}

type treeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// RepoType selects the Hub API path prefix.
type RepoType string

const (
	RepoTypeModel   RepoType = "model"
	RepoTypeDataset RepoType = "dataset"
	RepoTypeSpace   RepoType = "space"
)

func (rt RepoType) apiPrefix() string {
	switch rt {
	case RepoTypeDataset:
		return "datasets"
	case RepoTypeSpace:
		return "spaces"
	default:
		return "models"
	}
}

// resolvePrefix returns the segment between Base and "{repo}/resolve/...".
// Models live at root ("/{repo}/resolve/..."); datasets/spaces are namespaced.
func (rt RepoType) resolvePrefix() string {
	switch rt {
	case RepoTypeDataset:
		return "datasets/"
	case RepoTypeSpace:
		return "spaces/"
	default:
		return ""
	}
}

// ListFiles returns all files in repo at revision via the recursive tree API.
func (c *Client) ListFiles(ctx context.Context, repo, revision string) ([]FileEntry, error) {
	return c.ListFilesTyped(ctx, repo, revision, RepoTypeModel)
}

// ListFilesTyped is ListFiles with a configurable repo type.
func (c *Client) ListFilesTyped(ctx context.Context, repo, revision string, repoType RepoType) ([]FileEntry, error) {
	if revision == "" {
		revision = "main"
	}
	u := fmt.Sprintf("%s/api/%s/%s/tree/%s?recursive=1",
		c.Base, repoType.apiPrefix(), escapePath(repo), url.PathEscape(revision))

	var entries []treeEntry
	err := c.doRetry(ctx, "tree "+repo, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		c.setAuth(req)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return &httpError{
				op:     "tree",
				status: resp.StatusCode,
				body:   strings.TrimSpace(string(b)),
			}
		}
		entries = entries[:0]
		return json.NewDecoder(resp.Body).Decode(&entries)
	})
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		if e.Type != "file" || e.Path == "" {
			continue
		}
		out = append(out, FileEntry{Path: e.Path, Size: e.Size})
	}
	return out, nil
}

// warmCommitPinnedTree prefetches /tree/<commit-sha> when revision is
// symbolic (main).  huggingface_hub re-issues tree requests pinned to
// X-Repo-Commit on repeat downloads; import must warm that slot too.
func (c *Client) warmCommitPinnedTree(ctx context.Context, repo string, opts DownloadOpts, files []FileEntry) {
	if !symbolicRevision(opts.Revision) || len(files) == 0 {
		return
	}
	sha, err := c.commitSHAForFile(ctx, repo, opts.Revision, files[0].Path, opts.RepoType)
	if err != nil || sha == "" || sha == opts.Revision {
		return
	}
	_, _ = c.ListFilesTyped(ctx, repo, sha, opts.RepoType)
}

func symbolicRevision(rev string) bool {
	rev = strings.TrimSpace(rev)
	if rev == "" || rev == "main" || rev == "master" || rev == "HEAD" {
		return true
	}
	return !looksLikeCommitSHA(rev)
}

func looksLikeCommitSHA(rev string) bool {
	if len(rev) != 40 {
		return false
	}
	for _, c := range rev {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

func (c *Client) commitSHAForFile(ctx context.Context, repo, revision, filePath string, repoType RepoType) (string, error) {
	u := c.resolveURL(repo, revision, filePath, repoType)
	var sha string
	err := c.doRetry(ctx, "commit-sha "+repo, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
		if err != nil {
			return err
		}
		c.setAuth(req)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return &httpError{
				op:     "commit-sha",
				status: resp.StatusCode,
				body:   strings.TrimSpace(string(b)),
			}
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		sha = strings.TrimSpace(resp.Header.Get("X-Repo-Commit"))
		if sha == "" {
			return errors.New("missing X-Repo-Commit")
		}
		return nil
	})
	return sha, err
}

type fetchTask struct {
	url   string
	state *fileState // resolved output for this task; nil when Sink mode
	path  string
	start int64
	end   int64 // inclusive end for Range header
	size  int64
}

// DownloadOpts configures a snapshot download.
type DownloadOpts struct {
	LocalDir   string
	Sink       io.Writer // if non-nil, all bytes go here (bench /dev/null)
	Workers    int
	Revision   string
	MinBytes   int64 // skip tiny files when saturating (0 = download all)
	RangeChunk int64
	// Progress is called with cumulative downloaded bytes and total planned
	// bytes. It is invoked once with done=0 before workers launch, and then
	// whenever a range task completes successfully.
	//
	// Callbacks may run concurrently from worker goroutines.
	Progress func(done, total int64)

	// RepoType selects the API path prefix; empty defaults to "model".
	RepoType RepoType

	// Files restricts the download to these exact relative paths
	// (after stripping leading "/").  Empty = entire snapshot.
	Files []string

	// Include / Exclude are glob patterns evaluated against each tree
	// entry's relative path with filepath.Match.  Include filters in;
	// Exclude filters out (Exclude wins if both match).  Empty Include
	// means "match everything"; empty Exclude means "exclude nothing".
	Include []string
	Exclude []string
}

// Download fetches every file (or those >= MinBytes) with parallel range I/O.
func (c *Client) Download(ctx context.Context, repo string, opts DownloadOpts) (totalBytes int64, err error) {
	if opts.Revision == "" {
		opts.Revision = "main"
	}
	if opts.Workers <= 0 {
		opts.Workers = 32
	}
	if opts.RangeChunk <= 0 {
		opts.RangeChunk = defaultRangeChunk
	}
	if opts.RepoType == "" {
		opts.RepoType = RepoTypeModel
	}

	files, err := c.ListFilesTyped(ctx, repo, opts.Revision, opts.RepoType)
	if err != nil {
		return 0, err
	}
	c.warmCommitPinnedTree(ctx, repo, opts, files)
	files, err = filterFiles(files, opts)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no files match selection (Include=%v Exclude=%v Files=%v)",
			opts.Include, opts.Exclude, opts.Files)
	}

	// fileStates: one *os.File per relative path, opened once and shared
	// across every range task that targets that file.  Without this the
	// old code path called os.OpenFile + Seek + Close per range, which on
	// Qwen-7B (911 ranges) issued ~1800 redundant open/close syscalls.
	pool := &filePool{}
	if opts.Sink == nil && opts.LocalDir != "" {
		if err := os.MkdirAll(opts.LocalDir, 0o755); err != nil {
			return 0, err
		}
	}
	defer pool.closeAll()

	tasks := make([]fetchTask, 0, len(files))
	var plannedBytes int64
	for _, f := range files {
		if opts.MinBytes > 0 && f.Size < opts.MinBytes {
			continue
		}
		plannedBytes += f.Size
		u := c.resolveURL(repo, opts.Revision, f.Path, opts.RepoType)
		var st *fileState
		if opts.Sink == nil && opts.LocalDir != "" {
			st, err = pool.prepare(opts.LocalDir, f.Path, f.Size)
			if err != nil {
				return 0, err
			}
		}
		if f.Size <= opts.RangeChunk {
			tasks = append(tasks, fetchTask{url: u, state: st, path: f.Path, size: f.Size, start: 0, end: f.Size - 1})
			continue
		}
		for off := int64(0); off < f.Size; {
			end := off + opts.RangeChunk - 1
			if end >= f.Size {
				end = f.Size - 1
			}
			tasks = append(tasks, fetchTask{url: u, state: st, path: f.Path, start: off, end: end, size: f.Size})
			off = end + 1
		}
	}
	if len(tasks) == 0 {
		return 0, fmt.Errorf("no files to download")
	}
	if opts.Progress != nil {
		opts.Progress(0, plannedBytes)
	}

	sem := make(chan struct{}, opts.Workers)
	var wg sync.WaitGroup
	var firstErr atomic.Value
	var nbytes atomic.Int64

	for _, t := range tasks {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(t fetchTask) {
			defer wg.Done()
			defer func() { <-sem }()
			n, err := c.fetchTask(ctx, repo, opts, t)
			if err != nil {
				if firstErr.Load() == nil {
					firstErr.Store(err)
				}
				return
			}
			done := nbytes.Add(n)
			if opts.Progress != nil {
				opts.Progress(done, plannedBytes)
			}
		}(t)
	}
	wg.Wait()
	if v := firstErr.Load(); v != nil {
		return nbytes.Load(), v.(error)
	}
	return nbytes.Load(), ctx.Err()
}

func (c *Client) fetchTask(ctx context.Context, _ string, opts DownloadOpts, t fetchTask) (int64, error) {
	var total int64
	err := c.doRetry(ctx, "GET "+t.path, func(ctx context.Context) error {
		n, err := c.fetchOnce(ctx, opts, t)
		total = n
		return err
	})
	return total, err
}

func (c *Client) fetchOnce(ctx context.Context, opts DownloadOpts, t fetchTask) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url, nil)
	if err != nil {
		return 0, err
	}
	c.setAuth(req)
	if t.end >= t.start && (t.end+1-t.start) < t.size {
		// strconv.AppendInt avoids the fmt.Sprintf allocation that
		// otherwise shows up once per range task (~900 allocs on a
		// Qwen-7B download).  buf is stack-allocated so this whole
		// branch is alloc-free.
		var buf [40]byte
		out := append(buf[:0], "bytes="...)
		out = strconv.AppendInt(out, t.start, 10)
		out = append(out, '-')
		out = strconv.AppendInt(out, t.end, 10)
		req.Header.Set("Range", string(out))
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, &httpError{
			op:     "GET " + t.path,
			status: resp.StatusCode,
			body:   strings.TrimSpace(string(b)),
		}
	}

	// Path A: --sink (no disk, no file pool).  Stream straight through
	// the pooled CopyBuffer.
	if opts.Sink != nil {
		bufp := copyBufPool.Get().(*[]byte)
		n, err := io.CopyBuffer(writerOnly{opts.Sink}, readerOnly{resp.Body}, *bufp)
		copyBufPool.Put(bufp)
		return n, err
	}

	// Path B: --local-dir with a shared *os.File per path.  Use WriteAt
	// in the copy loop so parallel ranges into the same file never need
	// per-task Seek/Lock and can be issued concurrently against one fd.
	if t.state == nil {
		return 0, fmt.Errorf("internal: nil fileState on disk task %q", t.path)
	}
	bufp := copyBufPool.Get().(*[]byte)
	n, err := copyToWriterAt(t.state.file, t.start, resp.Body, *bufp)
	copyBufPool.Put(bufp)
	return n, err
}

// copyToWriterAt drains src into dst.WriteAt at successive offsets.
// We mirror io.CopyBuffer's loop shape but call WriteAt instead of Write,
// keeping the same allocation profile (zero, given a pre-allocated buf).
func copyToWriterAt(dst io.WriterAt, offset int64, src io.Reader, buf []byte) (int64, error) {
	var written int64
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.WriteAt(buf[:nr], offset)
			if nw < 0 || nw > nr {
				return written, fmt.Errorf("invalid WriteAt return: %d", nw)
			}
			written += int64(nw)
			offset += int64(nw)
			if ew != nil {
				return written, ew
			}
			if nw < nr {
				return written, io.ErrShortWrite
			}
		}
		if er == io.EOF {
			return written, nil
		}
		if er != nil {
			return written, er
		}
	}
}

// readerOnly / writerOnly hide ReadFrom / WriteTo so io.CopyBuffer takes
// its explicit-loop path with the supplied buffer, never re-allocating.
// Without this, io.CopyBuffer on (devnull, resp.Body) calls
// os.(*File).ReadFrom which allocates a fresh 32 KiB slab per call.
type readerOnly struct{ io.Reader }
type writerOnly struct{ io.Writer }

func (c *Client) resolveURL(repo, revision, filePath string, repoType RepoType) string {
	segments := strings.Split(filePath, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	enc := strings.Join(segments, "/")
	return fmt.Sprintf("%s/%s%s/resolve/%s/%s",
		c.Base, repoType.resolvePrefix(), escapePath(repo), url.PathEscape(revision), enc)
}

func (c *Client) setAuth(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func escapePath(repo string) string {
	// repo is "org/name" — only encode each segment.
	parts := strings.Split(repo, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// DownloadWithTimeout wraps Download with an overall deadline.
func (c *Client) DownloadWithTimeout(repo string, opts DownloadOpts, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.Download(ctx, repo, opts)
}

// OpenSink returns an io.Writer for bench-mode paths like "/dev/null".
//
// For /dev/null specifically we return io.Discard rather than an *os.File
// pointing at the OS device.  Reasoning:
//   - io.Discard implements io.ReaderFrom and consumes bytes with zero
//     allocation and zero syscalls in its hot loop.
//   - An *os.File backed by /dev/null forces every Write() through the
//     OS fd mutex; under high parallelism this becomes the dominant
//     allocator (one runtime_Semacquire per write-loop iteration) and
//     serializes all writers on a single mutex.
//
// Use "/dev/null-fd" to opt-in to the older real-fd path if you need to
// measure raw kernel write throughput (e.g. for sendfile correctness).
func OpenSink(p string) (io.Writer, func() error, error) {
	if p == "" || p == "-" {
		return os.Stdout, func() error { return nil }, nil
	}
	if p == "/dev/null" {
		return io.Discard, func() error { return nil }, nil
	}
	if p == "/dev/null-fd" {
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return nil, nil, err
		}
		return f, f.Close, nil
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

// RepoBaseName returns the last path segment of a repo id.
func RepoBaseName(repo string) string {
	return path.Base(repo)
}

// fileState carries the shared output file for every range targeting one path.
type fileState struct {
	file *os.File
	once sync.Once
	err  error
}

// filePool maps relPath → *fileState; entries are populated lazily.
type filePool struct {
	mu      sync.Mutex
	entries map[string]*fileState
}

// prepare returns the *fileState for relPath, creating the file (with the
// final Truncate to size) at most once per Download.  Subsequent callers
// share the same *os.File and issue WriteAt against it concurrently.
func (p *filePool) prepare(localDir, relPath string, size int64) (*fileState, error) {
	p.mu.Lock()
	if p.entries == nil {
		p.entries = make(map[string]*fileState)
	}
	st, ok := p.entries[relPath]
	if !ok {
		st = &fileState{}
		p.entries[relPath] = st
	}
	p.mu.Unlock()

	st.once.Do(func() {
		dest := filepath.Join(localDir, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			st.err = err
			return
		}
		f, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			st.err = err
			return
		}
		if size > 0 {
			// Truncate to the final length up front so WriteAt at
			// the tail of the file never has to extend it.  This
			// also pre-allocates the file's metadata extents on
			// xfs/ext4 (best-effort, not a guarantee on every FS).
			if err := f.Truncate(size); err != nil {
				_ = f.Close()
				st.err = err
				return
			}
		}
		st.file = f
	})
	return st, st.err
}

// closeAll closes every open fileState; call from a deferred cleanup.
func (p *filePool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, st := range p.entries {
		if st.file != nil {
			_ = st.file.Close()
		}
	}
}

// filterFiles applies Include / Exclude / Files to the tree listing.
//
// Semantics (mirrors huggingface_hub's snapshot_download):
//   - opts.Files (if non-empty) restricts to that exact set (no globbing).
//   - opts.Include (if non-empty): a path is in if any pattern matches.
//   - opts.Exclude: a path is dropped if any pattern matches.
//
// Patterns are filepath.Match-compatible; for typical "*.safetensors"
// / "config.json" usage filepath.Match is sufficient and stdlib-only.
func filterFiles(in []FileEntry, opts DownloadOpts) ([]FileEntry, error) {
	if len(opts.Files) == 0 && len(opts.Include) == 0 && len(opts.Exclude) == 0 {
		return in, nil
	}

	// Pre-validate patterns so we surface a bad glob before the network call.
	for _, p := range opts.Include {
		if _, err := filepath.Match(p, "validate"); err != nil {
			return nil, fmt.Errorf("invalid --include pattern %q: %w", p, err)
		}
	}
	for _, p := range opts.Exclude {
		if _, err := filepath.Match(p, "validate"); err != nil {
			return nil, fmt.Errorf("invalid --exclude pattern %q: %w", p, err)
		}
	}

	var explicit map[string]struct{}
	if len(opts.Files) > 0 {
		explicit = make(map[string]struct{}, len(opts.Files))
		for _, f := range opts.Files {
			explicit[strings.TrimPrefix(f, "/")] = struct{}{}
		}
	}

	out := in[:0]
	for _, f := range in {
		if explicit != nil {
			if _, ok := explicit[f.Path]; !ok {
				continue
			}
		}
		if len(opts.Include) > 0 && !globAny(opts.Include, f.Path) {
			continue
		}
		if globAny(opts.Exclude, f.Path) {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

func globAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, path); ok {
			return true
		}
		// Also try matching against the base name so users can write
		// "*.safetensors" without anchoring it to "/".
		if ok, _ := filepath.Match(p, filepathBase(path)); ok {
			return true
		}
	}
	return false
}

func filepathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// httpError carries a status code so doRetry can distinguish retryable
// (429/5xx) from fatal (4xx) responses without parsing the message.
type httpError struct {
	op     string
	status int
	body   string
}

func (e *httpError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("%s: %d %s", e.op, e.status, http.StatusText(e.status))
	}
	return fmt.Sprintf("%s: %d %s: %s", e.op, e.status, http.StatusText(e.status), e.body)
}

func (e *httpError) Status() int { return e.status }

// doRetry runs fn with bounded exponential backoff on transient failures.
//
// Retry policy:
//   - 429, 502, 503, 504 → retry with backoff (honoring Retry-After if a
//     downstream caller surfaces it; today we use exponential only).
//   - net.OpError / io.ErrUnexpectedEOF mid-body → retry.
//   - context.Canceled / context.DeadlineExceeded → do not retry; the
//     caller canceled.
//
// Backoff: base * 2^(attempt), capped at max.  Defaults: base=200ms, max=5s.
func (c *Client) doRetry(ctx context.Context, op string, fn func(ctx context.Context) error) error {
	max := c.MaxRetries
	if max <= 0 {
		max = 5
	}
	base := c.RetryBaseDelay
	if base <= 0 {
		base = 200 * time.Millisecond
	}
	cap := c.RetryMaxDelay
	if cap <= 0 {
		cap = 5 * time.Second
	}

	var lastErr error
	for attempt := 0; attempt <= max; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if !shouldRetry(err) {
			return err
		}
		if attempt == max {
			break
		}
		delay := base << attempt
		if delay > cap || delay <= 0 {
			delay = cap
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("%s: gave up after %d retries: %w", op, max, lastErr)
}

func shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var he *httpError
	if errors.As(err, &he) {
		switch he.status {
		case http.StatusTooManyRequests,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	// Network-level: dial reset, peer hangup mid-body, etc.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}
