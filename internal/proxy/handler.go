// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package proxy implements pulsys's Hugging Face-aware reverse-proxy
// data plane.
//
// The handler is plain `net/http` (no fasthttp).  Why net/http for the
// slow path?
//
//   - It's the ONLY ingress where Go's response writer transparently
//     calls (*net.TCPConn).ReadFrom on an *io.SectionReader / *os.File
//     body, which resolves to sendfile(2) on Linux + Darwin.  fasthttp
//     cannot do this -- its body-stream API forces a userspace copy.
//
//   - It removes one third-party dependency from the production hot
//     path.
//
//   - The handler is fronted by internal/coreserver for warm cache hits
//     (zero-alloc HTTP/1.1 parser + sf_hdtr-fused sendfile on Darwin).
//     coreserver invokes this handler via its Fallback hook for any
//     request it cannot serve from cache: cache misses, non-GET, multi
//     -host (/_p/), metadata, etc.  The shared interface is
//     http.ResponseWriter / *http.Request so we get net/http's sendfile
//     plumbing for free on the slow path while keeping the warm-hit
//     fast path in coreserver where the absolute-minimum-syscall
//     pattern lives.
package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/classify"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/hostallow"
	"github.com/pulsys-io/pulsys/internal/rewrite"
	"github.com/pulsys-io/pulsys/internal/telemetry"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

// hopRequestHeaders are stripped before any outbound upstream request.
var hopRequestHeaders = map[string]struct{}{
	"connection": {}, "keep-alive": {}, "proxy-connection": {}, "upgrade": {},
	"te": {}, "trailer": {}, "transfer-encoding": {}, "host": {},
	// Accept-Encoding is intentionally stripped on every outbound request
	// so upstream returns RAW bytes.  Caching gzipped bodies is wrong for
	// two reasons:
	//  1. hf-cli's chunked downloader writes the response stream verbatim
	//     to disk and then validates against the uncompressed size -- if
	//     we serve compressed bytes the consistency check fails.
	//  2. Storing compressed bytes in the cache would require us to either
	//     decompress on every warm hit (expensive) or pin clients to the
	//     same encoding negotiation (fragile).  Storing uncompressed bytes
	//     once and serving them verbatim is the right invariant.
	"accept-encoding": {},
}

// hopResponseHeaders are stripped from upstream responses before we relay
// them to the client.  The response framing (Content-Length / chunked) is
// handled by net/http.
var hopResponseHeaders = map[string]struct{}{
	"connection": {}, "keep-alive": {}, "proxy-connection": {}, "upgrade": {},
	"transfer-encoding": {},
}

// preservedResponseHeaders are upstream response headers that downstream
// clients (huggingface_hub in particular) treat as authoritative
// metadata for a resolved artifact.  We capture them on cold fetches
// and replay them verbatim on every warm / offline hit so a HEAD
// validation succeeds without an upstream call.
//
// huggingface_hub.file_download._get_metadata_or_catch_error reads:
//
//	X-Repo-Commit      -> commit_hash    (required; LocalEntryNotFoundError if absent on a remote-only run)
//	X-Linked-Etag      -> etag           (LFS file's content sha; falls back to ETag header)
//	X-Linked-Size      -> file size      (falls back to Content-Length)
//	Location           -> redirect chain (already handled separately)
//
// We also preserve Accept-Ranges so range-probing clients see the same
// answer warm and cold.  Canonical header keys (http.CanonicalHeaderKey
// form) are used as map keys; values join multi-value headers with ", "
// per RFC 7230 §3.2.2.
var preservedResponseHeaders = []string{
	"X-Repo-Commit",
	"X-Linked-Etag",
	"X-Linked-Size",
	"X-Xet-Hash",
	"Accept-Ranges",
}

// pickPreservedHeaders extracts the preservedResponseHeaders subset from
// an upstream response header set, returning nil if none are present so
// Meta.ExtraHeaders stays small on cache hits that don't need it.
func pickPreservedHeaders(src http.Header) map[string]string {
	var out map[string]string
	for _, k := range preservedResponseHeaders {
		v := src.Get(k)
		if v == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(preservedResponseHeaders))
		}
		out[k] = v
	}
	return out
}

// applyExtraHeaders writes the preserved upstream headers back to a
// client-facing response on warm / offline cache hits.  Skips empty
// values so a meta that picked up partial entries doesn't shadow
// legitimate ones with empty strings on replay.
func applyExtraHeaders(dst http.Header, extra map[string]string) {
	for k, v := range extra {
		if v == "" {
			continue
		}
		dst.Set(k, v)
	}
}

// resolveRevParts splits a /<org>/<repo>/resolve/<rev>/<file> path into
// its components, returning the path with <rev> replaced by the
// argument.  If `path` doesn't match the resolve pattern, ok is false.
// Used to compute the SHA-pinned alias key for a request that came in
// under a symbolic revision (e.g. "main").
func resolveRevAliasPath(path, newRev string) (string, bool) {
	const marker = "/resolve/"
	i := strings.Index(path, marker)
	if i < 0 {
		return "", false
	}
	rest := path[i+len(marker):]
	j := strings.IndexByte(rest, '/')
	if j <= 0 {
		return "", false
	}
	if rest[:j] == newRev {
		return path, true // identity: rev already matches
	}
	return path[:i+len(marker)] + newRev + rest[j:], true
}

// maybeWriteRevAlias persists a SHA-pinned alias for the cache entry we
// just wrote at `canonicalKey`.  Triggered by responses that carry an
// X-Repo-Commit header on a `/resolve/<rev>/<file>` path where <rev>
// differs from the commit hash.  Without this alias, huggingface_hub's
// post-download HEAD validations -- which target /resolve/<sha>/<file>
// rather than /resolve/<rev>/<file> -- 504 on warm/offline because the
// cache was indexed under the symbolic ref, not the SHA.
func (h *Handler) maybeWriteRevAlias(method, upstreamHost, path, rawQuery, auth, canonicalKey string, headers http.Header) {
	sha := headers.Get("X-Repo-Commit")
	if sha == "" {
		return
	}
	aliasPath, ok := resolveRevAliasPath(path, sha)
	if !ok || aliasPath == path {
		return
	}
	// Same key-normalisation rules as the request path: query stripped
	// on content-addressed hosts, auth always stripped.  (See ServeHTTP
	// for the rationale.)
	_ = auth
	keyQuery := rawQuery
	if cache.IsContentAddressedHost(upstreamHost) {
		keyQuery = ""
	}
	const keyAuth = ""
	aliasKey := h.keyHexC.Get(method, upstreamHost, aliasPath, keyQuery, keyAuth)
	if aliasKey == canonicalKey {
		return
	}
	if err := h.store.StoreAlias(aliasKey, canonicalKey, upstreamHost, aliasPath, rawQuery); err != nil {
		h.log.Warn("store rev alias", "err", err, "host", upstreamHost, "path", aliasPath)
	}
}

// Handler is the HF-aware reverse-proxy handler.  It satisfies
// http.Handler so it composes with anything in the net/http ecosystem
// (httptest.Server, http.ServeMux, coreserver's Fallback hook, etc.).
type Handler struct {
	cfg     *config.Config
	store   *cache.Store
	up      upstream.Client
	log     *slog.Logger
	allowf  func(host string) bool
	keyHexC cache.KeyHexCache
	// inflightBudget bounds how long an artifact GET waits to claim its
	// in-flight range before falling through to a non-caching pass-through
	// (see ServeHTTP).  Zero means "wait unbounded" — used by the
	// importer's loopback handler, which has no client-side header timeout
	// and wants to populate the cache.  The public ingress sets a positive
	// budget from cfg.InflightAcquireTimeout.
	inflightBudget time.Duration
}

// NewHandler constructs the proxy handler.  up is an interface so tests
// can inject a fake upstream Client.
func NewHandler(cfg *config.Config, store *cache.Store, up upstream.Client, log *slog.Logger) *Handler {
	return &Handler{
		cfg:            cfg,
		store:          store,
		up:             up,
		log:            log,
		allowf:         hostallow.New(cfg.AllowHost),
		inflightBudget: cfg.InflightAcquireTimeout,
	}
}

// dataPlaneSecurityHeaders is the minimal response-side
// defense-in-depth applied to every data-plane response.
//
// We deliberately ship a SMALLER set than the admin handler:
//
//	X-Content-Type-Options: nosniff
//	  -- universal, safe even on binary downloads.  Prevents
//	     browsers from sniffing an error/JSON body as
//	     text/html and rendering it.
//
// We do NOT set the following, even though the admin handler
// does:
//
//	X-Frame-Options / CSP
//	  -- the data plane returns model artifacts (safetensors,
//	     parquet) and JSON metadata for first-party CLI clients.
//	     It is NOT a browser surface, so framing / CSP would
//	     add no defense.
//
//	Cross-Origin-Resource-Policy: same-origin
//	  -- legitimate browser-based callers (Colab, HF Hub web
//	     UI, notebook-hosted SDKs) MAY load resources from
//	     the proxy from a different origin.  Setting CORP to
//	     same-origin would silently break them.  See
//	     docs/security.md.
//
//	Strict-Transport-Security
//	  -- TLS termination is the LB's job; HSTS lives at the
//	     LB.
//
// Setting the header here (rather than at each writer site)
// is a single sieve that catches every code path including
// 4xx / 5xx errors, sendfile responses, and partial-content
// range responses.
func setDataPlaneSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// ServeHTTP dispatches one request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setDataPlaneSecurityHeaders(w)
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
		return
	}

	upstreamHost, path, ok := h.routeUpstream(w, r.URL.Path)
	if !ok {
		return
	}
	rawQuery := r.URL.RawQuery
	// Lift the Pulsys-internal origin marker out of the inbound query
	// BEFORE anything else looks at the URL.  It is purely a side-channel
	// from the redirect rewriter (see internal/rewrite.OriginQueryParam)
	// and must never reach upstream or the cache key.  We re-attach it
	// to the streamCacheable* writers as Meta.OriginPath so the admin UI
	// can attribute content-addressed bodies back to the originating
	// /Org/Name/resolve/<rev>/<file>.
	originPath, cleanedQuery := rewrite.ExtractAndStripOrigin(rawQuery)
	if originPath != "" {
		rawQuery = cleanedQuery
		r.URL.RawQuery = cleanedQuery
	}
	method := r.Method
	auth := r.Header.Get("Authorization")
	// Cache-key normalisation:
	//
	// 1. Authorization is NEVER part of the cache key.  A single user
	//    runs the same proxy across many clients (Python, Go) and many
	//    contexts (online with HF_TOKEN, offline without, hf-transfer
	//    pulling from S3 / cas-bridge with a presign instead of a
	//    bearer).  Letting Authorization into the key fragments the
	//    cache by client behavior and produces the warm-cache regressions
	//    observed on EC2:
	//      - 920 cas-bridge upstream fetches during a "warm" 15 GB Go
	//        run because Go preserves Authorization on the proxy
	//        loopback redirect while huggingface_hub strips it (different
	//        cache slot for the same body).
	//      - 98 offline_504 refusals on Python's HEAD-validation
	//        because the cold GET ran with HF_TOKEN and the offline
	//        HEAD ran anonymously (different cache slot for the same
	//        /resolve/<sha>/<file> entry).
	//    Auth still flows to upstream on cold fetches; it just doesn't
	//    influence WHERE we file the response on disk.
	//
	// 2. Query is part of the key for huggingface.co (the API takes
	//    meaningful query params on tree / paths-info).  For content-
	//    addressed hosts (cas-bridge / LFS CDN) we strip the query
	//    because it's a rotating AWS presigned signature; two redirects
	//    to the same file body otherwise land in different slots.
	keyQuery := rawQuery
	if cache.IsContentAddressedHost(upstreamHost) {
		keyQuery = ""
	}
	const keyAuth = ""
	keyHex := h.keyHexC.Get(method, upstreamHost, path, keyQuery, keyAuth)

	// HEAD requests are read-only metadata.  We treat them as aliases
	// of GET on the same path for both classification (artifact bit)
	// and cache lookup (we read from the GET-keyed entry and return
	// headers only).  Without this alias, `huggingface_hub.snapshot_
	// download` -- which issues HEAD per file to pre-validate ETag /
	// Content-Length before the body GET -- 504s in offline mode for
	// every file, because we only ever wrote a GET cache entry.
	isHead := method == http.MethodHead
	artifact := classify.ArtifactGET(h.cfg.DefaultHost, upstreamHost, method, path)
	if isHead && !artifact {
		// Re-evaluate the path under method=GET to pick up
		// /resolve/<rev>/<file>, /info/lfs/..., and the /_p/<host>/
		// content-addressed variants.
		artifact = classify.ArtifactGET(h.cfg.DefaultHost, upstreamHost, http.MethodGet, path)
	}
	rangeHdr := r.Header.Get("Range")

	// Per-request cache-key trace (debug-only).  Captures the EXACT
	// components hashed into keyHex so Python-vs-Go-vs-curl key mismatches
	// are visible to grep instead of inferable from miss-counter arithmetic.
	// Auth is logged as a length-and-prefix tag (never the raw token).
	if h.log.Enabled(r.Context(), slog.LevelDebug) {
		h.log.Debug("cache-key",
			"method", method,
			"host", upstreamHost,
			"path", path,
			"raw_query_len", len(rawQuery),
			"key_query_len", len(keyQuery),
			"query_stripped", rawQuery != "" && keyQuery == "",
			"auth_tag", authTag(auth),
			"range", rangeHdr,
			"artifact", artifact,
			"key_hex", keyHex,
		)
	}

	// Cache-hit policy:
	//   - artifacts (LFS bodies): always serve from cache when present.
	//   - metadata (revision / tree / etc.): serve from cache when
	//     -offline is set (default true).  Pass -offline=false to
	//     always refresh metadata upstream for repo freshness.
	//   - HEAD: same gate as GET; lookup key uses method=GET.
	//
	// Cache misses pass through to upstream unless -strict-offline is set.
	if (method == http.MethodGet || isHead) && (artifact || h.cfg.Offline) {
		lookupKey := keyHex
		if isHead {
			// HEAD looks up under the GET method, but with the same
			// key-normalisation we just did on the request key:
			// keyAuth is always "", keyQuery is "" only on
			// content-addressed hosts.  This is what lets HEAD on
			// /resolve/<sha>/<file> find the alias entry written by
			// the cold GET on /resolve/main/<file>.
			lookupKey = h.keyHexC.Get(http.MethodGet, upstreamHost, path, keyQuery, keyAuth)
		}
		if h.tryServeFromCache(w, rangeHdr, lookupKey, isHead) {
			return
		}
	}

	// Strict-offline: refuse upstream on cache miss with a 504 carrying
	// enough detail to diagnose the miss.  Default is pass-through.
	if h.cfg.StrictOffline {
		respondOffline(w, upstreamHost, path, rangeHdr, keyHex, artifact, method)
		return
	}

	h.forward(w, r, upstreamHost, path, rawQuery, keyHex, artifact, method, originPath)
}

// authTag is a short, non-secret fingerprint of an Authorization header
// for log correlation.  Empty for absent auth, "anon" for non-bearer,
// "bearer:len=NN/sha=AAAA" for Bearer tokens.  Never logs the token.
func authTag(auth string) string {
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return "anon"
	}
	tok := auth[len(prefix):]
	// Reuse the cache key hash: same hash function the keyHexC uses so the
	// auth_tag in two log lines matches iff the underlying token matches.
	sum := cache.HashAuth(tok)
	return "bearer:len=" + strconv.Itoa(len(tok)) + "/sha=" + sum[:8]
}

// respondOffline writes a 504 with a body that names the missing cache slot.
// Format is line-oriented "key=value" so test harnesses can grep without a
// JSON parser, and CloudWatch/SSM dashboards stay readable.
func respondOffline(w http.ResponseWriter, host, path, rangeHdr, keyHex string, artifact bool, method string) {
	telemetry.IncOfflineRefusals()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Pulsys-Offline", "1")
	w.Header().Set("X-Pulsys-KeyHex", keyHex)
	w.WriteHeader(http.StatusGatewayTimeout)
	_, _ = io.WriteString(w,
		"offline=1\n"+
			"reason=cache-miss-in-offline-mode\n"+
			"method="+method+"\n"+
			"upstream_host="+host+"\n"+
			"path="+path+"\n"+
			"range="+rangeHdr+"\n"+
			"key_hex="+keyHex+"\n"+
			"artifact="+strconv.FormatBool(artifact)+"\n",
	)
}

func (h *Handler) routeUpstream(w http.ResponseWriter, path string) (host string, outPath string, ok bool) {
	host = strings.ToLower(strings.TrimSpace(h.cfg.DefaultHost))
	outPath = path
	if !strings.HasPrefix(path, "/_p/") {
		return host, outPath, true
	}
	rest := strings.TrimPrefix(path, "/_p/")
	i := strings.Index(rest, "/")
	if i <= 0 || i == len(rest)-1 {
		http.Error(w, "bad /_p/ path", http.StatusBadRequest)
		return "", "", false
	}
	prefHost := strings.ToLower(rest[:i])
	if !h.allowf(prefHost) {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return "", "", false
	}
	return prefHost, rest[i:], true
}

// tryServeFromCache returns true and writes the response if the cached
// object fully covers the requested range (or the whole object if no
// Range was set).  On a hit it records 0 upstream fetches / 0 upstream
// bytes.
//
// The body is delivered via io.Copy on an *io.SectionReader over the
// cache's *os.File.  net/http's ResponseWriter is also an
// io.ReaderFrom; on TCP connections, io.Copy(rw, sectionReader)
// resolves to (*net.TCPConn).ReadFrom(*io.SectionReader) which
// transparently dispatches to sendfile(2) on Linux + Darwin.
//
// (The truly absolute-minimum-syscall path is coreserver.tryServeWarm,
// which fuses status line + headers + body into a single
// sendfile + sf_hdtr syscall on Darwin.  coreserver runs in front of
// this handler and only falls through to ServeHTTP when it cannot
// take over.)
func (h *Handler) tryServeFromCache(w http.ResponseWriter, rangeHdr, keyHex string, headOnly bool) bool {
	meta, err := h.store.LoadMeta(keyHex)
	if err != nil || meta == nil {
		telemetry.IncCacheMiss()
		return false
	}
	// Alias entry: follow one hop to the canonical cache slot.  This is
	// how /resolve/<sha>/<file> finds the body that was originally
	// cached under /resolve/main/<file>: huggingface_hub downloads via
	// the symbolic ref but validates via the resolved commit hash on
	// every subsequent HEAD/GET.  The cold cache path writes a tiny
	// AliasOf meta at the SHA-pinned key so both paths converge on the
	// same body without duplicating it on disk.
	if meta.AliasOf != "" {
		canonicalKey := meta.AliasOf
		canonical, err := h.store.LoadMeta(canonicalKey)
		if err != nil || canonical == nil {
			telemetry.IncCacheMiss()
			return false
		}
		meta = canonical
		keyHex = canonicalKey
	}
	// Cached redirect (30x): replay status + (re-)rewritten Location.
	// This is the fast path for /resolve/main/<file>: HF returns 302 to
	// cas-bridge; without cache every range issues a fresh upstream
	// roundtrip just to read that same 302.  With this branch the warm
	// path is one disk-meta read.  Range header is irrelevant on
	// redirects -- the client carries it into the followed request to
	// /_p/cas-bridge/... where the body is served from disk.
	if cache.IsCacheableRedirectStatus(meta.StatusCode) && meta.Location != "" {
		return h.serveCachedRedirect(w, meta)
	}
	// HEAD on a cached 200/206 body: answer with the metadata headers
	// and an empty body.  We deliberately do NOT require Span coverage
	// for HEAD -- the client wants to know the resource exists and its
	// total size, which meta carries even when our body cache is
	// partial.
	if headOnly {
		return h.serveCachedHead(w, meta)
	}
	if len(meta.Spans) == 0 {
		telemetry.IncCacheMiss()
		return false
	}
	total := int64(-1)
	if meta.Total != nil {
		total = *meta.Total
	}
	var wantStart, wantEnd int64
	if rangeHdr == "" {
		if total < 0 {
			telemetry.IncCacheMiss()
			return false
		}
		wantStart, wantEnd = 0, total
	} else {
		var ok bool
		wantStart, wantEnd, ok = cache.ParseSingleRange(rangeHdr, total)
		if !ok || wantEnd < 0 {
			telemetry.IncCacheMiss()
			return false
		}
	}
	if !cache.Covers(meta.Spans, wantStart, wantEnd) {
		telemetry.IncCacheMiss()
		return false
	}

	bh, release, err := h.store.AcquireBody(keyHex)
	if err != nil {
		telemetry.IncCacheMiss()
		return false
	}
	defer func() { _ = release.Close() }()
	telemetry.IncCacheHit()

	n := wantEnd - wantStart
	hdr := w.Header()
	if meta.ContentType != "" {
		hdr.Set("Content-Type", meta.ContentType)
	}
	if meta.ETag != "" {
		hdr.Set("ETag", meta.ETag)
	}
	applyExtraHeaders(hdr, meta.ExtraHeaders)
	hdr.Set("Content-Length", strconv.FormatInt(n, 10))
	if rangeHdr != "" {
		hdr.Set("Content-Range", formatContentRange(wantStart, wantEnd, total))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	sec := io.NewSectionReader(bh.File(), wantStart, n)
	written, _ := io.Copy(w, sec)
	telemetry.AddClientBytesServed(written)
	return true
}

// serveCachedHead answers a HEAD request from cached GET metadata.
// Used by clients (notably huggingface_hub's snapshot_download) that
// validate ETag / Content-Length via HEAD before issuing the body GET.
// Without this alias every such HEAD 504s in offline mode -- we only
// ever wrote the GET response under the GET key.
//
// Coverage is intentionally NOT checked: HEAD describes the full
// resource, and meta.Total carries the authoritative size even when
// our body cache holds only a partial range.  Range headers on HEAD
// are ignored for the same reason (HEAD+Range is rare in practice and
// every cached span has the same Content-Length when answering as
// "headers for the whole object").
func (h *Handler) serveCachedHead(w http.ResponseWriter, meta *cache.Meta) bool {
	telemetry.IncCacheHit()
	hdr := w.Header()
	if meta.ContentType != "" {
		hdr.Set("Content-Type", meta.ContentType)
	}
	if meta.ETag != "" {
		hdr.Set("ETag", meta.ETag)
	}
	applyExtraHeaders(hdr, meta.ExtraHeaders)
	if meta.Total != nil && *meta.Total >= 0 {
		hdr.Set("Content-Length", strconv.FormatInt(*meta.Total, 10))
	}
	w.WriteHeader(http.StatusOK)
	return true
}

// serveCachedRedirect replays a previously cached 30x response.  The
// stored Location is the upstream (pre-rewrite) URL; we re-run it
// through rewrite.LocationToProxy on every hit so the cache is
// independent of the current PublicBaseURL.
func (h *Handler) serveCachedRedirect(w http.ResponseWriter, meta *cache.Meta) bool {
	telemetry.IncCacheHit()
	loc := meta.Location
	// On a warm cached redirect we know the user-facing request path
	// that this redirect was originally generated for (meta.Path).
	// Re-attach it as the rewrite origin so the follow-up /_p/ request
	// can persist it as OriginPath on the body cache entry.
	if nl, ok := rewrite.LocationToProxyWithOrigin(h.cfg.PublicBaseURL, h.allowf, loc, meta.Path); ok {
		loc = nl
	}
	hdr := w.Header()
	hdr.Set("Location", loc)
	if meta.ContentType != "" {
		hdr.Set("Content-Type", meta.ContentType)
	}
	if meta.ETag != "" {
		hdr.Set("ETag", meta.ETag)
	}
	// Replay preserved upstream headers (X-Linked-Etag, X-Linked-Size,
	// X-Repo-Commit, ...).  huggingface_hub's _get_metadata_or_catch_error
	// reads these on every HEAD/GET to /resolve/<rev>/<file> and raises
	// LocalEntryNotFoundError if they're missing on a warm-cache hit.
	applyExtraHeaders(hdr, meta.ExtraHeaders)
	// 30x bodies are typically empty (and irrelevant -- only the
	// Location matters to the client).  Setting Content-Length: 0
	// avoids net/http auto-framing a chunked body.
	hdr.Set("Content-Length", "0")
	w.WriteHeader(meta.StatusCode)
	return true
}

func formatContentRange(start, end, total int64) string {
	last := end - 1
	if total >= 0 {
		return "bytes " + strconv.FormatInt(start, 10) + "-" + strconv.FormatInt(last, 10) + "/" + strconv.FormatInt(total, 10)
	}
	return "bytes " + strconv.FormatInt(start, 10) + "-" + strconv.FormatInt(last, 10) + "/*"
}

// forward issues the upstream request and either tees its body to
// disk + client (artifact path) or streams metadata bytes through.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, upstreamHost, path, rawQuery, keyHex string, artifact bool, meth, originPath string) {
	hdr := http.Header{}
	// Content-addressed hosts (Xet CAS bridge, LFS CDN) authenticate
	// upstream requests via the presigned signature in the query string.
	// Forwarding the client's Authorization header here is at best dead
	// weight and at worst a token leak to a third-party AWS frontend.
	// Strip it so the upstream call matches what the host actually uses.
	// Pulsys never forwards a caller's inbound Authorization upstream: a
	// pulsys_* PAT (or any client credential) must never reach Hugging
	// Face or a content-addressed CDN.  The inbound Authorization is
	// always dropped here.  On non content-addressed hosts we replace it
	// with Pulsys's own read-only HF token (PULSYS_HF_TOKEN), which is
	// what authenticates and authorizes every cold-miss read; content-
	// addressed hosts (Xet CAS bridge, LFS CDN) authenticate via the
	// presigned signature in the query string and need no header.
	injectHFToken := !cache.IsContentAddressedHost(upstreamHost) && h.cfg.UpstreamHFToken != ""
	for k, vv := range r.Header {
		kk := strings.ToLower(k)
		if _, skip := hopRequestHeaders[kk]; skip {
			continue
		}
		if kk == "authorization" {
			continue
		}
		for _, v := range vv {
			hdr.Add(k, v)
		}
	}
	if injectHFToken {
		hdr.Set("Authorization", "Bearer "+h.cfg.UpstreamHFToken)
	}

	cctx := r.Context()
	if cctx == nil {
		cctx = context.Background()
	}

	var reqBody []byte
	if meth != http.MethodGet && meth != http.MethodHead && r.Body != nil {
		// We have to materialize the request body for upstream re-emission.
		// fasthttp used to give us a []byte directly; with net/http we do
		// the same explicit copy.
		if b, err := io.ReadAll(io.LimitReader(r.Body, 256<<20)); err == nil {
			reqBody = b
		}
		_ = r.Body.Close()
	}

	// For artifact misses we coordinate concurrent fetches by the
	// requested byte range, NOT by the cache key as a whole.  This lets
	// hf-cli's parallel range fetches (typically 4-16 disjoint ranges per
	// large LFS file) flow through the proxy concurrently while still
	// deduplicating any genuine duplicate-range requests.
	var unlock func()
	passthrough := false
	if artifact && meth == http.MethodGet {
		rangeHdr := r.Header.Get("Range")
		var inflightStart, inflightEnd int64 = 0, math.MaxInt64
		if rangeHdr != "" {
			if start, end, ok := cache.ParseSingleRange(rangeHdr, -1); ok && end > start {
				inflightStart, inflightEnd = start, end
			}
		}
		if h.inflightBudget > 0 {
			// Bounded wait: a long whole-file fetch in flight (e.g. an
			// import, or another client's no-Range GET) must not pin this
			// request past the client's response-header read timeout.  If
			// we can't claim the range within the budget, re-check the
			// cache once (the holder may have just finished) and otherwise
			// fall through to an independent pass-through fetch below.  We
			// deliberately take NO in-flight ticket on that fallback so we
			// never become a second writer racing the holder's cache write.
			var ok bool
			unlock, ok = h.store.AcquireRangeCtx(cctx, keyHex, inflightStart, inflightEnd, h.inflightBudget)
			if !ok {
				if h.tryServeFromCache(w, rangeHdr, keyHex, false) {
					return
				}
				passthrough = true
				telemetry.IncInflightContendedPassthrough()
			}
		} else {
			// Unbounded wait (importer loopback / tests): serialize against
			// any overlapping fetch and serve from the populated cache.
			unlock = h.store.AcquireRange(keyHex, inflightStart, inflightEnd)
		}
		// Re-check cache under the in-flight ticket -- another goroutine
		// may have populated this exact range while we were waiting.
		if !passthrough && h.tryServeFromCache(w, rangeHdr, keyHex, false) {
			unlock()
			return
		}
	}

	resp, err := h.up.Do(cctx, meth, upstreamHost, path, rawQuery, hdr, reqBody)
	if err != nil {
		if unlock != nil {
			unlock()
		}
		h.log.Warn("upstream error", "err", err, "host", upstreamHost)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// The "outbound origin" we attach to rewritten Location headers is
	// the *inbound* user-facing path on this request -- unless this
	// request itself was a /_p/<host>/... follow that carried an
	// inbound origin (a multi-hop redirect: HF -> resolve-cache ->
	// cas-bridge).  In that case we preserve the inbound origin so the
	// final body's meta still names the user-facing request, not the
	// intermediate resolve-cache URL.
	outboundOrigin := originPath
	if outboundOrigin == "" && !strings.HasPrefix(r.URL.Path, "/_p/") {
		outboundOrigin = r.URL.Path
	}
	h.copyAndRewriteHeaders(w.Header(), resp, outboundOrigin)

	// Contended pass-through: we could not claim the in-flight range in
	// time, so stream the upstream response straight to the client WITHOUT
	// writing the cache (no ticket held).  The concurrent holder remains
	// responsible for populating the cache; this request just guarantees
	// the client gets a timely response instead of blocking on the lock.
	if passthrough {
		h.streamMetadata(w, resp)
		return
	}

	// Any 200 / 206 GET response is cacheable, regardless of whether the
	// route is classified as an artifact or metadata.  Caching metadata
	// (tree / revision / etc.) is what makes the -offline contract
	// achievable for a real `hf download` flow: without it the very
	// first metadata round-trip blocks the entire download chain.
	auth := r.Header.Get("Authorization")
	if meth == http.MethodGet && (resp.Status == http.StatusOK || resp.Status == http.StatusPartialContent) {
		h.streamCacheable(w, resp, keyHex, upstreamHost, path, rawQuery, auth, unlock, artifact, originPath)
		return
	}

	// 30x GET responses are cacheable too: HF's /resolve/<rev>/<file>
	// returns 302 → cas-bridge with each call.  Without this, every
	// range request from a parallel client incurs one upstream round-
	// trip just to re-read the same redirect target.  Caching the
	// Location once collapses 911 redirect lookups (Qwen-7B at
	// 16 MiB chunks) into a single upstream hit per file.
	if meth == http.MethodGet && cache.IsCacheableRedirectStatus(resp.Status) {
		h.cacheRedirect(w, resp, keyHex, upstreamHost, path, rawQuery, auth)
		if unlock != nil {
			unlock()
		}
		return
	}

	// Non-cacheable response (304, 4xx, 5xx, HEAD, etc.) -- stream
	// through with byte counters; never ReadAll.
	h.streamMetadata(w, resp)
	if unlock != nil {
		unlock()
	}
}

// cacheRedirect persists a 30x response (status + upstream Location) to
// disk and forwards the rewritten 30x to the client.  The on-disk meta
// stores the upstream Location verbatim; we re-rewrite on every warm
// hit so the cache survives proxy URL changes.
func (h *Handler) cacheRedirect(w http.ResponseWriter, resp *upstream.Response, keyHex, upstreamHost, path, rawQuery, auth string) {
	defer func() { _ = resp.Body.Close() }()
	upstreamLoc := resp.Header.Get("Location")
	if upstreamLoc == "" {
		// Pathological: 30x without Location.  Don't cache; pass through.
		h.streamMetadata(w, resp)
		return
	}
	if err := h.store.StoreRedirect(
		keyHex,
		resp.Status,
		upstreamHost,
		path,
		rawQuery,
		upstreamLoc,
		resp.Header.Get("Content-Type"),
		resp.Header.Get("ETag"),
		pickPreservedHeaders(resp.Header),
	); err != nil {
		h.log.Warn("store redirect", "err", err, "host", upstreamHost, "path", path)
	}
	h.maybeWriteRevAlias(http.MethodGet, upstreamHost, path, rawQuery, auth, keyHex, resp.Header)
	telemetry.IncMetadataUpstreamFetch()
	// Discard any body bytes for accounting consistency; 30x bodies are
	// usually empty but HF sometimes returns a short HTML/"Found" stub.
	if resp.ContentLength != 0 {
		n, _ := io.Copy(io.Discard, resp.Body)
		telemetry.AddMetadataUpstreamBytes(n)
	}
	// copyAndRewriteHeaders already wrote the rewritten Location header
	// into w.Header().  Set Content-Length: 0 so net/http frames the
	// response cleanly and write the status.
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(resp.Status)
}

// streamCacheable is the unified "tee upstream bytes to disk + client"
// path.  It replaces the older artifact-only streamArtifact for the
// happy 200 / 206 GET cases.  `artifact` controls only which telemetry
// counter is incremented; the on-disk layout is identical so a warm
// hit serves the cached entry regardless of original classification.
func (h *Handler) streamCacheable(w http.ResponseWriter, resp *upstream.Response, keyHex, upstreamHost, path, rawQuery, auth string, unlock func(), artifact bool, originPath string) {
	switch resp.Status {
	case http.StatusOK:
		h.streamCacheable200(w, resp, keyHex, upstreamHost, path, rawQuery, auth, unlock, artifact, originPath)
	case http.StatusPartialContent:
		h.streamCacheable206(w, resp, keyHex, upstreamHost, path, rawQuery, auth, unlock, artifact, originPath)
	default:
		// Defense in depth: caller should have filtered, but if we get
		// here just stream-through without recording bytes.
		h.streamMetadata(w, resp)
		if unlock != nil {
			unlock()
		}
	}
}

func (h *Handler) streamCacheable200(w http.ResponseWriter, resp *upstream.Response, keyHex, upstreamHost, path, rawQuery, auth string, unlock func(), artifact bool, originPath string) {
	params := cache.SegmentParams{
		Status:       200,
		UpstreamHost: upstreamHost,
		Path:         path,
		RawQuery:     rawQuery,
		ETag:         resp.Header.Get("ETag"),
		ContentType:  resp.Header.Get("Content-Type"),
		Start:        0,
		Length:       resp.ContentLength, // -1 if chunked
		Total:        resp.ContentLength,
		ExtraHeaders: pickPreservedHeaders(resp.Header),
		OriginPath:   originPath,
	}
	dw, err := h.store.BeginSegment(keyHex, params)
	if err != nil {
		h.log.Warn("begin segment", "err", err)
		_ = resp.Body.Close()
		if unlock != nil {
			unlock()
		}
		if errors.Is(err, cache.ErrQuotaExceeded) {
			telemetry.IncCacheQuotaReject()
			http.Error(w, "cache storage quota exceeded", http.StatusInsufficientStorage)
			return
		}
		http.Error(w, "cache write failed", http.StatusInternalServerError)
		return
	}
	setArtifactHeaders(w, resp.ContentLength)
	w.WriteHeader(http.StatusOK)

	teed := &diskTeeReader{src: resp.Body, disk: dw}
	written, _ := io.Copy(w, teed)
	_ = dw.Close()
	_ = resp.Body.Close()
	if unlock != nil {
		unlock()
	}
	h.maybeWriteRevAlias(http.MethodGet, upstreamHost, path, rawQuery, auth, keyHex, resp.Header)
	if artifact {
		telemetry.AddArtifactUpstreamBytes(written)
		telemetry.IncArtifactUpstreamFetch()
	} else {
		telemetry.AddMetadataUpstreamBytes(written)
		telemetry.IncMetadataUpstreamFetch()
	}
	telemetry.AddClientBytesServed(written)
}

func (h *Handler) streamCacheable206(w http.ResponseWriter, resp *upstream.Response, keyHex, upstreamHost, path, rawQuery, auth string, unlock func(), artifact bool, originPath string) {
	start, end, total, ok := parseContentRangeResponse(resp.Header.Get("Content-Range"))
	if !ok {
		// Unknown Content-Range -- fall through to streaming pass-through.
		h.streamMetadata(w, resp)
		if unlock != nil {
			unlock()
		}
		return
	}
	params := cache.SegmentParams{
		Status:       206,
		UpstreamHost: upstreamHost,
		Path:         path,
		RawQuery:     rawQuery,
		ETag:         resp.Header.Get("ETag"),
		ContentType:  resp.Header.Get("Content-Type"),
		Start:        start,
		Length:       end - start,
		Total:        total, // may be -1 for "*"
		ExtraHeaders: pickPreservedHeaders(resp.Header),
		OriginPath:   originPath,
	}
	dw, err := h.store.BeginSegment(keyHex, params)
	if err != nil {
		h.log.Warn("begin range segment", "err", err)
		_ = resp.Body.Close()
		if unlock != nil {
			unlock()
		}
		if errors.Is(err, cache.ErrQuotaExceeded) {
			telemetry.IncCacheQuotaReject()
			http.Error(w, "cache storage quota exceeded", http.StatusInsufficientStorage)
			return
		}
		http.Error(w, "cache write failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Range", formatContentRange(start, end, total))
	setArtifactHeaders(w, end-start)
	w.WriteHeader(http.StatusPartialContent)

	teed := &diskTeeReader{src: resp.Body, disk: dw}
	written, _ := io.Copy(w, teed)
	_ = dw.Close()
	_ = resp.Body.Close()
	if unlock != nil {
		unlock()
	}
	h.maybeWriteRevAlias(http.MethodGet, upstreamHost, path, rawQuery, auth, keyHex, resp.Header)
	if artifact {
		telemetry.AddArtifactUpstreamBytes(written)
		telemetry.IncArtifactUpstreamFetch()
	} else {
		telemetry.AddMetadataUpstreamBytes(written)
		telemetry.IncMetadataUpstreamFetch()
	}
	telemetry.AddClientBytesServed(written)
}

// setArtifactHeaders applies Content-Length when known; absence triggers
// net/http's chunked encoding.
func setArtifactHeaders(w http.ResponseWriter, contentLength int64) {
	if contentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	} else {
		w.Header().Del("Content-Length")
	}
}

// streamMetadata streams a non-artifact (or non-cacheable artifact)
// response through to the client with byte counting.  Never buffers
// in memory.
func (h *Handler) streamMetadata(w http.ResponseWriter, resp *upstream.Response) {
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	} else {
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(resp.Status)
	written, _ := io.Copy(w, resp.Body)
	telemetry.IncMetadataUpstreamFetch()
	telemetry.AddMetadataUpstreamBytes(written)
	telemetry.AddClientBytesServed(written)
}

func (h *Handler) copyAndRewriteHeaders(dst http.Header, resp *upstream.Response, originPath string) {
	for k, vv := range resp.Header {
		kk := strings.ToLower(k)
		if _, skip := hopResponseHeaders[kk]; skip {
			continue
		}
		// Content-Length is set explicitly later when known.
		if kk == "content-length" {
			continue
		}
		// Location / Content-Location are rewritten below.
		if kk == "location" || kk == "content-location" {
			continue
		}
		// Drop any header NAME containing CR, LF, or NUL (illegal
		// per RFC 7230 3.2; presence indicates an upstream that
		// either had a parser bug or was attempting to inject
		// headers via response-splitting).  We cannot pass these
		// through to the client without giving the downstream LB
		// the same opportunity to be confused.
		if strings.ContainsAny(k, "\r\n\x00") {
			continue
		}
		// Hugging Face advertises Xet (chunk-deduplicated) downloads via
		// a Link header carrying rel="xet-auth" and
		// rel="xet-reconstruction-info".  When present, the python
		// `huggingface_hub` library bypasses the regular `Location:`
		// redirect and talks the Xet protocol directly to
		// `cas-server.xethub.hf.co` -- completely sidestepping our proxy
		// and therefore our cache.
		//
		// Stripping these Link relations forces the client to fall back
		// to the simple Location-following path, which we DO intercept
		// (the presigned cas-bridge.xethub.hf.co URL points at the full
		// file body served as a single HTTP response; we rewrite
		// Location to /_p/ below so the client comes back through us,
		// where streamArtifact tees every byte to disk).  The end
		// result is the same file, just without Xet's chunk-level dedup
		// -- which we don't need because our own disk cache provides
		// the dedup at the (key, range) level.
		if kk == "link" {
			for _, v := range stripXetLinks(vv) {
				if sanitized, ok := sanitiseHeaderValue(v); ok {
					dst.Add(k, sanitized)
				}
			}
			continue
		}
		for _, v := range vv {
			if sanitized, ok := sanitiseHeaderValue(v); ok {
				dst.Add(k, sanitized)
			}
		}
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if nl, ok := rewrite.LocationToProxyWithOrigin(h.cfg.PublicBaseURL, h.allowf, loc, originPath); ok {
			if s, ok := sanitiseHeaderValue(nl); ok {
				dst.Set("Location", s)
			}
		} else {
			if s, ok := sanitiseHeaderValue(loc); ok {
				dst.Set("Location", s)
			}
		}
	}
	if cloc := resp.Header.Get("Content-Location"); cloc != "" {
		if nl, ok := rewrite.LocationToProxyWithOrigin(h.cfg.PublicBaseURL, h.allowf, cloc, originPath); ok {
			if s, ok := sanitiseHeaderValue(nl); ok {
				dst.Set("Content-Location", s)
			}
		} else {
			if s, ok := sanitiseHeaderValue(cloc); ok {
				dst.Set("Content-Location", s)
			}
		}
	}
}

// sanitiseHeaderValue defends against response-splitting via
// upstream-injected CR/LF or NUL bytes.  Per RFC 7230 §3.2.6 a
// header value is *( field-content / obs-fold ) -- bare CR or LF
// are illegal.  An upstream that returns such a value is either
// compromised or running a buggy parser; either way we must not
// pass it through to the client, because the downstream LB or
// browser will interpret the LF as an end-of-header marker and
// the bytes after as a new header / status line.
//
// Returns (cleaned, true) for an unmodified clean value, (cleaned,
// true) for a value where only trailing whitespace was trimmed, and
// ("", false) for a value containing CR / LF / NUL -- the caller
// drops headers we return false for, the safer of the two
// available options (sending an empty header would change request
// semantics in surprising ways).
func sanitiseHeaderValue(v string) (string, bool) {
	if strings.ContainsAny(v, "\r\n\x00") {
		return "", false
	}
	return v, true
}

// ----- Link header helpers (Xet stripping) -------------------------------

// stripXetLinks removes Link header values whose `rel=` is one of the
// Xet-protocol-only relations (xet-auth, xet-reconstruction-info).  It
// preserves all other rel= entries (e.g. rel=next, rel=prev).
//
// A single Link header may carry multiple comma-separated link-values
// per RFC 8288.  We split on commas that appear OUTSIDE angle brackets,
// drop any value carrying a Xet rel, and re-join.  An entry whose rel
// is empty (malformed) is preserved.
func stripXetLinks(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		var kept []string
		for _, part := range splitLinkValues(v) {
			if linkValueIsXet(part) {
				continue
			}
			kept = append(kept, part)
		}
		if len(kept) > 0 {
			out = append(out, strings.Join(kept, ", "))
		}
	}
	return out
}

// splitLinkValues splits a Link header value on commas that are not
// inside angle brackets.  Quotes inside parameters are honored per
// RFC 8288 section 3.
func splitLinkValues(v string) []string {
	var out []string
	depth := 0 // depth inside <> brackets
	inQuote := false
	start := 0
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch c {
		case '<':
			if !inQuote {
				depth++
			}
		case '>':
			if !inQuote && depth > 0 {
				depth--
			}
		case '"':
			inQuote = !inQuote
		case ',':
			if depth == 0 && !inQuote {
				out = append(out, strings.TrimSpace(v[start:i]))
				start = i + 1
			}
		}
	}
	if start <= len(v) {
		out = append(out, strings.TrimSpace(v[start:]))
	}
	return out
}

// linkValueIsXet reports whether a single link-value carries one of the
// Xet rel parameters we want to strip.
func linkValueIsXet(part string) bool {
	rel := strings.ToLower(part)
	idx := strings.Index(rel, "rel=")
	if idx < 0 {
		return false
	}
	rest := rel[idx+4:]
	if len(rest) > 0 && rest[0] == '"' {
		rest = rest[1:]
		if end := strings.IndexByte(rest, '"'); end >= 0 {
			rest = rest[:end]
		}
	} else {
		if end := strings.IndexAny(rest, ";,"); end >= 0 {
			rest = rest[:end]
		}
	}
	for _, tok := range strings.Fields(rest) {
		if strings.HasPrefix(strings.ToLower(tok), "xet-") {
			return true
		}
	}
	return false
}

// diskTeeReader implements io.Reader by reading from src and writing the
// bytes through to disk on the same call.  Used in the cold artifact
// path so streaming to the client and persisting to disk happen in a
// single pass (no double buffering).
//
// Disk write errors abort the read -- we never want the client to
// receive bytes that we failed to durably persist.
type diskTeeReader struct {
	src  io.Reader
	disk io.Writer
}

func (t *diskTeeReader) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		if _, werr := t.disk.Write(p[:n]); werr != nil {
			return n, werr
		}
	}
	return n, err
}
