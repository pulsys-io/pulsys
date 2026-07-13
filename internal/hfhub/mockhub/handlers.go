// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package mockhub

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// router builds an http.Handler that dispatches by path prefix.
// Go 1.22's pattern mux can't disambiguate /api/... from
// /{org}/{name}/resolve/... without negative lookahead, so we split
// into two muxes and route at the top level.
func (s *Server) router() http.Handler {
	apiMux := http.NewServeMux()

	apiMux.HandleFunc("GET /api/models/{org}/{name}", s.modelInfo)
	apiMux.HandleFunc("GET /api/datasets/{org}/{name}", s.modelInfoTyped("datasets"))
	apiMux.HandleFunc("GET /api/spaces/{org}/{name}", s.modelInfoTyped("spaces"))

	apiMux.HandleFunc("GET /api/models/{org}/{name}/tree/{rev}", s.tree)
	apiMux.HandleFunc("GET /api/datasets/{org}/{name}/tree/{rev}", s.treeTyped("datasets"))

	apiMux.HandleFunc("POST /api/models/{org}/{name}/paths-info/{rev}", s.pathsInfo)
	apiMux.HandleFunc("POST /api/datasets/{org}/{name}/paths-info/{rev}", s.pathsInfoTyped("datasets"))

	apiMux.HandleFunc("POST /api/models/{org}/{name}/preupload/{rev}", s.preupload)
	apiMux.HandleFunc("POST /api/models/{org}/{name}/commit/{rev}", s.commit)
	apiMux.HandleFunc("POST /api/datasets/{org}/{name}/preupload/{rev}", s.preuploadTyped("datasets"))
	apiMux.HandleFunc("POST /api/datasets/{org}/{name}/commit/{rev}", s.commitTyped("datasets"))

	// LFS PUT lives at a fixed root path so a sub-mux is safe.
	lfsPutMux := http.NewServeMux()
	lfsPutMux.HandleFunc("PUT /lfs-storage/{oid}", s.lfsPut)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			_, _ = io.WriteString(w, "ok")
		case strings.HasPrefix(r.URL.Path, "/api/"):
			apiMux.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/lfs-storage/"):
			lfsPutMux.ServeHTTP(w, r)
		case strings.Contains(r.URL.Path, ".git/info/lfs/"):
			s.lfsRoute(w, r)
		default:
			s.resolveRoute(w, r)
		}
	})
}

// resolveRoute parses /[<repo_type>/]<org>/<name>/resolve/<rev>/<path>
// without depending on http.ServeMux pattern disambiguation. It
// records the canonical repoType, populates PathValues, and calls
// resolveImpl. Returns 404 for malformed paths.
func (s *Server) resolveRoute(w http.ResponseWriter, r *http.Request) {
	repoType, org, name, rev, path, ok := parseResolvePath(r.URL.Path)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	r.SetPathValue("org", org)
	r.SetPathValue("name", name)
	r.SetPathValue("rev", rev)
	r.SetPathValue("path", path)
	s.resolveImpl(w, r, repoType)
}

func parseResolvePath(p string) (repoType, org, name, rev, path string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	repoType = "models"
	if len(parts) > 0 && (parts[0] == "datasets" || parts[0] == "spaces") {
		repoType = parts[0]
		parts = parts[1:]
	}
	if len(parts) < 5 || parts[2] != "resolve" {
		return "", "", "", "", "", false
	}
	org = parts[0]
	name = parts[1]
	rev = parts[3]
	path = strings.Join(parts[4:], "/")
	if path == "" {
		return "", "", "", "", "", false
	}
	return repoType, org, name, rev, path, true
}

// lfsRoute parses /<org>/<name>.git/info/lfs/{objects/batch|verify}
// without depending on mux. The ".git" segment trips ServeMux's
// `{name}` matcher; manual parsing is more robust.
func (s *Server) lfsRoute(w http.ResponseWriter, r *http.Request) {
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
	r.SetPathValue("org", parts[0])
	r.SetPathValue("name", parts[1])
	switch {
	case r.Method == http.MethodPost && tail == "objects/batch":
		s.lfsBatch(w, r)
	case r.Method == http.MethodPost && tail == "verify":
		s.lfsVerify(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "not found")
	}
}

// ----- model info / tree / paths-info -----

func (s *Server) modelInfo(w http.ResponseWriter, r *http.Request) {
	s.modelInfoTyped("models")(w, r)
}

func (s *Server) modelInfoTyped(repoType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.state.incCall("GET", "/api/"+repoType+"/{repo}")
		if _, err := s.state.authorize(r.Header.Get("Authorization")); err != nil {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		name := repoNameFromPath(r)
		files, c, ok := s.state.listRepoFiles(repoType, name, "main")
		if !ok {
			writeJSONError(w, http.StatusNotFound, "repo not found")
			return
		}
		siblings := make([]siblingEntry, 0, len(files))
		for _, path := range sortedPaths(files) {
			f := files[path]
			sib := siblingEntry{
				RFilename: f.Path,
				Size:      f.Size,
				BlobID:    f.OID,
			}
			if f.IsLFS {
				sib.LFS = &lfs{OID: f.OID, Size: f.Size, PointerSize: 134}
			}
			siblings = append(siblings, sib)
		}
		writeJSON(w, modelInfoResponse{
			ID:       name,
			ModelID:  name,
			SHA:      c.OID,
			Tags:     []string{},
			Siblings: siblings,
		})
	}
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request) {
	s.treeTyped("models")(w, r)
}

func (s *Server) treeTyped(repoType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.state.incCall("GET", "/api/"+repoType+"/{repo}/tree/{rev}")
		if _, err := s.state.authorize(r.Header.Get("Authorization")); err != nil {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		name := repoNameFromPath(r)
		rev := r.PathValue("rev")
		files, _, ok := s.state.listRepoFiles(repoType, name, rev)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "repo or revision not found")
			return
		}
		out := make([]treeEntry, 0, len(files))
		for _, path := range sortedPaths(files) {
			f := files[path]
			te := treeEntry{
				Type: "file",
				OID:  f.OID,
				Size: f.Size,
				Path: f.Path,
			}
			if f.IsLFS {
				te.LFS = &lfs{OID: f.OID, Size: f.Size, PointerSize: 134}
			}
			out = append(out, te)
		}
		writeJSON(w, out)
	}
}

func (s *Server) pathsInfo(w http.ResponseWriter, r *http.Request) {
	s.pathsInfoTyped("models")(w, r)
}

func (s *Server) pathsInfoTyped(repoType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.state.incCall("POST", "/api/"+repoType+"/{repo}/paths-info/{rev}")
		if _, err := s.state.authorize(r.Header.Get("Authorization")); err != nil {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var req pathsInfoRequest
		if err := decodeJSONOrForm(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		name := repoNameFromPath(r)
		rev := r.PathValue("rev")
		files, c, ok := s.state.listRepoFiles(repoType, name, rev)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "repo or revision not found")
			return
		}
		out := make([]pathsInfoEntry, 0, len(req.Paths))
		for _, p := range req.Paths {
			f, ok := files[p]
			if !ok {
				continue
			}
			entry := pathsInfoEntry{
				Path: f.Path,
				Size: f.Size,
				Type: "file",
				OID:  f.OID,
			}
			if f.IsLFS {
				entry.LFS = &lfs{OID: f.OID, Size: f.Size, PointerSize: 134}
			}
			entry.LastCommit = &struct {
				ID   string `json:"id"`
				Date string `json:"date"`
			}{ID: c.OID, Date: c.Time.Format(time.RFC3339)}
			out = append(out, entry)
		}
		writeJSON(w, out)
	}
}

// ----- resolve (download) -----

func (s *Server) resolveImpl(w http.ResponseWriter, r *http.Request, repoType string) {
	s.state.incCall(r.Method, "/{repo}/resolve/{rev}/{path}")
	if _, err := s.state.authorize(r.Header.Get("Authorization")); err != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	name := repoNameFromPath(r)
	rev := r.PathValue("rev")
	path := r.PathValue("path")
	f, body, ok := s.state.fileBytes(repoType, name, rev, path)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "entry not found")
		return
	}
	etag := `"` + f.OID + `"`
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Repo-Commit", f.CommitOID)
	w.Header().Set("X-Linked-Etag", etag)
	w.Header().Set("X-Linked-Size", strconv.FormatInt(f.Size, 10))
	w.Header().Set("Accept-Ranges", "bytes")

	// Range support.
	rangeHdr := r.Header.Get("Range")
	if rangeHdr != "" {
		start, end, ok := parseSimpleRange(rangeHdr, int64(len(body)))
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(body)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body[start : end+1])
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// ----- upload: preupload -----

func (s *Server) preupload(w http.ResponseWriter, r *http.Request) {
	s.preuploadTyped("models")(w, r)
}

func (s *Server) preuploadTyped(repoType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.state.incCall("POST", "/api/"+repoType+"/{repo}/preupload/{rev}")
		role, err := s.state.authorize(r.Header.Get("Authorization"))
		if err != nil || (role != RoleAnonymous && role != RoleWrite) {
			writeJSONError(w, http.StatusUnauthorized, "write authorization required")
			return
		}
		var req preuploadRequest
		if err := decodeJSONOrForm(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		name := repoNameFromPath(r)
		resp := preuploadResponse{Files: make([]preuploadFileResponse, 0, len(req.Files))}
		for _, f := range req.Files {
			mode := "regular"
			// HF treats anything > 10 MiB as LFS by default. Some
			// extensions (.bin, .safetensors) are also always-LFS;
			// keep it simple and follow the size threshold only.
			if f.Size > 10<<20 {
				mode = "lfs"
			}
			fr := preuploadFileResponse{Path: f.Path, UploadMode: mode}
			if s.state.hasFile(repoType, name, f.Path) {
				// Dedup hit: we still return the path so the client
				// knows it can skip the upload.
				fr.ShouldIgnore = true
			}
			resp.Files = append(resp.Files, fr)
		}
		writeJSON(w, resp)
	}
}

// ----- upload: commit -----

func (s *Server) commit(w http.ResponseWriter, r *http.Request) {
	s.commitTyped("models")(w, r)
}

func (s *Server) commitTyped(repoType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.state.incCall("POST", "/api/"+repoType+"/{repo}/commit/{rev}")
		role, err := s.state.authorize(r.Header.Get("Authorization"))
		if err != nil || (role != RoleAnonymous && role != RoleWrite) {
			writeJSONError(w, http.StatusUnauthorized, "write authorization required")
			return
		}
		name := repoNameFromPath(r)
		rev := r.PathValue("rev")
		summary, desc, inline, lfsPtr, deletes, err := decodeCommitNDJSON(r.Body)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		newSHA, err := s.state.commitFromRequest(repoType, name, rev, summary, desc, inline, lfsPtr, deletes)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, commitResponse{
			Success:   true,
			CommitOID: newSHA,
			CommitURL: fmt.Sprintf("%s/%s/%s/commit/%s", s.url, repoType, name, newSHA),
		})
	}
}

// decodeCommitNDJSON parses the commit endpoint's NDJSON body into
// (summary, description, inline files, LFS pointers, deleted paths).
func decodeCommitNDJSON(body io.Reader) (summary, desc string, inline map[string][]byte,
	lfsPtr map[string]lfsPointerSpec, deletes []string, err error) {
	inline = make(map[string][]byte)
	lfsPtr = make(map[string]lfsPointerSpec)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if err = json.Unmarshal(line, &msg); err != nil {
			return
		}
		switch msg.Key {
		case "header":
			var v struct {
				Summary     string `json:"summary"`
				Description string `json:"description"`
			}
			if err = json.Unmarshal(msg.Value, &v); err != nil {
				return
			}
			summary = v.Summary
			desc = v.Description
		case "file":
			var v struct {
				Path     string `json:"path"`
				Encoding string `json:"encoding"`
				Content  string `json:"content"`
			}
			if err = json.Unmarshal(msg.Value, &v); err != nil {
				return
			}
			var body []byte
			switch v.Encoding {
			case "", "utf-8":
				body = []byte(v.Content)
			case "base64":
				body, err = base64.StdEncoding.DecodeString(v.Content)
				if err != nil {
					return
				}
			default:
				err = fmt.Errorf("unknown commit file encoding %q", v.Encoding)
				return
			}
			inline[v.Path] = body
		case "lfsFile":
			var v struct {
				Path string `json:"path"`
				OID  string `json:"oid"`
				Size int64  `json:"size"`
				Algo string `json:"algo"`
			}
			if err = json.Unmarshal(msg.Value, &v); err != nil {
				return
			}
			lfsPtr[v.Path] = lfsPointerSpec{oid: v.OID, size: v.Size}
		case "deletedFile":
			var v struct {
				Path string `json:"path"`
			}
			if err = json.Unmarshal(msg.Value, &v); err != nil {
				return
			}
			deletes = append(deletes, v.Path)
		case "deletedFolder":
			var v struct {
				Path string `json:"path"`
			}
			if err = json.Unmarshal(msg.Value, &v); err != nil {
				return
			}
			deletes = append(deletes, v.Path) // tests only assert presence
		default:
			// Unknown keys (copyFile, etc.) are silently ignored to
			// keep the mock forward-compatible with new HF features.
		}
	}
	if err = scanner.Err(); err != nil {
		return
	}
	return
}

// ----- LFS batch / PUT / verify -----

func (s *Server) lfsBatch(w http.ResponseWriter, r *http.Request) {
	s.state.incCall("POST", "/{repo}.git/info/lfs/objects/batch")
	role, err := s.state.authorize(r.Header.Get("Authorization"))
	if err != nil || (role != RoleAnonymous && role != RoleWrite && role != RoleRead) {
		writeJSONError(w, http.StatusUnauthorized, "authorization required")
		return
	}
	var req lfsBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := lfsBatchResponse{Transfer: "basic", HashAlgo: "sha256"}
	for _, obj := range req.Objects {
		entry := lfsBatchObjResponse{OID: obj.OID, Size: obj.Size}
		switch req.Operation {
		case "upload":
			if role != RoleWrite && role != RoleAnonymous {
				entry.Error = &lfsObjectError{Code: 403, Message: "forbidden"}
				resp.Objects = append(resp.Objects, entry)
				continue
			}
			entry.Actions = map[string]lfsAction{
				"upload": {
					Href:   s.url + "/lfs-storage/" + obj.OID,
					Header: map[string]string{"Content-Type": "application/octet-stream"},
				},
				"verify": {
					Href: s.url + lfsVerifyPath(r),
				},
			}
		case "download":
			if !s.state.hasLFSObject(obj.OID) {
				entry.Error = &lfsObjectError{Code: 404, Message: "object not found"}
			} else {
				entry.Actions = map[string]lfsAction{
					"download": {Href: s.url + "/lfs-storage/" + obj.OID},
				}
			}
		default:
			entry.Error = &lfsObjectError{Code: 400, Message: "unsupported operation"}
		}
		resp.Objects = append(resp.Objects, entry)
	}
	writeJSON(w, resp)
}

func lfsVerifyPath(r *http.Request) string {
	org := r.PathValue("org")
	name := r.PathValue("name")
	return "/" + org + "/" + name + ".git/info/lfs/verify"
}

func (s *Server) lfsPut(w http.ResponseWriter, r *http.Request) {
	s.state.incCall("PUT", "/lfs-storage/{oid}")
	role, err := s.state.authorize(r.Header.Get("Authorization"))
	if err != nil || (role != RoleAnonymous && role != RoleWrite) {
		// HF presigned URLs don't actually require Authorization, but
		// the mock accepts anonymous puts when configured that way.
		writeJSONError(w, http.StatusUnauthorized, "write authorization required")
		return
	}
	expectOID := r.PathValue("oid")
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024*1024*1024))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	gotOID := sha256Hex(body)
	if expectOID != "" && expectOID != gotOID {
		writeJSONError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("oid mismatch: expected %s got %s", expectOID, gotOID))
		return
	}
	_ = s.state.putLFSObject(body)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) lfsVerify(w http.ResponseWriter, r *http.Request) {
	s.state.incCall("POST", "/{repo}.git/info/lfs/verify")
	role, err := s.state.authorize(r.Header.Get("Authorization"))
	if err != nil || (role != RoleAnonymous && role != RoleWrite) {
		writeJSONError(w, http.StatusUnauthorized, "write authorization required")
		return
	}
	var req lfsVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.state.hasLFSObject(req.OID) {
		writeJSONError(w, http.StatusNotFound, "object not uploaded")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ----- helpers -----

func repoNameFromPath(r *http.Request) string {
	return r.PathValue("org") + "/" + r.PathValue("name")
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

func decodeJSONOrForm(r *http.Request, v any) error {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "application/x-ndjson") || ct == "" {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return nil
		}
		if err := json.Unmarshal(body, v); err != nil {
			return fmt.Errorf("bad json: %w", err)
		}
		return nil
	}
	if err := r.ParseForm(); err != nil {
		return err
	}
	// Best-effort: re-marshal form values into JSON, then decode.
	// This keeps a single decode path for paths-info callers that
	// send `paths=...&paths=...` form bodies.
	form := map[string][]string{}
	for k, v := range r.Form {
		form[k] = v
	}
	tmp, _ := json.Marshal(form)
	type pf struct {
		Paths []string `json:"paths"`
	}
	var p pf
	if err := json.Unmarshal(tmp, &p); err == nil {
		if pi, ok := v.(*pathsInfoRequest); ok {
			pi.Paths = p.Paths
			return nil
		}
	}
	return errors.New("unsupported content type")
}

// parseSimpleRange parses a single "bytes=START-END" range. Returns
// (start, end, ok). Open-ended forms (`bytes=N-`, `bytes=-N`) are
// supported. Multi-range requests are rejected (HF doesn't issue them).
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
		// suffix range: last N bytes
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
