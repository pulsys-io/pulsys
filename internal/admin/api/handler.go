// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pulsys-io/pulsys/internal/admin/models"
	adminstore "github.com/pulsys-io/pulsys/internal/admin/store"
	"github.com/pulsys-io/pulsys/internal/auth"
	"github.com/pulsys-io/pulsys/internal/cache"
)

// PATCacheInvalidator is the seam that lets the admin revoke
// handler punch a hole in the data-plane PAT cache the instant
// a token's revoked_at is written, instead of waiting for the
// gate's PositiveTTL (default 60s) to expire naturally.
//
// The original 2026-05-21 PAT-revocation incident was caused by
// the gate admitting a revoked PAT.  Phase 1 closed that at the
// gate by making the lookup go straight to Postgres on a cache
// miss.  Phase 5 closes the residual 60s window where the gate
// continues to admit a revoked PAT on a CACHE HIT by wiring
// this interface from the admin revoke path back to the gate.
//
// Implementations: *auth.PATGate (production), inline stub in
// tests.  Interface lives here (not in internal/auth) so this
// package does not depend on internal/auth at all -- the
// dependency arrow only points outward when admin is wired in
// cmd/pulsys/main.go.
type PATCacheInvalidator interface {
	InvalidateByHash(hash []byte)
}

// Handler serves /admin/api/v1/* JSON endpoints.
type Handler struct {
	Store    *adminstore.AdminStore
	CacheDir string
	Cache    *cache.Store
	Tenant   string // default tenant slug when resolving by name (unused if actor has tenant)
	// AuditInsert, when set, replaces Store.InsertAudit (used in tests).
	AuditInsert func(ctx context.Context, tenantID, actorType string, actorID *string, action, resource, outcome string, metadata json.RawMessage, clientIP, userAgent string) error

	// PATCache, when set, is invalidated whenever the admin
	// revoke handler successfully marks a token as revoked.
	// Nil-safe: the revoke handler skips the call when this is
	// not wired (test paths, dev mode without a data plane).
	PATCache PATCacheInvalidator
}

func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/v1/tenant", requireAccess(auth.RoleReader, "admin:read", h.getTenant))
	mux.HandleFunc("GET /admin/api/v1/users", requireAccess(auth.RoleAdmin, "admin:write", h.listUsers))
	mux.HandleFunc("GET /admin/api/v1/tokens", requireAccess(auth.RoleMember, "admin:read", h.listTokens))
	mux.HandleFunc("POST /admin/api/v1/tokens", requireAccess(auth.RoleAdmin, "admin:write", h.createToken))
	mux.HandleFunc("DELETE /admin/api/v1/tokens/{id}", requireAccess(auth.RoleAdmin, "admin:write", h.revokeToken))
	mux.HandleFunc("GET /admin/api/v1/settings", requireAccess(auth.RoleReader, "admin:read", h.listSettings))
	mux.HandleFunc("PUT /admin/api/v1/settings/{scope}/{key}", requireAccess(auth.RoleAdmin, "admin:write", h.putSetting))
	mux.HandleFunc("GET /admin/api/v1/audit", requireAccess(auth.RoleReader, "admin:read", h.listAudit))
	mux.HandleFunc("GET /admin/api/v1/imports", requireAccess(auth.RoleReader, "admin:read", h.listImports))
	mux.HandleFunc("POST /admin/api/v1/imports", requireAccess(auth.RoleAdmin, "admin:write", h.createImport))
	mux.HandleFunc("GET /admin/api/v1/imports/{id}", requireAccess(auth.RoleReader, "admin:read", h.getImport))
	mux.HandleFunc("POST /admin/api/v1/imports/{id}/cancel", requireAccess(auth.RoleAdmin, "admin:write", h.cancelImport))
	mux.HandleFunc("POST /admin/api/v1/imports/{id}/force-cancel", requireAccess(auth.RoleAdmin, "admin:write", h.forceCancelImport))
	mux.HandleFunc("DELETE /admin/api/v1/imports/{id}", requireAccess(auth.RoleAdmin, "admin:write", h.deleteImport))
	mux.HandleFunc("GET /admin/api/v1/models", requireAccess(auth.RoleReader, "admin:read", h.listModels))
	mux.HandleFunc("GET /admin/api/v1/models/grouped", requireAccess(auth.RoleReader, "admin:read", h.listModelsGrouped))
	mux.HandleFunc("GET /admin/api/v1/cache/stats", requireAccess(auth.RoleReader, "admin:read", h.cacheStats))
	mux.HandleFunc("DELETE /admin/api/v1/models/cache", requireAccess(auth.RoleAdmin, "admin:write", h.purgeCacheByPrefix))
}

func (h *Handler) getTenant(w http.ResponseWriter, r *http.Request) {
	actor := auth.ActorFromContext(r.Context())
	t, err := h.Store.GetTenant(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	actor := auth.ActorFromContext(r.Context())
	users, err := h.Store.ListUsers(r.Context(), actor.TenantID, queryLimit(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": users})
}

func (h *Handler) listTokens(w http.ResponseWriter, r *http.Request) {
	actor := auth.ActorFromContext(r.Context())
	tokens, err := h.Store.ListTokens(r.Context(), actor.TenantID, queryLimit(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": tokens})
}

type createTokenRequest struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresIn *int64   `json:"expires_in_seconds,omitempty"`
}

// allowedTokenScopes is the canonical set of scopes a PAT may
// be minted with via the admin API.  An admin can mint a PAT
// with any subset; the data-plane gate (PATGate) and the admin
// authz layer enforce the actual privilege at request time, so
// minting an unknown scope grants no real privilege today.  We
// still reject unknown scopes here for two reasons:
//
//  1. Scope sprawl is a long-tail audit nightmare.  Once a
//     "totally:invented" scope lands in the DB it shows up in
//     every admin UI dropdown, every customer screenshot, every
//     compliance export -- with no enforcement story.
//  2. A future fine-grained policy ("models:read-private",
//     "admin:billing:write") relies on the assumption that any
//     scope string we read out of the tokens table was once on
//     the allowlist.  Validating at write time means a typo
//     surfaces immediately as a 400, not silently at next-quarter
//     RBAC migration time.
var allowedTokenScopes = map[string]struct{}{
	"models:read":  {},
	"models:write": {},
	"admin:read":   {},
	"admin:write":  {},
	"admin:*":      {},
}

// maxTokenTTL caps the lifetime an admin can ask for on a
// brand-new PAT.  366 days lets standard "rotate yearly" CI
// pipelines work without splitting the rotation across a year
// boundary, while still rejecting absurd inputs like math.MaxInt64
// that would silently overflow time.Duration arithmetic.
//
// Tokens with no expiry are still supported (omit
// expires_in_seconds entirely); this cap applies only when the
// caller explicitly opts in to a finite TTL.
const maxTokenTTL = 366 * 24 * time.Hour

func (h *Handler) createToken(w http.ResponseWriter, r *http.Request) {
	actor := auth.ActorFromContext(r.Context())
	var req createTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if len(req.Scopes) == 0 {
		req.Scopes = []string{"models:read"}
	}
	// Validate every requested scope against the allowlist before
	// burning RNG output on a token we'd just refuse to persist.
	// We return the offending scope so the admin gets a single,
	// actionable error rather than "invalid scopes" with no hint.
	for _, sc := range req.Scopes {
		if _, ok := allowedTokenScopes[sc]; !ok {
			writeError(w, http.StatusBadRequest, "scope not allowed: "+sc)
			return
		}
	}
	// TTL validation.  The legacy code accepted any int64 here
	// and silently fell through to "no expiry" on zero/negative.
	// Both branches are footguns: zero usually means "I forgot
	// to set it" rather than "I want immortal"; negative is
	// always a bug.  Reject both with 400.
	var expires *time.Time
	if req.ExpiresIn != nil {
		secs := *req.ExpiresIn
		switch {
		case secs == 0:
			writeError(w, http.StatusBadRequest, "expires_in_seconds: omit the field for no expiry; 0 is ambiguous")
			return
		case secs < 0:
			writeError(w, http.StatusBadRequest, "expires_in_seconds: must be positive")
			return
		case time.Duration(secs)*time.Second > maxTokenTTL || time.Duration(secs) > maxTokenTTL/time.Second:
			writeError(w, http.StatusBadRequest, "expires_in_seconds: exceeds 366 days")
			return
		}
		t := time.Now().UTC().Add(time.Duration(secs) * time.Second)
		expires = &t
	}
	display, prefix, hash, err := auth.GeneratePAT()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	owner := actor.UserID
	if actor.Type == auth.ActorToken {
		owner = ""
	}
	res, err := h.Store.CreateToken(r.Context(), actor.TenantID, owner, req.Name, prefix, hash, req.Scopes, expires)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create token")
		return
	}
	res.Secret = display
	writeJSON(w, http.StatusCreated, res)
}

// revokeToken marks a token revoked AND evicts it from the
// data-plane PAT cache so it can never be admitted again.
//
// Idempotency: a retry of a previously-successful revoke
// returns 204 (not 404) and does NOT write a second audit row.
// This is the contract enterprise CI pipelines need so they
// can safely retry an interrupted DELETE without flagging a
// false "outcome=failure" on the audit trail.
//
// Cache eviction: after the DB UPDATE succeeds we call
// h.PATCache.InvalidateByHash(hash).  The hash is the same
// sha256 that PATGate keys its cache by, so the next data-
// plane request that presents this PAT misses the cache,
// triggers a fresh Postgres lookup, sees the revoked_at
// timestamp, and gets a 401.  Without this call the gate
// continues to admit the token for up to PositiveTTL (60s)
// after revoke -- a residual 60s of the original incident.
func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	actor := auth.ActorFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing token id")
		return
	}
	hash, alreadyRevoked, err := h.Store.RevokeToken(r.Context(), actor.TenantID, id)
	if errors.Is(err, adminstore.ErrTokenNotFound) {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	// Always evict the cache, even on the already-revoked
	// branch: a previous revoke on a different proxy node may
	// have repopulated our local cache with a still-valid
	// lookup between then and now.
	if h.PATCache != nil {
		h.PATCache.InvalidateByHash(hash)
	}
	// Signal idempotency to the audit middleware so the second
	// revoke does not produce a "outcome=failure" audit row.
	if alreadyRevoked {
		w.Header().Set("X-Pulsys-Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listSettings(w http.ResponseWriter, r *http.Request) {
	actor := auth.ActorFromContext(r.Context())
	scope := r.URL.Query().Get("scope")
	items, err := h.Store.ListSettings(r.Context(), actor.TenantID, scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type putSettingRequest struct {
	Value   json.RawMessage `json:"value"`
	Version int64           `json:"version,omitempty"`
}

// putSetting writes a setting under strict optimistic
// concurrency.  See adminstore.UpsertSetting for the contract:
// version=0 inserts (refusing on conflict), version>=1 updates
// (refusing on stale version).  Negative versions are rejected
// at the handler so the error is "version: must be >= 0" rather
// than the lower-level wrap-around.
func (h *Handler) putSetting(w http.ResponseWriter, r *http.Request) {
	actor := auth.ActorFromContext(r.Context())
	scope := r.PathValue("scope")
	key := r.PathValue("key")
	if scope == "" || key == "" {
		writeError(w, http.StatusBadRequest, "scope and key required")
		return
	}
	var req putSettingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Version < 0 {
		writeError(w, http.StatusBadRequest, "version: must be >= 0")
		return
	}
	st, err := h.Store.UpsertSetting(r.Context(), actor.TenantID, scope, key, req.Value, req.Version, actor.UserID)
	if errors.Is(err, adminstore.ErrSettingConflict) {
		writeError(w, http.StatusConflict, "setting version conflict")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings write failed")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *Handler) listAudit(w http.ResponseWriter, r *http.Request) {
	actor := auth.ActorFromContext(r.Context())
	items, err := h.Store.ListAudit(r.Context(), actor.TenantID, queryLimit(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list audit")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type createImportRequest struct {
	RepoID   string `json:"repo_id"`
	Revision string `json:"revision,omitempty"`
}

func (h *Handler) createImport(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil || !h.Store.HasRiver() {
		writeError(w, http.StatusServiceUnavailable, "import service unavailable")
		return
	}
	actor := auth.ActorFromContext(r.Context())
	var req createImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.RepoID = strings.TrimSpace(req.RepoID)
	req.Revision = strings.TrimSpace(req.Revision)
	if req.RepoID == "" {
		writeError(w, http.StatusBadRequest, "repo_id required")
		return
	}
	if !validHFRepoID(req.RepoID) {
		writeError(w, http.StatusBadRequest, "invalid repo_id")
		return
	}
	if req.Revision == "" {
		req.Revision = "main"
	}
	if !validHFRevision(req.Revision) {
		writeError(w, http.StatusBadRequest, "invalid revision")
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"repo_id":   req.RepoID,
		"revision":  req.Revision,
		"repo_type": "models",
	})
	createdBy := actor.UserID
	if actor.Type == auth.ActorToken {
		createdBy = ""
	}
	job, err := h.Store.CreateImportJob(r.Context(), actor.TenantID, createdBy, adminstore.ImportJobTypeHFCacheImport, payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create import")
		return
	}
	h.insertAudit(r, actor, "import.create", "imports/"+job.ID, "success", map[string]any{
		"job_id":   job.ID,
		"repo_id":  req.RepoID,
		"revision": req.Revision,
	})
	writeJSON(w, http.StatusCreated, job)
}

func (h *Handler) listImports(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil || !h.Store.HasRiver() {
		writeError(w, http.StatusServiceUnavailable, "import service unavailable")
		return
	}
	actor := auth.ActorFromContext(r.Context())
	items, err := h.Store.ListImportJobs(r.Context(), actor.TenantID, queryLimit(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list imports")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) getImport(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil || !h.Store.HasRiver() {
		writeError(w, http.StatusServiceUnavailable, "import service unavailable")
		return
	}
	actor := auth.ActorFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing import id")
		return
	}
	job, err := h.Store.GetImportJob(r.Context(), actor.TenantID, id)
	if errors.Is(err, adminstore.ErrImportJobNotFound) {
		writeError(w, http.StatusNotFound, "import not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get import")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// cancelImport asks River to cancel a queued or running job.  We
// always return 204 on success, including the no-op case where the
// job has already reached a terminal state -- River's JobCancel is
// idempotent and the admin contract treats "already done" as a
// successful retry rather than a 404, matching revokeToken.
func (h *Handler) cancelImport(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil || !h.Store.HasRiver() {
		writeError(w, http.StatusServiceUnavailable, "import service unavailable")
		return
	}
	actor := auth.ActorFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing import id")
		return
	}
	err := h.Store.CancelImportJob(r.Context(), actor.TenantID, id)
	if errors.Is(err, adminstore.ErrImportJobNotFound) {
		writeError(w, http.StatusNotFound, "import not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cancel failed")
		return
	}
	h.insertAudit(r, actor, "import.cancel", "imports/"+id, "success", map[string]any{
		"job_id": id,
	})
	w.WriteHeader(http.StatusNoContent)
}

// forceCancelImport is the admin escape hatch for stuck import rows.
//
// Regular Cancel signals the worker via River; for orphaned rows
// (process restart while running, wedged worker) the row never
// transitions and Delete is hidden, leaving the operator no UI path
// to clean up. Force-cancel directly UPDATEs the river_job row to
// state='cancelled', bypassing River's "no delete running" safety.
//
// Distinct audit action `import.force_cancel` so the trail makes it
// obvious this was the destructive escape hatch and not the normal
// cancel flow.
func (h *Handler) forceCancelImport(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil || !h.Store.HasRiver() {
		writeError(w, http.StatusServiceUnavailable, "import service unavailable")
		return
	}
	actor := auth.ActorFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing import id")
		return
	}
	err := h.Store.ForceCancelImportJob(r.Context(), actor.TenantID, id)
	if errors.Is(err, adminstore.ErrImportJobNotFound) {
		writeError(w, http.StatusNotFound, "import not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "force-cancel failed")
		return
	}
	h.insertAudit(r, actor, "import.force_cancel", "imports/"+id, "success", map[string]any{
		"job_id": id,
	})
	w.WriteHeader(http.StatusNoContent)
}

// deleteImport removes a terminal import job row.  Returns 409 if
// the job is still running (the admin UI should hide Delete in that
// case; a 409 means the worker started between the last poll and
// the click).  Tenant scoping is enforced by the store via
// GetImportJob; cross-tenant ids yield 404.
func (h *Handler) deleteImport(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil || !h.Store.HasRiver() {
		writeError(w, http.StatusServiceUnavailable, "import service unavailable")
		return
	}
	actor := auth.ActorFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing import id")
		return
	}
	err := h.Store.DeleteImportJob(r.Context(), actor.TenantID, id)
	if errors.Is(err, adminstore.ErrImportJobNotFound) {
		writeError(w, http.StatusNotFound, "import not found")
		return
	}
	if errors.Is(err, adminstore.ErrImportJobRunning) {
		writeError(w, http.StatusConflict, "import is running; cancel before delete")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	h.insertAudit(r, actor, "import.delete", "imports/"+id, "success", map[string]any{
		"job_id": id,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	items, err := models.ListFromCache(h.CacheDir, queryLimit(r, 500))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to scan cache")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) listModelsGrouped(w http.ResponseWriter, r *http.Request) {
	includeFiles := r.URL.Query().Get("include_files") == "true"
	listing, err := models.ListGroupedFromCache(h.CacheDir, queryLimit(r, 500), includeFiles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to scan cache")
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

func (h *Handler) cacheStats(w http.ResponseWriter, r *http.Request) {
	if h.Cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache store unavailable")
		return
	}
	stats := h.Cache.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"used_bytes":      stats.UsedBytes,
		"quota_bytes":     stats.QuotaBytes,
		"free_disk_bytes": stats.FreeDiskBytes,
		"entry_count":     stats.EntryCount,
	})
}

type purgeCacheRequest struct {
	Org  string `json:"org"`
	Name string `json:"name"`
}

func (h *Handler) purgeCacheByPrefix(w http.ResponseWriter, r *http.Request) {
	if h.Cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache store unavailable")
		return
	}
	var req purgeCacheRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Org == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, "org and name required")
		return
	}
	// Validate org/name against the Hugging Face naming grammar
	// BEFORE we use them for any comparison or for the audit log.
	// The matcher below is exact equality so an invalid value
	// would simply match nothing, but the audit log embeds
	// "org/name" as a free-form target string and we don't want
	// path-traversal or whitespace-control characters to land in
	// the audit record where downstream log consumers might mis-
	// interpret them.  RFC-style allowlist: ASCII alphanumeric,
	// dash, underscore, dot; length 1..96.
	if !validHFNameSegment(req.Org) {
		writeError(w, http.StatusBadRequest, "invalid org segment")
		return
	}
	if !validHFNameSegment(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid name segment")
		return
	}
	// PurgeOrTrim splits two responsibilities:
	//
	//   - Remove: drop the cache body when (org, name) was the last
	//     remaining owner of this entry.  Same disk effect as the
	//     legacy PurgeKeys branch.
	//   - Trim:   rewrite meta.json without (org, name) when other
	//     models still own the body.  Necessary because Xet/LFS
	//     chunks are content-addressed and can legitimately be
	//     shared across HF repos; deleting the body when one of two
	//     owners purges would silently degrade the second model.
	res, err := h.Cache.PurgeOrTrim(func(_ string, m *cache.Meta) (cache.PurgeDecision, *cache.Meta) {
		if !models.OwnedBy(m, req.Org, req.Name) {
			return cache.DecisionKeep, nil
		}
		trimmed := models.RemoveOwner(m, req.Org, req.Name)
		if !models.HasAnyOwner(trimmed) {
			return cache.DecisionRemove, nil
		}
		return cache.DecisionTrim, trimmed
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "purge failed")
		return
	}
	actor := auth.ActorFromContext(r.Context())
	meta, _ := json.Marshal(map[string]any{
		"purged":      res.Purged,
		"trimmed":     res.Trimmed,
		"bytes_freed": res.BytesFreed,
		"org":         req.Org,
		"name":        req.Name,
	})
	actorType := string(actor.Type)
	if actorType == "" {
		actorType = "user"
	}
	var actorID *string
	if actor.UserID != "" {
		actorID = &actor.UserID
	} else if actor.TokenID != "" {
		actorID = &actor.TokenID
	}
	insertAudit := h.AuditInsert
	if insertAudit == nil && h.Store != nil {
		insertAudit = h.Store.InsertAudit
	}
	if insertAudit != nil {
		_ = insertAudit(r.Context(), actor.TenantID, actorType, actorID,
			"cache.purge", req.Org+"/"+req.Name, "success", meta, clientIP(r), r.UserAgent())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"purged":      res.Purged,
		"trimmed":     res.Trimmed,
		"bytes_freed": res.BytesFreed,
	})
}

// validHFNameSegment reports whether s is a syntactically valid
// HuggingFace org / repository name segment.  The HF naming rules
// allow ASCII letters, digits, dash, underscore, dot; we require
// at least 1 and at most 96 bytes and forbid leading dots and ".."
// so a future caller cannot accidentally promote user input to a
// filesystem path.
func validHFNameSegment(s string) bool {
	if s == "" || len(s) > 96 {
		return false
	}
	if s[0] == '.' {
		return false
	}
	if s == ".." || s == "." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}

func validHFRepoID(s string) bool {
	parts := strings.Split(s, "/")
	if len(parts) < 1 || len(parts) > 2 {
		return false
	}
	for _, part := range parts {
		if !validHFNameSegment(part) {
			return false
		}
	}
	return true
}

func validHFRevision(s string) bool {
	if s == "" || len(s) > 128 || s[0] == '.' {
		return false
	}
	if s == "." || s == ".." || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == '/':
		default:
			return false
		}
	}
	return true
}

func (h *Handler) insertAudit(r *http.Request, actor auth.Actor, action, resource, outcome string, metadata map[string]any) {
	meta, _ := json.Marshal(metadata)
	actorType := string(actor.Type)
	if actorType == "" {
		actorType = "user"
	}
	var actorID *string
	if actor.UserID != "" {
		actorID = &actor.UserID
	} else if actor.TokenID != "" {
		actorID = &actor.TokenID
	}
	insertAudit := h.AuditInsert
	if insertAudit == nil && h.Store != nil {
		insertAudit = h.Store.InsertAudit
	}
	if insertAudit != nil {
		_ = insertAudit(r.Context(), actor.TenantID, actorType, actorID,
			action, resource, outcome, meta, clientIP(r), r.UserAgent())
	}
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func queryLimit(r *http.Request, def int) int {
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// X-Content-Type-Options: nosniff is the belt-and-suspenders
	// defense against MIME-sniffing browsers re-interpreting our
	// JSON body as HTML (the classic stored-XSS regression).
	// Setting it on every JSON response -- success AND error --
	// is cheap and is required by WSTG-INPV-01/02 and CONF-06.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
