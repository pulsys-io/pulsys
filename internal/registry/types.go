// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package registry

import (
	"errors"
	"time"
)

// Errors returned across the package surface.
var (
	ErrNotFound      = errors.New("registry: not found")
	ErrAlreadyExists = errors.New("registry: already exists")
	ErrInvalidInput  = errors.New("registry: invalid input")
	ErrBlobMissing   = errors.New("registry: referenced blob not present in blobstore")
	// ErrLFSSizeMismatch is returned when a commit references an
	// LFS pointer whose declared size in the NDJSON does not match
	// the size we recorded when the blob was uploaded.  Without
	// this check a client could commit a pointer claiming size N
	// for a blob of true size M, and downstream consumers (LFS
	// batch responses, file listings) would report N even though
	// the served bytes are M.  See CommitTx implementation.
	ErrLFSSizeMismatch = errors.New("registry: LFS pointer size does not match stored blob size")
	// ErrCommitPathInvalid is returned when a commit file path
	// fails validation (contains "..", leading "/", a NUL byte,
	// or exceeds the length cap).  Without this validation any
	// authenticated client can persist exotic strings into
	// file_revisions.path that downstream listings, archive
	// exports, or UIs may misinterpret.  Not a filesystem
	// traversal (the path is a DB string column), but a data-
	// hygiene + stored-XSS hardening boundary.
	ErrCommitPathInvalid = errors.New("registry: invalid commit file path")
)

// MaxCommitFilePathBytes is the upper bound on the byte length
// of a file path in a commit's file_revisions row.  1024 covers
// every reasonable repo layout (HF practice tops out around
// 200) while bounding the audit / listing payload size.
const MaxCommitFilePathBytes = 1024

// ValidateCommitFilePath returns ErrCommitPathInvalid for any
// path that should not be persisted via the commit NDJSON.
// Rules (intersection of safe-for-DB and safe-for-archive):
//
//   - non-empty, length <= MaxCommitFilePathBytes
//   - no NUL byte (corrupts most string handling)
//   - no leading "/" (paths are repo-relative)
//   - no ".." path segment (no parent escapes even though the
//     storage path is content-addressed; some UIs join paths
//     filesystem-style)
//   - no backslash (avoids OS-specific separators leaking)
//   - no embedded CR/LF (avoids log-injection if path is
//     logged unescaped)
func ValidateCommitFilePath(p string) error {
	if p == "" || len(p) > MaxCommitFilePathBytes {
		return ErrCommitPathInvalid
	}
	if p[0] == '/' {
		return ErrCommitPathInvalid
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == 0x00 || c == '\\' || c == '\r' || c == '\n' {
			return ErrCommitPathInvalid
		}
	}
	// Reject ".." as any path segment (split on "/").  An
	// embedded ".." substring inside a real filename (e.g.
	// "weights..bin") is fine; we only flag the segment form.
	for _, seg := range splitPathSegments(p) {
		if seg == ".." {
			return ErrCommitPathInvalid
		}
	}
	return nil
}

func splitPathSegments(p string) []string {
	var out []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			if i > start {
				out = append(out, p[start:i])
			}
			start = i + 1
		}
	}
	if start < len(p) {
		out = append(out, p[start:])
	}
	return out
}

// Repo is one repository owned by a tenant.
type Repo struct {
	ID        string
	TenantID  string
	Type      string // "models" | "datasets" | "spaces"
	Namespace string
	Name      string
	Private   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// FullName returns "namespace/name".
func (r Repo) FullName() string { return r.Namespace + "/" + r.Name }

// Commit is one snapshot in a repository's history.
type Commit struct {
	ID          string
	RepoID      string
	SHA         string
	ParentSHA   string
	Summary     string
	Description string
	CreatedAt   time.Time
}

// FileRevision is one path -> blob mapping within a commit.
type FileRevision struct {
	ID       string
	CommitID string
	Path     string
	BlobOID  string
	IsLFS    bool
	Size     int64 // populated from the joined blobs row
}

// Blob is one row in the globally-content-addressed blob table.
type Blob struct {
	OID        string
	Size       int64
	StorageURL string
	RefCount   int64
}

// Mirror tells the resolver "if this repo isn't local, forward to
// upstream_host" - the only way passthrough is engaged in v1.
type Mirror struct {
	ID           string
	TenantID     string
	Type         string
	Namespace    string
	Name         string
	UpstreamHost string
	PinnedSHA    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CommitInput is the data shape the upload handler hands to CommitTx.
// `Inline` entries carry their bytes directly (already written to the
// blobstore and verified by the caller); `LFSPointers` reference an
// already-uploaded LFS blob by oid + size. Deletes drop a path from
// the new commit's file_revisions.
type CommitInput struct {
	RepoID      string
	Branch      string // "main" or arbitrary feature branch
	Summary     string
	Description string
	AuthorID    string // optional user id

	// Inline references blobs that were written to the blobstore from
	// an inline `file` entry in the commit NDJSON. blob_oid was
	// computed by the blobstore Put step and is byte-verified.
	Inline map[string]InlineCommitFile
	// LFSPointers references blobs that already exist in the blobstore
	// from prior LFS PUT uploads.
	LFSPointers map[string]LFSCommitFile
	// Deletes is the set of paths removed in this commit.
	Deletes []string
}

// InlineCommitFile is a non-LFS file added/updated in a commit.
type InlineCommitFile struct {
	BlobOID string
	Size    int64
}

// LFSCommitFile is a LFS pointer file added/updated in a commit.
type LFSCommitFile struct {
	BlobOID string
	Size    int64
}

// CommitResult is what CommitTx returns on success.
type CommitResult struct {
	CommitID string
	SHA      string
	Branch   string
}
