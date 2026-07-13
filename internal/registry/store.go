// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the registry data layer. All public methods accept a
// tenant id; the pool is shared across tenants and RLS enforces
// isolation when configured (see migration 0004).
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool. The pool MUST already be
// migrated to schema version >= 4.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool returns the underlying pgxpool. Intended for tests that need
// to query tables the Store does not directly expose (audit_log,
// settings). Production callers should NOT reach through this.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// CreateRepo inserts a repo row. Returns ErrAlreadyExists when the
// (tenant, type, ns, name) tuple already exists (and is not
// soft-deleted).
func (s *Store) CreateRepo(ctx context.Context, tenantID, repoType, ns, name string, private bool, createdBy string) (Repo, error) {
	if err := validateRepoTriple(repoType, ns, name); err != nil {
		return Repo{}, err
	}
	var creator any
	if createdBy != "" {
		creator = createdBy
	}
	row := s.pool.QueryRow(ctx, `
INSERT INTO repos (tenant_id, repo_type, namespace, name, private, created_by)
VALUES ($1,$2,$3,$4,$5,$6)
RETURNING id, created_at, updated_at`,
		tenantID, repoType, ns, name, private, creator)
	var r Repo
	r.TenantID = tenantID
	r.Type = repoType
	r.Namespace = ns
	r.Name = name
	r.Private = private
	if err := row.Scan(&r.ID, &r.CreatedAt, &r.UpdatedAt); err != nil {
		if isUniqueViolation(err) {
			return Repo{}, ErrAlreadyExists
		}
		return Repo{}, err
	}
	return r, nil
}

// GetRepo loads a repo by tenant + (type, ns, name).
func (s *Store) GetRepo(ctx context.Context, tenantID, repoType, ns, name string) (Repo, error) {
	var r Repo
	err := s.pool.QueryRow(ctx, `
SELECT id, tenant_id, repo_type, namespace, name, private, created_at, updated_at
FROM repos
WHERE tenant_id=$1 AND repo_type=$2 AND namespace=$3 AND name=$4 AND deleted_at IS NULL`,
		tenantID, repoType, ns, name).
		Scan(&r.ID, &r.TenantID, &r.Type, &r.Namespace, &r.Name, &r.Private, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Repo{}, ErrNotFound
	}
	return r, err
}

// ResolveBranch returns the commit sha for a ref. The ref may be a
// branch name ("main", "pr/42"), a 40-char commit sha, or a >=7-char
// short sha.
func (s *Store) ResolveBranch(ctx context.Context, repoID, ref string) (string, error) {
	if ref == "" {
		ref = "main"
	}
	// Branch lookup first (cheap, exact).
	var sha string
	err := s.pool.QueryRow(ctx,
		`SELECT commit_sha FROM branches WHERE repo_id=$1 AND name=$2`,
		repoID, ref).Scan(&sha)
	if err == nil {
		return sha, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	// Exact commit sha?
	if len(ref) == 40 && isHex(ref) {
		err := s.pool.QueryRow(ctx,
			`SELECT sha FROM commits WHERE repo_id=$1 AND sha=$2`,
			repoID, ref).Scan(&sha)
		if err == nil {
			return sha, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}

	// Short-sha prefix.
	if len(ref) >= 7 && isHex(ref) {
		err := s.pool.QueryRow(ctx,
			`SELECT sha FROM commits WHERE repo_id=$1 AND sha LIKE $2 LIMIT 2`,
			repoID, ref+"%").Scan(&sha)
		if err == nil {
			return sha, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}

	return "", ErrNotFound
}

// ListFiles returns the file revisions at a commit (joined with
// blob size), keyed by path.
func (s *Store) ListFiles(ctx context.Context, repoID, sha string) (map[string]FileRevision, *Commit, error) {
	var c Commit
	err := s.pool.QueryRow(ctx, `
SELECT id, repo_id, sha, COALESCE(parent_sha,''), summary, description, created_at
FROM commits WHERE repo_id=$1 AND sha=$2`,
		repoID, sha).Scan(&c.ID, &c.RepoID, &c.SHA, &c.ParentSHA, &c.Summary, &c.Description, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT fr.id, fr.path, fr.blob_oid, fr.is_lfs, b.size_bytes
FROM file_revisions fr
JOIN blobs b ON b.oid = fr.blob_oid
WHERE fr.commit_id = $1`, c.ID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := make(map[string]FileRevision)
	for rows.Next() {
		var fr FileRevision
		fr.CommitID = c.ID
		if err := rows.Scan(&fr.ID, &fr.Path, &fr.BlobOID, &fr.IsLFS, &fr.Size); err != nil {
			return nil, nil, err
		}
		out[fr.Path] = fr
	}
	return out, &c, rows.Err()
}

// LookupFile returns one file at (repo, ref, path). The branch / sha
// resolution mirrors ResolveBranch.
func (s *Store) LookupFile(ctx context.Context, repoID, ref, path string) (FileRevision, Commit, error) {
	sha, err := s.ResolveBranch(ctx, repoID, ref)
	if err != nil {
		return FileRevision{}, Commit{}, err
	}
	var c Commit
	if err := s.pool.QueryRow(ctx, `
SELECT id, repo_id, sha, COALESCE(parent_sha,''), summary, description, created_at
FROM commits WHERE repo_id=$1 AND sha=$2`, repoID, sha).
		Scan(&c.ID, &c.RepoID, &c.SHA, &c.ParentSHA, &c.Summary, &c.Description, &c.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return FileRevision{}, Commit{}, ErrNotFound
		}
		return FileRevision{}, Commit{}, err
	}
	var fr FileRevision
	fr.CommitID = c.ID
	err = s.pool.QueryRow(ctx, `
SELECT fr.id, fr.path, fr.blob_oid, fr.is_lfs, b.size_bytes
FROM file_revisions fr
JOIN blobs b ON b.oid = fr.blob_oid
WHERE fr.commit_id=$1 AND fr.path=$2`, c.ID, path).
		Scan(&fr.ID, &fr.Path, &fr.BlobOID, &fr.IsLFS, &fr.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return FileRevision{}, Commit{}, ErrNotFound
	}
	return fr, c, err
}

// HasBlob reports whether oid exists in the blobs table. Used by the
// LFS verify and LFS batch endpoints to short-circuit "already
// uploaded" without hitting the blobstore filesystem.
func (s *Store) HasBlob(ctx context.Context, oid string) (Blob, bool, error) {
	var b Blob
	err := s.pool.QueryRow(ctx,
		`SELECT oid, size_bytes, storage_url, refcount FROM blobs WHERE oid=$1`, oid).
		Scan(&b.OID, &b.Size, &b.StorageURL, &b.RefCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return Blob{}, false, nil
	}
	if err != nil {
		return Blob{}, false, err
	}
	return b, true, nil
}

// UpsertBlob inserts a blobs row, or no-ops if it already exists. The
// refcount is NOT touched here; CommitTx increments it for each
// file_revisions row that references the blob.
func (s *Store) UpsertBlob(ctx context.Context, oid string, size int64, storageURL string) error {
	if !isHexN(oid, 64) {
		return fmt.Errorf("%w: bad oid", ErrInvalidInput)
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO blobs (oid, size_bytes, storage_url)
VALUES ($1, $2, $3)
ON CONFLICT (oid) DO NOTHING`,
		oid, size, storageURL)
	return err
}

// CommitTx is the atomic write entry point. It:
//
//  1. Verifies the parent branch sha matches expectations (or auto-
//     creates the branch when missing - HF Hub does this for fresh
//     feature branches).
//  2. Builds the new file tree by copying parent_files - deletes +
//     inline overrides + LFS overrides.
//  3. Inserts a new commit row with a synthesized SHA.
//  4. Inserts file_revisions for the resulting tree.
//  5. Increments blobs.refcount for each unique blob_oid referenced
//     for the FIRST time by this commit (no double-counting across
//     reverts that point at the same blob again).
//  6. Advances the branch pointer.
func (s *Store) CommitTx(ctx context.Context, in CommitInput) (CommitResult, error) {
	if in.RepoID == "" {
		return CommitResult{}, fmt.Errorf("%w: repo_id required", ErrInvalidInput)
	}
	branch := in.Branch
	if branch == "" {
		branch = "main"
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CommitResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var parentSHA string
	if err := tx.QueryRow(ctx,
		`SELECT commit_sha FROM branches WHERE repo_id=$1 AND name=$2`, in.RepoID, branch).
		Scan(&parentSHA); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CommitResult{}, err
	}

	// Collect parent file_revisions.
	parentFiles := map[string]FileRevision{}
	if parentSHA != "" {
		rows, err := tx.Query(ctx, `
SELECT fr.path, fr.blob_oid, fr.is_lfs, b.size_bytes
FROM file_revisions fr
JOIN commits c ON c.id = fr.commit_id
JOIN blobs b ON b.oid = fr.blob_oid
WHERE c.repo_id=$1 AND c.sha=$2`, in.RepoID, parentSHA)
		if err != nil {
			return CommitResult{}, err
		}
		for rows.Next() {
			var fr FileRevision
			if err := rows.Scan(&fr.Path, &fr.BlobOID, &fr.IsLFS, &fr.Size); err != nil {
				rows.Close()
				return CommitResult{}, err
			}
			parentFiles[fr.Path] = fr
		}
		rows.Close()
	}

	newFiles := make(map[string]FileRevision, len(parentFiles))
	for p, f := range parentFiles {
		newFiles[p] = f
	}
	for _, d := range in.Deletes {
		if err := ValidateCommitFilePath(d); err != nil {
			return CommitResult{}, err
		}
		delete(newFiles, d)
	}
	for path, f := range in.Inline {
		if err := ValidateCommitFilePath(path); err != nil {
			return CommitResult{}, err
		}
		newFiles[path] = FileRevision{Path: path, BlobOID: f.BlobOID, Size: f.Size, IsLFS: false}
	}
	for path, f := range in.LFSPointers {
		if err := ValidateCommitFilePath(path); err != nil {
			return CommitResult{}, err
		}
		newFiles[path] = FileRevision{Path: path, BlobOID: f.BlobOID, Size: f.Size, IsLFS: true}
	}

	// Confirm every referenced blob exists AND -- for LFS
	// pointers -- that the client-declared size matches the
	// authoritative blobs.size_bytes recorded at upload time.
	// We pull size_bytes in the same query so the cross-check
	// adds no extra roundtrips.
	//
	// Inline blobs are skipped from the size check because
	// inline file sizes are computed from the in-line bytes
	// (no client-asserted size to cross-check).
	missing := make([]string, 0)
	for _, f := range newFiles {
		var dummy int
		var storedSize int64
		if err := tx.QueryRow(ctx, `SELECT 1, size_bytes FROM blobs WHERE oid=$1`, f.BlobOID).Scan(&dummy, &storedSize); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				missing = append(missing, f.BlobOID)
				continue
			}
			return CommitResult{}, err
		}
		if f.IsLFS && f.Size > 0 && f.Size != storedSize {
			return CommitResult{}, fmt.Errorf("%w: path=%q oid=%s declared=%d stored=%d",
				ErrLFSSizeMismatch, f.Path, f.BlobOID, f.Size, storedSize)
		}
	}
	if len(missing) > 0 {
		return CommitResult{}, fmt.Errorf("%w: %s", ErrBlobMissing, missing[0])
	}

	newSHA := deriveCommitSHA(in.Summary, time.Now(), parentSHA)
	var commitID string
	err = tx.QueryRow(ctx, `
INSERT INTO commits (repo_id, sha, parent_sha, author_id, summary, description)
VALUES ($1,$2, NULLIF($3,''), NULLIF($4,'')::uuid, $5, $6)
RETURNING id`,
		in.RepoID, newSHA, parentSHA, in.AuthorID, in.Summary, in.Description).Scan(&commitID)
	if err != nil {
		return CommitResult{}, fmt.Errorf("insert commit: %w", err)
	}

	if len(newFiles) > 0 {
		batch := &pgx.Batch{}
		for path, fr := range newFiles {
			batch.Queue(`INSERT INTO file_revisions (commit_id, path, blob_oid, is_lfs) VALUES ($1,$2,$3,$4)`,
				commitID, path, fr.BlobOID, fr.IsLFS)
		}
		br := tx.SendBatch(ctx, batch)
		for range newFiles {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return CommitResult{}, fmt.Errorf("insert file_revisions: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return CommitResult{}, err
		}
	}

	// Increment refcount once per blob first-referenced by this commit.
	added := map[string]struct{}{}
	for _, fr := range newFiles {
		if _, dup := added[fr.BlobOID]; dup {
			continue
		}
		if pfr, was := parentFiles[fr.Path]; was && pfr.BlobOID == fr.BlobOID {
			// Same blob carried over; refcount already reflects the
			// parent's reference, so we skip the bump.
			continue
		}
		added[fr.BlobOID] = struct{}{}
	}
	if len(added) > 0 {
		oids := make([]string, 0, len(added))
		for o := range added {
			oids = append(oids, o)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE blobs SET refcount = refcount + 1 WHERE oid = ANY($1::text[])`, oids); err != nil {
			return CommitResult{}, fmt.Errorf("bump refcount: %w", err)
		}
	}

	// Advance branch.
	if _, err := tx.Exec(ctx, `
INSERT INTO branches (repo_id, name, commit_sha)
VALUES ($1,$2,$3)
ON CONFLICT (repo_id, name) DO UPDATE
SET commit_sha = EXCLUDED.commit_sha,
    updated_at = now()`, in.RepoID, branch, newSHA); err != nil {
		return CommitResult{}, fmt.Errorf("update branch: %w", err)
	}

	if _, err := tx.Exec(ctx, `UPDATE repos SET updated_at=now() WHERE id=$1`, in.RepoID); err != nil {
		return CommitResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return CommitResult{}, err
	}
	return CommitResult{CommitID: commitID, SHA: newSHA, Branch: branch}, nil
}

// ---- mirrors ----

// CreateMirror declares that a repo path should be served from an
// upstream when not present in the local registry.
func (s *Store) CreateMirror(ctx context.Context, tenantID, repoType, ns, name, upstream, pinnedSHA, createdBy string) (Mirror, error) {
	if err := validateRepoTriple(repoType, ns, name); err != nil {
		return Mirror{}, err
	}
	if upstream == "" {
		upstream = "huggingface.co"
	}
	if pinnedSHA != "" && !isHexN(pinnedSHA, 40) {
		return Mirror{}, fmt.Errorf("%w: bad pinned_sha", ErrInvalidInput)
	}
	var creator any
	if createdBy != "" {
		creator = createdBy
	}
	row := s.pool.QueryRow(ctx, `
INSERT INTO mirrors (tenant_id, repo_type, namespace, name, upstream_host, pinned_sha, created_by)
VALUES ($1,$2,$3,$4,$5, NULLIF($6,''), $7)
RETURNING id, created_at, updated_at`,
		tenantID, repoType, ns, name, upstream, pinnedSHA, creator)
	var m Mirror
	m.TenantID = tenantID
	m.Type = repoType
	m.Namespace = ns
	m.Name = name
	m.UpstreamHost = upstream
	m.PinnedSHA = pinnedSHA
	if err := row.Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt); err != nil {
		if isUniqueViolation(err) {
			return Mirror{}, ErrAlreadyExists
		}
		return Mirror{}, err
	}
	return m, nil
}

// GetMirror returns the mirror declaration for a (tenant, type, ns,
// name) or ErrNotFound when none.
func (s *Store) GetMirror(ctx context.Context, tenantID, repoType, ns, name string) (Mirror, error) {
	var m Mirror
	err := s.pool.QueryRow(ctx, `
SELECT id, tenant_id, repo_type, namespace, name, upstream_host, COALESCE(pinned_sha,''), created_at, updated_at
FROM mirrors
WHERE tenant_id=$1 AND repo_type=$2 AND namespace=$3 AND name=$4 AND deleted_at IS NULL`,
		tenantID, repoType, ns, name).
		Scan(&m.ID, &m.TenantID, &m.Type, &m.Namespace, &m.Name, &m.UpstreamHost, &m.PinnedSHA, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Mirror{}, ErrNotFound
	}
	return m, err
}

// ---- helpers ----

func validateRepoTriple(repoType, ns, name string) error {
	switch repoType {
	case "models", "datasets", "spaces":
	default:
		return fmt.Errorf("%w: repo_type=%q", ErrInvalidInput, repoType)
	}
	if !isRepoSlug(ns) || !isRepoSlug(name) {
		return fmt.Errorf("%w: invalid ns/name", ErrInvalidInput)
	}
	return nil
}

func isRepoSlug(s string) bool {
	if len(s) == 0 || len(s) > 96 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

func isHexN(s string, n int) bool { return len(s) == n && isHex(s) }

// deriveCommitSHA produces a 40-char hex SHA suitable for a git-like
// commit hash. The input mixes summary, parent_sha, current time and
// 16 bytes of crypto/rand so concurrent commits with identical
// summaries on the same parent at the same nanosecond do not
// collide. The (repo_id, sha) UNIQUE constraint backstops the random
// path with a hard correctness guarantee, but we want the happy path
// to never hit it.
func deriveCommitSHA(summary string, now time.Time, parent string) string {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%x", summary, parent, now.UnixNano(), nonce)))
	return hex.EncodeToString(h[:])[:40]
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// pgx surfaces SQLSTATE in err.Error(); we don't want a hard
	// dependency on pgconn here, so check the canonical 23505 code.
	return strings.Contains(err.Error(), "23505") ||
		strings.Contains(err.Error(), "duplicate key")
}
