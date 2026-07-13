// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Command fake-hf is a tiny localhost stand-in for huggingface.co used
// purely for benchmark warm-up and comparison.
//
// It speaks just enough of the Hugging Face HTTP surface to let:
//
//   - pulsys populate its content-addressed cache,
//   - Olah (Python/FastAPI HF mirror) populate its cache, AND
//   - `huggingface-cli download` / `hf download` complete an end-to-end
//     multi-file model fetch
//
// from a deterministic local source -- so the actual benchmark measures
// only the warm-hit serve path of each proxy (or, when --latency /
// --bandwidth are set, the cold-cache fetch path under simulated remote
// network conditions), not real upstream HF.
//
// Two model shapes are served:
//
//  1. /models/bench/bench (the "size suffix" model) -- single files
//     named "16k.bin" etc., used by scripts/bench_compare.sh for
//     same-URL wrk comparisons against Caddy, net/http, and Olah.
//
//  2. /models/bench/multi (the "realistic model" shape) -- a multi-
//     file model with config.json, tokenizer.json, three safetensors
//     shards, and an index file.  This is what scripts/bench_e2e.sh
//     uses to time `hf download` end-to-end through each proxy.
//
// Optional flags:
//
//	--latency 10ms      sleep N before serving each response
//	                    (simulates upstream RTT)
//	--bandwidth 100mbit cap body write to N bits/sec
//	                    (simulates upstream throughput;
//	                    accepts e.g. "100mbit", "1gbit", "10MB/s")
//
// With both at 0 (the default), fake-hf serves at loopback speed --
// useful for warm-hit wrk comparisons.  With latency/bandwidth set,
// it simulates a remote HF mirror so the proxy's cold-fetch value
// becomes visible (proxy makes the second download faster than the
// first, however slow the upstream is).
//
// Routes implemented:
//
//	HEAD/GET /models/bench/bench/resolve/main/<size>.bin
//	HEAD/GET /bench/bench/resolve/main/<size>.bin
//	HEAD/GET /models/bench/multi/resolve/main/<file>
//	HEAD/GET /bench/multi/resolve/main/<file>
//	POST     /api/models/bench/<repo>/paths-info/<commit>
//	GET/HEAD /api/models/bench/<repo>/revision/<commit>
//	GET      /api/models/bench/<repo>
//
// This binary is NOT for production.  It exists only so the bench
// harness can be reproducible offline.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// serverOpts holds the runtime knobs that the request handlers honor.
// Embedded in handler closures rather than read from globals so that
// tests / multiple instances stay independent.
type serverOpts struct {
	latency   time.Duration // injected before writing headers
	bytesPerS int64         // 0 = unlimited; otherwise rate-limit body
}

// parseBandwidth parses strings like "100mbit", "1gbit", "10MB/s", "0"
// (unlimited).  Returns bytes per second.  Bits are converted /8.
//
// Recognized units (case-insensitive):
//
//	bit, bits, b      bits per second (when paired with a decimal SI prefix)
//	bps, bit/s        bits per second
//	B, B/s, bytes/s   bytes per second
//	kbit, mbit, gbit  10^3, 10^6, 10^9 bits per second
//	kbps, mbps, gbps  same (alias)
//	KB, MB, GB        10^3, 10^6, 10^9 bytes per second (decimal)
//	KiB, MiB, GiB     2^10, 2^20, 2^30 bytes per second
func parseBandwidth(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	// Strip "/s" suffix (e.g. "10mb/s" -> "10mb").
	s = strings.TrimSuffix(s, "/s")
	// Find the boundary between the number and the unit.
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("missing number in %q", s)
	}
	numStr, unit := s[:i], strings.TrimSpace(s[i:])
	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("bad number %q: %w", numStr, err)
	}
	var multBits, multBytes float64
	switch unit {
	case "bps", "bit/s", "bit", "bits", "b":
		multBits = 1
	case "kbit", "kbps":
		multBits = 1e3
	case "mbit", "mbps":
		multBits = 1e6
	case "gbit", "gbps":
		multBits = 1e9
	case "byte", "bytes":
		multBytes = 1
	case "kb":
		multBytes = 1e3
	case "mb":
		multBytes = 1e6
	case "gb":
		multBytes = 1e9
	case "kib":
		multBytes = 1024
	case "mib":
		multBytes = 1024 * 1024
	case "gib":
		multBytes = 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unknown bandwidth unit %q (try mbit, gbit, MB, MiB, ...)", unit)
	}
	if multBytes > 0 {
		return int64(num * multBytes), nil
	}
	return int64(num * multBits / 8), nil
}

// throttledWriter copies from src to dst at a fixed bytes-per-second
// budget, sleeping after each chunk so that the average rate matches.
// chunkBytes controls latency vs accuracy: smaller = smoother but more
// syscall overhead.  64 KiB is enough to keep socket buffers fed at any
// realistic bandwidth (100 Mbit -> 1 chunk every 5 ms).
func copyAtRate(dst io.Writer, src io.Reader, bytesPerS int64) (int64, error) {
	if bytesPerS <= 0 {
		// Fast path -- no throttle.
		buf := make([]byte, 64<<10)
		return io.CopyBuffer(dst, src, buf)
	}
	const chunkBytes = 64 << 10
	buf := make([]byte, chunkBytes)
	// Per-chunk delay (rounded up).
	delay := time.Duration(float64(time.Second) * float64(chunkBytes) / float64(bytesPerS))
	var total int64
	for {
		t0 := time.Now()
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			// Catch any flusher (e.g. net/http does this implicitly
			// on Hijack-less ResponseWriter via Write).  Without a
			// flush, the kernel may aggregate chunks and defeat the
			// throttle's smoothness.
			if f, ok := dst.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		elapsed := time.Since(t0)
		if elapsed < delay {
			time.Sleep(delay - elapsed)
		}
	}
}

// payloadBytes turns "16k", "256k", "4m", "10m", "16m", "64m" (or "16k.bin",
// "256k.bin", ...) into a byte count.  Returns 0 when the suffix is unknown,
// which the handlers report as 404 to mimic HF "entry not found".
func payloadBytes(name string) int64 {
	name = strings.TrimSuffix(name, ".bin")
	if len(name) == 0 {
		return 0
	}
	var mult int64
	last := name[len(name)-1]
	switch last {
	case 'k', 'K':
		mult = 1 << 10
	case 'm', 'M':
		mult = 1 << 20
	case 'g', 'G':
		mult = 1 << 30
	default:
		return 0
	}
	n, err := strconv.ParseInt(name[:len(name)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n * mult
}

func etagFor(path string, size int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", path, size)))
	return `"` + hex.EncodeToString(h[:16]) + `"`
}

// multiModelFiles is the file list for /models/bench/multi -- a
// realistic-shaped model with config, tokenizer, three shards, and an
// index file.  Sizes are chosen so the total wallclock at 100 Mbit
// upstream (~12.5 MB/s) lands around 60 s direct vs near-instant for
// a warm proxy hit -- enough headroom to see proxy speedup clearly.
var multiModelFiles = []struct {
	name string
	size int64
}{
	{"config.json", 1 << 10},
	{"tokenizer.json", 256 << 10},
	{"tokenizer_config.json", 4 << 10},
	{"special_tokens_map.json", 1 << 10},
	{"model.safetensors.index.json", 8 << 10},
	{"model-00001-of-00003.safetensors", 256 << 20},
	{"model-00002-of-00003.safetensors", 256 << 20},
	{"model-00003-of-00003.safetensors", 256 << 20},
}

func multiSize(name string) (int64, bool) {
	for _, f := range multiModelFiles {
		if f.name == name {
			return f.size, true
		}
	}
	return 0, false
}

// resolveFile parses HF-shaped resolve URLs and returns
// (repo, filename, size).  Accepts both shapes Olah understands:
//
//	/models/<org>/<repo>/resolve/<commit>/<file>
//	/<org>/<repo>/resolve/<commit>/<file>
//
// For /models/bench/multi/... it consults the multiModelFiles list;
// for /models/bench/bench/... it falls back to the size-suffix parser.
func resolveFile(p string) (repo, file string, size int64) {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) < 5 {
		return "", "", 0
	}
	if parts[0] == "models" || parts[0] == "datasets" || parts[0] == "spaces" {
		parts = parts[1:]
	}
	if len(parts) < 5 || parts[2] != "resolve" {
		return "", "", 0
	}
	repo = parts[1]
	file = parts[4]
	if repo == "multi" {
		if sz, ok := multiSize(file); ok {
			return repo, file, sz
		}
		return repo, file, 0
	}
	return repo, file, payloadBytes(file)
}

func writeResolveHeaders(w http.ResponseWriter, file string, size int64) {
	etag := etagFor(file, size)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Linked-Etag", etag)
	w.Header().Set("X-Linked-Size", strconv.FormatInt(size, 10))
	w.Header().Set("X-Repo-Commit", "main")
}

// zeroReader emits n zero bytes without allocating per-call buffers.
// It is used for fake body streaming so wrk measures the proxy's serve
// path, not upstream allocation pressure.
type zeroReader struct{ remaining int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > z.remaining {
		n = z.remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = 0
	}
	z.remaining -= n
	return int(n), nil
}

func (o serverOpts) maybeDelay() {
	if o.latency > 0 {
		time.Sleep(o.latency)
	}
}

func (o serverOpts) resolveHandler(w http.ResponseWriter, r *http.Request) {
	_, file, size := resolveFile(r.URL.Path)
	if size <= 0 {
		http.NotFound(w, r)
		return
	}
	o.maybeDelay()
	writeResolveHeaders(w, file, size)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := copyAtRate(w, &zeroReader{remaining: size}, o.bytesPerS); err != nil {
		// Client hang-ups are routine when wrk closes connections after
		// its timeout; don't pollute logs with them.
		if !errIsConnReset(err) {
			log.Printf("fake-hf: copy %s: %v", r.URL.Path, err)
		}
	}
}

func errIsConnReset(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "use of closed network connection")
}

func (o serverOpts) pathsInfoHandler(w http.ResponseWriter, r *http.Request) {
	o.maybeDelay()
	// HF clients (and DingoSpeed) POST paths-info as JSON {"paths":[...]}
	// or as the legacy form-encoded "paths=..." style.  Accept both.
	var paths []string
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "application/json") {
		var body struct {
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		paths = body.Paths
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		paths = r.PostForm["paths"]
		if len(paths) == 0 {
			paths = r.Form["paths"]
		}
	}
	repo := repoFromPathsInfo(r.URL.Path)
	out := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		var size int64
		if repo == "multi" {
			size, _ = multiSize(p)
		} else {
			size = payloadBytes(p)
		}
		if size <= 0 {
			continue
		}
		out = append(out, map[string]any{
			"path": p,
			"size": size,
			"type": "file",
			"oid":  strings.Trim(etagFor(p, size), `"`),
			"lfs":  nil,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// repoFromPathsInfo extracts "bench" or "multi" from
// /api/models/bench/<repo>/paths-info/<commit>.
func repoFromPathsInfo(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	// expected: api models bench <repo> paths-info <commit>
	if len(parts) >= 5 && parts[0] == "api" && parts[1] == "models" && parts[2] == "bench" {
		return parts[3]
	}
	return ""
}

func (o serverOpts) modelInfoHandler(w http.ResponseWriter, r *http.Request) {
	o.maybeDelay()
	repo := "bench"
	if strings.Contains(r.URL.Path, "/multi") {
		repo = "multi"
	}
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"id":       "bench/" + repo,
		"sha":      "a000000000000000000000000000000000000000",
		"tags":     []string{},
		"siblings": siblingsFor(repo),
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// siblingsFor returns the file list a HF client sees from
// /api/models/<repo>/revision/<commit>.  Each entry needs "rfilename"
// so huggingface_hub.snapshot_download knows what to fetch.
func siblingsFor(repo string) []map[string]any {
	switch repo {
	case "multi":
		out := make([]map[string]any, 0, len(multiModelFiles))
		for _, f := range multiModelFiles {
			out = append(out, map[string]any{
				"rfilename": f.name,
				"size":      f.size,
				"blob_id":   strings.Trim(etagFor(f.name, f.size), `"`),
			})
		}
		return out
	default:
		return []map[string]any{}
	}
}

// revisionHandler answers HEAD/GET on /api/models/bench/<repo>/revision/<commit>.
// Olah's check_commit_hf and get_commit_hf use this to validate a repo +
// commit pair before serving a resolve URL.  We return a stable fake sha
// so warm hits never miss, plus the siblings list so the HF client knows
// what to fetch on `hf download`.
func (o serverOpts) revisionHandler(w http.ResponseWriter, r *http.Request) {
	o.maybeDelay()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		return
	}
	repo := "bench"
	if strings.Contains(r.URL.Path, "/multi/") {
		repo = "multi"
	}
	resp := map[string]any{
		"_id":      repo,
		"id":       "bench/" + repo,
		"sha":      "a000000000000000000000000000000000000000",
		"siblings": siblingsFor(repo),
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func main() {
	addr := flag.String("listen", "127.0.0.1:18484", "TCP listen address")
	latency := flag.Duration("latency", 0, "per-request artificial delay (e.g. 10ms) to simulate upstream RTT; 0=disabled")
	bandwidth := flag.String("bandwidth", "0", "per-response throughput cap (e.g. 100mbit, 1gbit, 10MB/s); 0=unlimited")
	flag.Parse()

	bps, err := parseBandwidth(*bandwidth)
	if err != nil {
		log.Fatalf("--bandwidth: %v", err)
	}
	opts := serverOpts{latency: *latency, bytesPerS: bps}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/bench/bench/paths-info/", opts.pathsInfoHandler)
	mux.HandleFunc("/api/models/bench/bench/revision/", opts.revisionHandler)
	mux.HandleFunc("/api/models/bench/bench", opts.modelInfoHandler)
	mux.HandleFunc("/api/models/bench/multi/paths-info/", opts.pathsInfoHandler)
	mux.HandleFunc("/api/models/bench/multi/revision/", opts.revisionHandler)
	mux.HandleFunc("/api/models/bench/multi", opts.modelInfoHandler)
	mux.HandleFunc("/", opts.resolveHandler)

	if opts.latency > 0 || opts.bytesPerS > 0 {
		bwDesc := "unlimited"
		if opts.bytesPerS > 0 {
			bwDesc = fmt.Sprintf("%d B/s", opts.bytesPerS)
		}
		log.Printf("fake-hf listening on %s  (latency=%s, bandwidth=%s)", *addr, opts.latency, bwDesc)
	} else {
		log.Printf("fake-hf listening on %s (no shaping)", *addr)
	}
	srv := &http.Server{Addr: *addr, Handler: mux}
	log.Fatal(srv.ListenAndServe())
}
