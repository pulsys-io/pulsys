// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pulsys-io/pulsys/internal/blobstore"
	"github.com/pulsys-io/pulsys/internal/registry"
)

// RegistryHandler serves the Hugging Face wire protocol from the
// local Pulsys registry (Postgres tables + blobstore). It is the
// outer handler: incoming requests are resolved against the registry
// first; on a registry miss it consults the `mirrors` table and, if
// the repo is mirrored, hands the request to `next` (the existing
// upstream/cache proxy) with the mirror's upstream_host pinned. A
// non-mirrored miss is a 404 - the v1 product is "explicit mirror
// only".
//
// The handler is wire-compatible with huggingface_hub: model info,
// tree, paths-info, resolve (GET + HEAD + Range), preupload, commit,
// LFS batch + PUT + verify, and POST /api/repos/create all return
// the same JSON / headers a real Hub does, so existing clients work
// against HF_ENDPOINT=https://pulsys.acme.com with no changes.
type RegistryHandler struct {
	Store     *registry.Store
	Blobs     blobstore.Store
	TenantID  string
	Next      http.Handler // mirror passthrough + existing cache
	PublicURL string       // used to emit absolute commit / LFS URLs

	// AuditPool is an optional pgxpool used for emitting audit_log
	// rows on successful commits. Nil disables audit emission (the
	// test harness sets this when it wants the assertion).
	AuditPool registry.AuditExecer

	// LFSMaxBytes bounds the body size accepted by PUT
	// /lfs-storage/{oid}.  Zero means "use defaultLFSMaxBytes
	// (200 GiB)".  A request whose Content-Length exceeds the
	// cap is rejected with 413 BEFORE we open any blobstore
	// state; a streaming upload that overruns the cap mid-flight
	// is truncated and rejected with 413.  Without this cap any
	// authenticated client could fill the proxy's disk with a
	// single PUT (the pre-Phase-5 behavior); for enterprise
	// deployments the LB does not typically gate body size at
	// this granularity, so the proxy owns the invariant.
	LFSMaxBytes int64

	// CommitMaxBytes bounds the NDJSON body size accepted by
	// POST /api/{type}/{repo}/commit/{rev}.  Zero means "use
	// defaultCommitMaxBytes (64 MiB)".  Closes the allocator-
	// amplification class hardened against in CVE-2025-58185
	// (encoding/asn1) and the HTTP/2 CONTINUATION-flood family
	// (VU#421644): an attacker who can authenticate could
	// otherwise submit an arbitrarily large commit body
	// containing thousands of base64-encoded inline files and
	// blow the proxy's heap before any size validation runs.
	//
	// A legitimate commit carries small text payloads (model
	// cards, configs, code) inline; multi-MiB binaries go via
	// LFS PUT and arrive here only as small JSON pointers.
	// 64 MiB has ample headroom for "huggingface-cli upload
	// --include='*.py'" on a large repo.  Operators with
	// unusual workloads can raise it; the cap is enforced
	// strictly (no chunked-stream loophole) via
	// http.MaxBytesReader so a lying Content-Length still
	// surfaces 413 mid-stream.
	CommitMaxBytes int64
}

// defaultLFSMaxBytes is the LFS PUT body cap when
// RegistryHandler.LFSMaxBytes is unset.  200 GiB accommodates
// the largest single safetensors shard we expect to see in
// practice (modern open-weight checkpoints top out around
// 100 GiB per shard) with 2x headroom, while still rejecting a
// runaway client that tries to upload several TiB of garbage
// in one request.  Operators with legitimate larger shards
// should set LFSMaxBytes explicitly.
const defaultLFSMaxBytes = 200 << 30

// defaultCommitMaxBytes is the NDJSON commit body cap when
// RegistryHandler.CommitMaxBytes is unset.  64 MiB matches the
// per-line scanner buffer (so a single legitimate giant base64
// inline file still parses) while bounding the absolute heap
// cost of one commit.
const defaultCommitMaxBytes = 64 << 20

// ServeHTTP routes the request. Unknown paths fall through to Next so
// /admin, /auth, /metrics, /healthz, etc. keep working.
func (h *RegistryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health / admin / auth / metrics are not HF-wire paths; they go
	// straight to the inner handler.
	if r.URL.Path == "/healthz" ||
		strings.HasPrefix(r.URL.Path, "/admin/") ||
		strings.HasPrefix(r.URL.Path, "/auth/") ||
		r.URL.Path == "/metrics" {
		h.Next.ServeHTTP(w, r)
		return
	}

	switch {
	case strings.HasPrefix(r.URL.Path, "/api/"):
		h.serveAPI(w, r)
	case strings.HasPrefix(r.URL.Path, "/lfs-storage/"):
		h.serveLFSStorage(w, r)
	case strings.Contains(r.URL.Path, ".git/info/lfs/"):
		h.serveLFSGit(w, r)
	default:
		h.serveResolve(w, r)
	}
}

// ---- API: model info / tree / paths-info / preupload / commit / repo create ----

func (h *RegistryHandler) serveAPI(w http.ResponseWriter, r *http.Request) {
	// huggingface_hub.create_repo POSTs to /api/repos/create with
	// JSON {name, type, private}.
	if r.URL.Path == "/api/repos/create" && r.Method == http.MethodPost {
		h.createRepo(w, r)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	if len(parts) < 3 {
		h.Next.ServeHTTP(w, r)
		return
	}
	repoType := parts[0]
	switch repoType {
	case "models", "datasets", "spaces":
	default:
		h.Next.ServeHTTP(w, r)
		return
	}
	ns := parts[1]
	name := parts[2]

	switch {
	case len(parts) == 3 && r.Method == http.MethodGet:
		h.modelInfo(w, r, repoType, ns, name)
	case len(parts) >= 5 && parts[3] == "tree" && r.Method == http.MethodGet:
		rev := parts[4]
		h.tree(w, r, repoType, ns, name, rev)
	case len(parts) >= 5 && parts[3] == "paths-info" && r.Method == http.MethodPost:
		rev := parts[4]
		h.pathsInfo(w, r, repoType, ns, name, rev)
	case len(parts) >= 5 && parts[3] == "preupload" && r.Method == http.MethodPost:
		rev := parts[4]
		h.preupload(w, r, repoType, ns, name, rev)
	case len(parts) >= 5 && parts[3] == "commit" && r.Method == http.MethodPost:
		rev := parts[4]
		h.commit(w, r, repoType, ns, name, rev)
	default:
		h.Next.ServeHTTP(w, r)
	}
}

func (h *RegistryHandler) modelInfo(w http.ResponseWriter, r *http.Request, repoType, ns, name string) {
	repo, sha, files, ok := h.lookupHead(r.Context(), repoType, ns, name)
	if !ok {
		h.fallbackOrNotFound(w, r, repoType, ns, name)
		return
	}
	siblings := make([]siblingPayload, 0, len(files))
	for _, p := range sortedKeys(files) {
		f := files[p]
		s := siblingPayload{RFilename: f.Path, Size: f.Size, BlobID: f.BlobOID}
		if f.IsLFS {
			s.LFS = &lfsPayload{OID: f.BlobOID, Size: f.Size, PointerSize: 134}
		}
		siblings = append(siblings, s)
	}
	writeJSON(w, modelInfoPayload{
		ID:       repo.FullName(),
		ModelID:  repo.FullName(),
		SHA:      sha,
		Tags:     []string{},
		Siblings: siblings,
		Private:  repo.Private,
	})
}

func (h *RegistryHandler) tree(w http.ResponseWriter, r *http.Request, repoType, ns, name, rev string) {
	repo, err := h.Store.GetRepo(r.Context(), h.TenantID, repoType, ns, name)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			h.fallbackOrNotFound(w, r, repoType, ns, name)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "store: "+err.Error())
		return
	}
	sha, err := h.Store.ResolveBranch(r.Context(), repo.ID, rev)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "revision not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "resolve: "+err.Error())
		return
	}
	files, _, err := h.Store.ListFiles(r.Context(), repo.ID, sha)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list: "+err.Error())
		return
	}
	out := make([]treePayload, 0, len(files))
	for _, p := range sortedKeys(files) {
		f := files[p]
		te := treePayload{Type: "file", OID: f.BlobOID, Size: f.Size, Path: f.Path}
		if f.IsLFS {
			te.LFS = &lfsPayload{OID: f.BlobOID, Size: f.Size, PointerSize: 134}
		}
		out = append(out, te)
	}
	writeJSON(w, out)
}

func (h *RegistryHandler) pathsInfo(w http.ResponseWriter, r *http.Request, repoType, ns, name, rev string) {
	repo, err := h.Store.GetRepo(r.Context(), h.TenantID, repoType, ns, name)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			h.fallbackOrNotFound(w, r, repoType, ns, name)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "store: "+err.Error())
		return
	}
	sha, err := h.Store.ResolveBranch(r.Context(), repo.ID, rev)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "revision not found")
		return
	}
	files, c, err := h.Store.ListFiles(r.Context(), repo.ID, sha)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list: "+err.Error())
		return
	}
	var req pathsInfoPayloadReq
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}
	out := make([]pathsInfoPayloadEntry, 0, len(req.Paths))
	for _, p := range req.Paths {
		f, ok := files[p]
		if !ok {
			continue
		}
		entry := pathsInfoPayloadEntry{
			Path: f.Path, Size: f.Size, Type: "file", OID: f.BlobOID,
		}
		if f.IsLFS {
			entry.LFS = &lfsPayload{OID: f.BlobOID, Size: f.Size, PointerSize: 134}
		}
		entry.LastCommit = &commitRef{ID: c.SHA, Date: c.CreatedAt.UTC().Format(time.RFC3339)}
		out = append(out, entry)
	}
	writeJSON(w, out)
}

// ---- resolve (download / HEAD) ----

func (h *RegistryHandler) serveResolve(w http.ResponseWriter, r *http.Request) {
	repoType, ns, name, rev, path, ok := parseResolvePath(r.URL.Path)
	if !ok {
		h.Next.ServeHTTP(w, r)
		return
	}
	repo, err := h.Store.GetRepo(r.Context(), h.TenantID, repoType, ns, name)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			h.fallbackOrNotFound(w, r, repoType, ns, name)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "store: "+err.Error())
		return
	}
	fr, c, err := h.Store.LookupFile(r.Context(), repo.ID, rev, path)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "file not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "lookup: "+err.Error())
		return
	}

	body, _, err := h.Blobs.Open(r.Context(), fr.BlobOID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "blob open: "+err.Error())
		return
	}
	defer func() { _ = body.Close() }()

	etag := `"` + fr.BlobOID + `"`
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Repo-Commit", c.SHA)
	w.Header().Set("X-Linked-Etag", etag)
	w.Header().Set("X-Linked-Size", strconv.FormatInt(fr.Size, 10))
	w.Header().Set("Accept-Ranges", "bytes")

	rangeHdr := r.Header.Get("Range")
	if rangeHdr == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(fr.Size, 10))
		if r.Method == http.MethodHead {
			// Force the headers out before returning so net/http
			// doesn't override our Content-Length with 0 when no
			// body bytes are written.
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.Copy(w, body)
		return
	}

	start, end, ok := parseSimpleRange(rangeHdr, fr.Size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", fr.Size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fr.Size))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := body.Seek(start, io.SeekStart); err != nil {
		return
	}
	_, _ = io.CopyN(w, body, end-start+1)
}

// ---- mirror fallback ----

// fallbackOrNotFound checks the mirrors table for the requested repo
// and either forwards to Next (when mirrored) or 404s.
func (h *RegistryHandler) fallbackOrNotFound(w http.ResponseWriter, r *http.Request, repoType, ns, name string) {
	if _, err := h.Store.GetMirror(r.Context(), h.TenantID, repoType, ns, name); err == nil {
		// Mirror declared -> hand the request to the existing
		// proxy / upstream / cache layer. The inner handler already
		// honors -default-upstream-host; per-repo upstream override
		// would land here in a future iteration.
		h.Next.ServeHTTP(w, r)
		return
	}
	writeJSONError(w, http.StatusNotFound, fmt.Sprintf("%s/%s/%s not found", repoType, ns, name))
}

// lookupHead resolves (repo, HEAD of main, files) in one go.
func (h *RegistryHandler) lookupHead(ctx context.Context, repoType, ns, name string) (registry.Repo, string, map[string]registry.FileRevision, bool) {
	repo, err := h.Store.GetRepo(ctx, h.TenantID, repoType, ns, name)
	if err != nil {
		return registry.Repo{}, "", nil, false
	}
	sha, err := h.Store.ResolveBranch(ctx, repo.ID, "main")
	if err != nil {
		// Repo exists but has no commits yet: return an empty file
		// list under the canonical "no commits" sentinel.
		return repo, "0000000000000000000000000000000000000000", map[string]registry.FileRevision{}, true
	}
	files, _, err := h.Store.ListFiles(ctx, repo.ID, sha)
	if err != nil {
		return registry.Repo{}, "", nil, false
	}
	return repo, sha, files, true
}

// ---- payload types ----

type modelInfoPayload struct {
	ID       string           `json:"id"`
	ModelID  string           `json:"modelId"`
	SHA      string           `json:"sha"`
	Tags     []string         `json:"tags"`
	Siblings []siblingPayload `json:"siblings"`
	Private  bool             `json:"private"`
}

type siblingPayload struct {
	RFilename string      `json:"rfilename"`
	Size      int64       `json:"size"`
	BlobID    string      `json:"blob_id"`
	LFS       *lfsPayload `json:"lfs,omitempty"`
}

type treePayload struct {
	Type string      `json:"type"`
	OID  string      `json:"oid"`
	Size int64       `json:"size"`
	Path string      `json:"path"`
	LFS  *lfsPayload `json:"lfs,omitempty"`
}

type lfsPayload struct {
	OID         string `json:"oid"`
	Size        int64  `json:"size"`
	PointerSize int    `json:"pointerSize"`
}

type pathsInfoPayloadReq struct {
	Paths []string `json:"paths"`
}

type pathsInfoPayloadEntry struct {
	Path       string      `json:"path"`
	Size       int64       `json:"size"`
	Type       string      `json:"type"`
	OID        string      `json:"oid"`
	LFS        *lfsPayload `json:"lfs,omitempty"`
	LastCommit *commitRef  `json:"lastCommit,omitempty"`
}

type commitRef struct {
	ID   string `json:"id"`
	Date string `json:"date"`
}

// ---- helpers ----

func sortedKeys(m map[string]registry.FileRevision) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// parseResolvePath parses /[<repo_type>/]<org>/<name>/resolve/<rev>/<path>.
// Returns (repoType, ns, name, rev, path, ok).
func parseResolvePath(p string) (repoType, ns, name, rev, path string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	repoType = "models"
	if len(parts) > 0 && (parts[0] == "datasets" || parts[0] == "spaces") {
		repoType = parts[0]
		parts = parts[1:]
	}
	if len(parts) < 5 || parts[2] != "resolve" {
		return "", "", "", "", "", false
	}
	return repoType, parts[0], parts[1], parts[3], strings.Join(parts[4:], "/"), true
}

// parseSimpleRange parses a single "bytes=START-END" range. Returns
// (start, end, ok). Open-ended forms (`bytes=N-`, `bytes=-N`) and
// suffix ranges are supported.
func parseSimpleRange(h string, size int64) (start, end int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(h, prefix) {
		return 0, 0, false
	}
	r := strings.TrimPrefix(h, prefix)
	if strings.Contains(r, ",") {
		return 0, 0, false
	}
	dash := strings.IndexByte(r, '-')
	if dash < 0 {
		return 0, 0, false
	}
	a, b := r[:dash], r[dash+1:]
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 || n > size {
			return 0, 0, false
		}
		return size - n, size - 1, true
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if b == "" {
		return start, size - 1, true
	}
	end, err = strconv.ParseInt(b, 10, 64)
	if err != nil || end < start || end >= size {
		return 0, 0, false
	}
	return start, end, true
}
