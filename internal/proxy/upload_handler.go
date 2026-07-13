// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pulsys-io/pulsys/internal/blobstore"
	"github.com/pulsys-io/pulsys/internal/registry"
)

// isSerializationFailure returns true for Postgres 40001 errors,
// which are how SERIALIZABLE surfaces the loser of two concurrent
// commits. The whole point of SERIALIZABLE is that the loser MUST
// retry - the database guarantees correctness, not progress.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001"
}

// commitWithRetry calls CommitTx, retrying SERIALIZABLE failures
// (40001) with jittered exponential backoff. Caps at ~3.2s total
// wall time across 12 attempts - the user already paid for a
// commit, we'd rather complete than 500 them.
//
// Stops early on ctx cancellation so a dropped client doesn't keep
// hammering the database.
func (h *RegistryHandler) commitWithRetry(ctx context.Context, in registry.CommitInput) (registry.CommitResult, error) {
	const maxAttempts = 12
	const baseBackoff = 5 * time.Millisecond
	const capBackoff = 250 * time.Millisecond

	var (
		res registry.CommitResult
		err error
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err = ctx.Err(); err != nil {
			return res, err
		}
		res, err = h.Store.CommitTx(ctx, in)
		if err == nil {
			return res, nil
		}
		if !isSerializationFailure(err) {
			return res, err
		}
		// Backoff = min(cap, base*2^attempt) with full jitter so two
		// goroutines never wake on the same window.
		wait := baseBackoff << attempt
		if wait > capBackoff {
			wait = capBackoff
		}
		// rand.Int64N panics on <= 0; guard for the (impossible) case.
		if wait > 0 {
			wait = time.Duration(rand.Int64N(int64(wait)))
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return res, ctx.Err()
		case <-t.C:
		}
	}
	return res, err
}

// HF write surface. Each handler mirrors the wire shape
// huggingface_hub uses so existing tools work unchanged against
// HF_ENDPOINT=https://pulsys.acme.com.

// ---- POST /api/repos/create ----

// createRepoRequest mirrors huggingface_hub.HfApi.create_repo.
type createRepoRequest struct {
	Name         string `json:"name"`
	Organization string `json:"organization,omitempty"`
	Type         string `json:"type"` // model | dataset | space (singular)
	Private      bool   `json:"private,omitempty"`
}

func (h *RegistryHandler) createRepo(w http.ResponseWriter, r *http.Request) {
	var req createRepoRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	repoType, ok := repoTypePlural(req.Type)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "type must be model|dataset|space")
		return
	}

	ns, name := splitRepoName(req.Name, req.Organization)
	if ns == "" || name == "" {
		writeJSONError(w, http.StatusBadRequest, "name required")
		return
	}

	repo, err := h.Store.CreateRepo(r.Context(), h.TenantID, repoType, ns, name, req.Private, "")
	if err != nil {
		if errors.Is(err, registry.ErrAlreadyExists) {
			// HF returns 200 with the existing repo for exist_ok=True;
			// the client always passes that flag. We mirror that.
			repo, err2 := h.Store.GetRepo(r.Context(), h.TenantID, repoType, ns, name)
			if err2 != nil {
				writeJSONError(w, http.StatusInternalServerError, err2.Error())
				return
			}
			writeJSON(w, map[string]any{
				"url":  fmt.Sprintf("%s/%s", h.publicURL(), repo.FullName()),
				"name": repo.FullName(),
			})
			return
		}
		if errors.Is(err, registry.ErrInvalidInput) {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"url":  fmt.Sprintf("%s/%s", h.publicURL(), repo.FullName()),
		"name": repo.FullName(),
	})
}

// ---- POST /api/{type}/{repo}/preupload/{rev} ----

type preuploadFileReq struct {
	Path   string `json:"path"`
	Sample string `json:"sample,omitempty"`
	Size   int64  `json:"size"`
}

type preuploadReq struct {
	Files []preuploadFileReq `json:"files"`
}

type preuploadFileResp struct {
	Path         string `json:"path"`
	UploadMode   string `json:"uploadMode"`   // regular | lfs
	ShouldIgnore bool   `json:"shouldIgnore"` // dedup hit
	OID          string `json:"oid,omitempty"`
}

type preuploadResp struct {
	Files []preuploadFileResp `json:"files"`
}

// preupload returns per-file upload mode (inline vs lfs) and a dedup
// hint. Dedup is path-based here ("we already have a file at this
// path in HEAD"); the LFS batch endpoint performs the stronger
// content-hash dedup for big blobs.
func (h *RegistryHandler) preupload(w http.ResponseWriter, r *http.Request, repoType, ns, name, _ string) {
	repo, err := h.Store.GetRepo(r.Context(), h.TenantID, repoType, ns, name)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "repo not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var req preuploadReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}

	// Resolve HEAD once for dedup; empty repos have no files.
	var current map[string]registry.FileRevision
	if sha, err := h.Store.ResolveBranch(r.Context(), repo.ID, "main"); err == nil {
		current, _, _ = h.Store.ListFiles(r.Context(), repo.ID, sha)
	}

	out := preuploadResp{Files: make([]preuploadFileResp, 0, len(req.Files))}
	for _, f := range req.Files {
		mode := "regular"
		if f.Size > lfsThreshold || hasLFSExtension(f.Path) {
			mode = "lfs"
		}
		resp := preuploadFileResp{Path: f.Path, UploadMode: mode}
		if existing, ok := current[f.Path]; ok && existing.Size == f.Size {
			resp.ShouldIgnore = true
			resp.OID = existing.BlobOID
		}
		out.Files = append(out.Files, resp)
	}
	writeJSON(w, out)
}

// lfsThreshold is the size at which Pulsys hands off to LFS. HF uses
// 10 MiB as the default; we match.
const lfsThreshold = 10 << 20

// hasLFSExtension returns true for file extensions HF always tracks
// with LFS regardless of size (.bin, .safetensors, .gguf, etc.).
// Keeping the list short prevents surprises for tiny model files
// during tests; the size threshold is the real switch.
func hasLFSExtension(path string) bool {
	for _, ext := range []string{
		".safetensors", ".gguf", ".onnx", ".pth", ".pt", ".bin",
		".ckpt", ".tar", ".zip", ".7z", ".msgpack", ".npz",
	} {
		if strings.HasSuffix(strings.ToLower(path), ext) {
			return true
		}
	}
	return false
}

// ---- POST /api/{type}/{repo}/commit/{rev} ----

// commit applies an NDJSON commit body atomically against the
// registry.
//
// The wire format (huggingface_hub `commit` action):
//
//	{"key":"header", "value":{"summary":"...", "description":"..."}}
//	{"key":"file",  "value":{"path":"...", "encoding":"utf-8|base64", "content":"..."}}
//	{"key":"lfsFile","value":{"path":"...", "oid":"...", "size":N, "algo":"sha256"}}
//	{"key":"deletedFile","value":{"path":"..."}}
//
// Inline file bodies are decoded, hashed, and persisted via the
// blobstore here. LFS pointers reference blobs that were already
// PUT via /lfs-storage/{oid}; commit just adds the file_revisions
// row. The full transaction lands in registry.CommitTx, which is
// SERIALIZABLE.
func (h *RegistryHandler) commit(w http.ResponseWriter, r *http.Request, repoType, ns, name, rev string) {
	repo, err := h.Store.GetRepo(r.Context(), h.TenantID, repoType, ns, name)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "repo not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Cap the commit body BEFORE parsing.  http.MaxBytesReader
	// surfaces a *http.MaxBytesError when the cap trips, which
	// the scanner inside parseCommitNDJSON will report through
	// scanner.Err().  We map it to 413 so an enterprise client
	// gets the same signal whether the body exceeded the cap on
	// the very first byte (Content-Length fast path inside
	// MaxBytesReader) or while streaming (chunked / lying CL).
	//
	// Default 64 MiB; the cap is configurable via
	// RegistryHandler.CommitMaxBytes for unusual workloads.
	commitCap := h.CommitMaxBytes
	if commitCap == 0 {
		commitCap = defaultCommitMaxBytes
	}
	if commitCap > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, commitCap)
	}
	summary, desc, inlineRaw, lfsPointers, deletes, err := parseCommitNDJSON(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("commit body exceeds %d byte cap", commitCap))
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Materialize inline content into the blobstore. Each file is
	// hashed; the oid + size become the file_revisions row's
	// blob reference. We do this BEFORE the registry transaction so
	// a failed Postgres commit leaves only orphan blobs (GC-able)
	// rather than dangling file_revisions.
	inline := make(map[string]registry.InlineCommitFile, len(inlineRaw))
	for path, body := range inlineRaw {
		st, err := h.Blobs.Put(r.Context(), bytes.NewReader(body), blobstore.PutOptions{})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "blob put: "+err.Error())
			return
		}
		if err := h.Store.UpsertBlob(r.Context(), st.OID, st.Size, st.StorageURL); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "upsert blob: "+err.Error())
			return
		}
		inline[path] = registry.InlineCommitFile{BlobOID: st.OID, Size: st.Size}
	}

	// CommitTx runs at SERIALIZABLE; concurrent commits against the
	// same branch surface as serialization_failure (40001). The
	// proxy absorbs contention with jittered exponential backoff so
	// HF clients (which do NOT retry 40001) see a clean 200.
	commitInput := registry.CommitInput{
		RepoID:      repo.ID,
		Branch:      rev,
		Summary:     summary,
		Description: desc,
		Inline:      inline,
		LFSPointers: lfsPointers,
		Deletes:     deletes,
	}
	res, commitErr := h.commitWithRetry(r.Context(), commitInput)
	if commitErr != nil {
		switch {
		case errors.Is(commitErr, registry.ErrBlobMissing):
			writeJSONError(w, http.StatusBadRequest, "LFS object not uploaded: "+commitErr.Error())
		case errors.Is(commitErr, registry.ErrLFSSizeMismatch):
			// 422: client's NDJSON declared a size for an LFS
			// pointer that contradicts the size we recorded at
			// upload time.  Same status class as the LFS PUT
			// OID mismatch above (both are "request shape is
			// well-formed but its content is wrong").
			writeJSONError(w, http.StatusUnprocessableEntity, commitErr.Error())
		case errors.Is(commitErr, registry.ErrCommitPathInvalid):
			writeJSONError(w, http.StatusBadRequest, commitErr.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, "commit: "+commitErr.Error())
		}
		return
	}

	h.audit(r, "upload.commit", repoType+"/"+repo.FullName()+"@"+res.SHA,
		map[string]any{
			"sha":       res.SHA,
			"branch":    res.Branch,
			"summary":   summary,
			"added":     len(inline) + len(lfsPointers),
			"deleted":   len(deletes),
			"inline":    len(inline),
			"lfs_files": len(lfsPointers),
		})

	writeJSON(w, map[string]any{
		"success":   true,
		"commitOid": res.SHA,
		"commitUrl": fmt.Sprintf("%s/%s/commit/%s", h.publicURL(), repo.FullName(), res.SHA),
	})
}

// parseCommitNDJSON consumes an NDJSON commit body into structured
// parts. Returns (summary, description, inline files (path -> raw
// bytes), LFS pointers (path -> spec), deleted paths).
func parseCommitNDJSON(body io.Reader) (summary, desc string,
	inline map[string][]byte, lfsPtr map[string]registry.LFSCommitFile,
	deletes []string, err error) {
	inline = make(map[string][]byte)
	lfsPtr = make(map[string]registry.LFSCommitFile)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		// When http.MaxBytesReader fires, bufio.Scanner's
		// default ScanLines split function returns the final
		// partial-line bytes as a token (atEOF=true path).
		// json.Unmarshal on that truncation surfaces a
		// confusing "unexpected end of JSON input" error
		// that does NOT unwrap to *http.MaxBytesError, so
		// the handler would map it to 400.  Detect the
		// reader-side error here and prefer it; the size cap
		// is the real failure.
		if scanErr := scanner.Err(); scanErr != nil {
			err = scanErr
			return
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if err = json.Unmarshal(line, &env); err != nil {
			// Re-check: a partial final token may have
			// caused this JSON error.  If so, surface the
			// MaxBytesError so the handler returns 413.
			if scanErr := scanner.Err(); scanErr != nil {
				err = scanErr
				return
			}
			err = fmt.Errorf("bad commit envelope: %w", err)
			return
		}
		switch env.Key {
		case "header":
			var v struct {
				Summary     string `json:"summary"`
				Description string `json:"description"`
			}
			if err = json.Unmarshal(env.Value, &v); err != nil {
				return
			}
			summary, desc = v.Summary, v.Description
		case "file":
			var v struct {
				Path     string `json:"path"`
				Encoding string `json:"encoding"`
				Content  string `json:"content"`
			}
			if err = json.Unmarshal(env.Value, &v); err != nil {
				return
			}
			var bodyBytes []byte
			switch v.Encoding {
			case "", "utf-8":
				bodyBytes = []byte(v.Content)
			case "base64":
				bodyBytes, err = base64.StdEncoding.DecodeString(v.Content)
				if err != nil {
					return
				}
			default:
				err = fmt.Errorf("unknown encoding %q", v.Encoding)
				return
			}
			inline[v.Path] = bodyBytes
		case "lfsFile":
			var v struct {
				Path string `json:"path"`
				OID  string `json:"oid"`
				Size int64  `json:"size"`
				Algo string `json:"algo"`
			}
			if err = json.Unmarshal(env.Value, &v); err != nil {
				return
			}
			lfsPtr[v.Path] = registry.LFSCommitFile{BlobOID: v.OID, Size: v.Size}
		case "deletedFile", "deletedFolder":
			var v struct {
				Path string `json:"path"`
			}
			if err = json.Unmarshal(env.Value, &v); err != nil {
				return
			}
			deletes = append(deletes, v.Path)
		default:
			// Unknown keys (copyFile, etc.) are silently skipped to
			// keep the handler forward-compatible with new HF features.
		}
	}
	if err = scanner.Err(); err != nil {
		return
	}
	if summary == "" {
		err = errors.New("commit header required (summary)")
	}
	return
}

// ---- LFS batch ----

type lfsBatchReq struct {
	Operation string        `json:"operation"`
	Transfers []string      `json:"transfers"`
	Objects   []lfsBatchObj `json:"objects"`
}

type lfsBatchObj struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type lfsBatchResp struct {
	Transfer string            `json:"transfer"`
	Objects  []lfsBatchObjResp `json:"objects"`
	HashAlgo string            `json:"hash_algo,omitempty"`
}

type lfsBatchObjResp struct {
	OID     string               `json:"oid"`
	Size    int64                `json:"size"`
	Actions map[string]lfsAction `json:"actions,omitempty"`
	Error   *lfsObjectError      `json:"error,omitempty"`
}

type lfsAction struct {
	Href   string            `json:"href"`
	Header map[string]string `json:"header,omitempty"`
}

type lfsObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// serveLFSGit handles POST /{org}/{name}.git/info/lfs/objects/batch
// and POST /{org}/{name}.git/info/lfs/verify.
func (h *RegistryHandler) serveLFSGit(w http.ResponseWriter, r *http.Request) {
	idx := strings.Index(r.URL.Path, ".git/info/lfs/")
	if idx < 0 {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	head := strings.TrimPrefix(r.URL.Path[:idx], "/")
	tail := r.URL.Path[idx+len(".git/info/lfs/"):]
	parts := strings.Split(head, "/")
	if len(parts) != 2 {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	ns, name := parts[0], parts[1]
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	switch tail {
	case "objects/batch":
		h.lfsBatch(w, r, "models", ns, name)
	case "verify":
		h.lfsVerify(w, r, "models", ns, name)
	default:
		writeJSONError(w, http.StatusNotFound, "not found")
	}
}

// lfsBatch returns presigned PUT URLs for objects that aren't yet in
// the blobstore. The URLs point at our own /lfs-storage/{oid}
// endpoint so the proxy stays the single entry point - no S3
// presigning needed in the registry mode.
func (h *RegistryHandler) lfsBatch(w http.ResponseWriter, r *http.Request, repoType, ns, name string) {
	if _, err := h.Store.GetRepo(r.Context(), h.TenantID, repoType, ns, name); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "repo not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req lfsBatchReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	resp := lfsBatchResp{Transfer: "basic", HashAlgo: "sha256"}
	for _, obj := range req.Objects {
		entry := lfsBatchObjResp{OID: obj.OID, Size: obj.Size}
		switch req.Operation {
		case "upload":
			if _, present, err := h.Store.HasBlob(r.Context(), obj.OID); err != nil {
				entry.Error = &lfsObjectError{Code: 500, Message: err.Error()}
			} else if present {
				// Already uploaded; HF clients treat an empty actions
				// map as "no work to do for this object." We still
				// surface a verify URL so the client can confirm.
				entry.Actions = map[string]lfsAction{}
			} else {
				entry.Actions = map[string]lfsAction{
					"upload": {
						Href:   fmt.Sprintf("%s/lfs-storage/%s", h.publicURL(), obj.OID),
						Header: map[string]string{"Content-Type": "application/octet-stream"},
					},
					"verify": {
						Href: fmt.Sprintf("%s/%s/%s.git/info/lfs/verify", h.publicURL(), ns, name),
					},
				}
			}
		case "download":
			if _, present, err := h.Store.HasBlob(r.Context(), obj.OID); err != nil {
				entry.Error = &lfsObjectError{Code: 500, Message: err.Error()}
			} else if !present {
				entry.Error = &lfsObjectError{Code: 404, Message: "object not found"}
			} else {
				entry.Actions = map[string]lfsAction{
					"download": {Href: fmt.Sprintf("%s/lfs-storage/%s", h.publicURL(), obj.OID)},
				}
			}
		default:
			entry.Error = &lfsObjectError{Code: 400, Message: "unsupported operation"}
		}
		resp.Objects = append(resp.Objects, entry)
	}
	writeJSON(w, resp)
}

// lfsVerify confirms an oid + size is present in the registry's blob
// table after the client finishes the PUT.
func (h *RegistryHandler) lfsVerify(w http.ResponseWriter, r *http.Request, repoType, ns, name string) {
	if _, err := h.Store.GetRepo(r.Context(), h.TenantID, repoType, ns, name); err != nil {
		writeJSONError(w, http.StatusNotFound, "repo not found")
		return
	}
	var req struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	b, present, err := h.Store.HasBlob(r.Context(), req.OID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !present {
		writeJSONError(w, http.StatusNotFound, "object not uploaded")
		return
	}
	if req.Size != 0 && b.Size != req.Size {
		writeJSONError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("size mismatch: registry %d client %d", b.Size, req.Size))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- PUT /lfs-storage/{oid} ----

// serveLFSStorage streams r.Body directly into the blobstore. No
// ReadAll. The blobstore verifies the declared oid against the
// computed sha256 - a mis-uploaded object can never reach the blobs
// table under the wrong key. After verification we UpsertBlob so the
// next LFS verify call resolves it.
//
// Body-size cap: the request body is bounded by
// h.lfsMaxBytes() (default 200 GiB) via an io.LimitReader
// wrapper.  A request whose declared Content-Length exceeds
// the cap is rejected with 413 before any blobstore I/O.  A
// streaming upload that runs past the cap mid-flight produces
// errBodyTooLarge after the limit reader returns the truncated
// stream's last byte; the blobstore then fails the OID/size
// check and we map the result to 413.  See LFSMaxBytes field
// doc for the threat model.  Plan:
func (h *RegistryHandler) serveLFSStorage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	oid := strings.TrimPrefix(r.URL.Path, "/lfs-storage/")
	if oid == "" || strings.ContainsRune(oid, '/') {
		writeJSONError(w, http.StatusBadRequest, "missing oid")
		return
	}

	maxBytes := h.lfsMaxBytes()
	// Reject obvious oversize uploads BEFORE we open the
	// blobstore.  The Content-Length check is a fast-path; an
	// uncooperative client that lies about CL (or omits it,
	// e.g. chunked) is caught by the LimitReader below.
	if cl := r.ContentLength; cl > maxBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			"upload exceeds maximum LFS object size")
		return
	}
	// Wrap the body so a streaming upload past the cap is
	// truncated and surfaced as a short read to the blobstore.
	// We read up to maxBytes+1 bytes so a body of EXACTLY
	// maxBytes succeeds while a body of maxBytes+1 trips the
	// overrun check.
	limited := &lfsLimitReader{R: r.Body, N: maxBytes + 1}

	opts := blobstore.PutOptions{ExpectedOID: oid}
	if cl := r.ContentLength; cl > 0 {
		opts.ExpectedSize = cl
	}

	stat, err := h.Blobs.Put(r.Context(), limited, opts)
	if err != nil {
		switch {
		case limited.Tripped:
			// Body overran the cap mid-flight (chunked uploader
			// or lying Content-Length).  Return 413 instead of
			// 422 so the client sees a size-class error, not a
			// hash-class error.
			writeJSONError(w, http.StatusRequestEntityTooLarge,
				"upload exceeds maximum LFS object size")
		case errors.Is(err, blobstore.ErrOIDMismatch):
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, blobstore.ErrSizeMismatch):
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			writeJSONError(w, http.StatusGatewayTimeout, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// Post-write check: if the body would have exceeded the
	// cap but the blobstore accepted it anyway (e.g. the OID
	// happened to match), fail closed.
	if limited.Tripped {
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			"upload exceeds maximum LFS object size")
		return
	}
	if err := h.Store.UpsertBlob(r.Context(), stat.OID, stat.Size, stat.StorageURL); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "upsert blob: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// lfsMaxBytes returns the operator-configured cap or the
// package default (200 GiB).  Negative configured values are
// treated as "unset" so a typo can never silently disable the
// cap entirely.
func (h *RegistryHandler) lfsMaxBytes() int64 {
	if h.LFSMaxBytes > 0 {
		return h.LFSMaxBytes
	}
	return defaultLFSMaxBytes
}

// lfsLimitReader is io.LimitReader with an explicit "tripped"
// flag.  io.LimitReader silently returns io.EOF when the cap
// is hit, which the blobstore would interpret as a normal end
// of stream.  We need to distinguish "client sent exactly N
// bytes" from "client sent N+ bytes, we truncated".  The
// Tripped boolean is set whenever the underlying reader had
// more bytes to give us than we accepted.
type lfsLimitReader struct {
	R       io.Reader
	N       int64 // bytes remaining we are willing to read
	Tripped bool
}

func (l *lfsLimitReader) Read(p []byte) (int, error) {
	if l.N <= 0 {
		l.Tripped = true
		return 0, io.EOF
	}
	if int64(len(p)) > l.N {
		p = p[:l.N]
	}
	n, err := l.R.Read(p)
	l.N -= int64(n)
	if l.N <= 0 {
		// We hit the cap exactly with this read.  Peek to see
		// if the client had more queued; we can't actually
		// peek on io.Reader without a buffer, so mark tripped
		// only if the underlying read returned without EOF
		// (more bytes are presumably waiting).
		if err == nil {
			l.Tripped = true
		}
	}
	return n, err
}

// ---- helpers ----

func repoTypePlural(t string) (string, bool) {
	switch strings.ToLower(t) {
	case "model", "models":
		return "models", true
	case "dataset", "datasets":
		return "datasets", true
	case "space", "spaces":
		return "spaces", true
	}
	return "", false
}

// splitRepoName parses an HF repo identifier into (namespace, name).
// HF accepts both `name = "org/repo"` and `name = "repo", organization
// = "org"`; we tolerate both forms.
func splitRepoName(name, org string) (string, string) {
	if i := strings.Index(name, "/"); i > 0 {
		return name[:i], name[i+1:]
	}
	if org != "" {
		return org, name
	}
	return "", ""
}

func (h *RegistryHandler) publicURL() string {
	if h.PublicURL != "" {
		return strings.TrimRight(h.PublicURL, "/")
	}
	return ""
}

// audit writes one audit_log row. Failures are logged-and-swallowed
// so commit success isn't lost over an audit-table outage.
func (h *RegistryHandler) audit(r *http.Request, action, resource string, meta map[string]any) {
	if h.AuditPool == nil {
		return
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		metaJSON = []byte(`{}`)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*1e9) // 5s
	defer cancel()
	_, _ = h.AuditPool.Exec(ctx, `
INSERT INTO audit_log (tenant_id, actor_type, action, resource, outcome, metadata, ip, user_agent)
VALUES ($1, 'system', $2, $3, 'success', $4::jsonb, $5::inet, $6)`,
		h.TenantID, action, resource, string(metaJSON), clientIP(r), r.Header.Get("User-Agent"))
}

func clientIP(r *http.Request) any {
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		addr = addr[:i]
	}
	if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
		addr = addr[1 : len(addr)-1]
	}
	if addr == "" {
		return nil
	}
	return addr
}
