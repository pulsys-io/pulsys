// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package mockhub

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Role is the authorisation level a bearer token grants.
type Role int

const (
	// RoleAnonymous is the default for requests without a recognized
	// bearer token, used when Config.RequireAuth is false.
	RoleAnonymous Role = iota
	// RoleRead allows model info, tree, paths-info, resolve, LFS
	// downloads.
	RoleRead
	// RoleWrite allows everything Read does plus preupload, commit,
	// LFS batch (upload mode), LFS object PUT, LFS verify.
	RoleWrite
	// RoleExpired is a sentinel; tokens with this role always 401.
	RoleExpired
)

// Token associates a bearer token with a role.
type Token struct {
	Value string
	Role  Role
}

// fileEntry models one file in a repo at a particular revision.
type fileEntry struct {
	Path       string
	Size       int64
	OID        string // sha256 of bytes (full bytes for LFS, ETag for inline)
	IsLFS      bool
	Body       []byte // empty for LFS pointer files; LFS body lives in lfsStore
	CommitOID  string
	CommitTime time.Time
}

// commit is a snapshot in a repo's history.
type commit struct {
	OID       string
	Summary   string
	Desc      string
	ParentOID string
	Time      time.Time
	// files maps path -> *fileEntry at this commit.
	files map[string]*fileEntry
}

// repo is the server-side state for one HF repository.
type repo struct {
	Name     string
	Type     string // models, datasets, spaces (default: models)
	Private  bool
	branches map[string]string // ref name -> commit oid
	commits  map[string]*commit
}

func newRepo(name, repoType string) *repo {
	return &repo{
		Name:     name,
		Type:     repoType,
		branches: map[string]string{"main": ""},
		commits:  make(map[string]*commit),
	}
}

// headCommit returns the commit pointed to by branchOrSHA. Accepts
// branch names ("main"), full SHAs, and short SHAs.
func (r *repo) headCommit(branchOrSHA string) (*commit, bool) {
	if sha, ok := r.branches[branchOrSHA]; ok {
		if sha == "" {
			return &commit{
				OID:   emptyCommitSHA,
				files: map[string]*fileEntry{},
				Time:  time.Now().UTC(),
			}, true
		}
		c, ok := r.commits[sha]
		return c, ok
	}
	if c, ok := r.commits[branchOrSHA]; ok {
		return c, true
	}
	for sha, c := range r.commits {
		if len(branchOrSHA) >= 7 && len(sha) >= len(branchOrSHA) && sha[:len(branchOrSHA)] == branchOrSHA {
			return c, true
		}
	}
	return nil, false
}

// emptyCommitSHA is the conventional "no commits yet" sha (Git's
// empty-tree hash). HF returns commit info on /tree even for repos
// with no commits, so the mock needs a stable sentinel.
const emptyCommitSHA = "0000000000000000000000000000000000000000"

// state holds all in-memory data the mock serves from.
type state struct {
	mu       sync.Mutex
	repos    map[string]*repo // key: "models/{name}" / "datasets/{name}" / etc.
	lfsStore map[string][]byte
	tokens   []Token

	// counters expose per-endpoint call counts so tests can assert
	// "exactly one upstream call" invariants. Atomic so tests can
	// observe without holding mu.
	callCounts sync.Map // key: "<METHOD> <pathPattern>" -> *uint64

	requireAuth bool
}

func newState(cfg Config) *state {
	s := &state{
		repos:       make(map[string]*repo),
		lfsStore:    make(map[string][]byte),
		tokens:      append([]Token(nil), cfg.Tokens...),
		requireAuth: cfg.RequireAuth,
	}
	for _, spec := range cfg.Repos {
		key := repoKey(spec.Type, spec.Name)
		s.repos[key] = newRepo(spec.Name, spec.Type)
		if !spec.Empty {
			// Seed with an initial "Initial commit" containing the
			// given files. This matches huggingface_hub's expectation
			// that a fresh repo has at least .gitattributes.
			_, _ = s.commitDirect(key, "Initial commit", spec.InitialFiles)
		}
	}
	return s
}

// repoKey canonicalises (type, name) into the map key. Type defaults
// to "models" when empty so callers can pass "" for the common case.
func repoKey(repoType, name string) string {
	if repoType == "" {
		repoType = "models"
	}
	return repoType + "/" + name
}

// authorize validates an Authorization header value (with or without
// the "Bearer " prefix) against configured tokens. Returns the
// effective role. When no tokens are configured and RequireAuth is
// false, returns RoleAnonymous (treated as RoleWrite by all handlers).
func (s *state) authorize(header string) (Role, error) {
	value := stripBearer(header)
	if len(s.tokens) == 0 {
		if value != "" {
			return RoleWrite, nil
		}
		if s.requireAuth {
			return 0, errUnauthorized
		}
		return RoleAnonymous, nil
	}
	for _, t := range s.tokens {
		if t.Value != "" && t.Value == value {
			if t.Role == RoleExpired {
				return 0, errUnauthorized
			}
			return t.Role, nil
		}
	}
	if value == "" {
		if s.requireAuth {
			return 0, errUnauthorized
		}
		return RoleAnonymous, nil
	}
	return 0, errUnauthorized
}

var errUnauthorized = errors.New("mockhub: invalid bearer token")

func stripBearer(h string) string {
	if len(h) >= 7 && (h[:7] == "Bearer " || h[:7] == "bearer ") {
		return h[7:]
	}
	return h
}

// commitDirect creates a synthetic commit; used for seeding state in
// tests. Files map is path -> bytes (inline, not LFS). Returns the
// new commit OID.
func (s *state) commitDirect(repoKey, summary string, files map[string][]byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey]
	if !ok {
		return "", fmt.Errorf("mockhub: unknown repo %q", repoKey)
	}
	return s.appendCommit(r, summary, "", files, nil, nil), nil
}

// appendCommit creates a new commit on the repo's main branch and
// returns its sha. Must be called with s.mu held.
//
//   - inlineFiles: path -> bytes added/updated this commit
//   - lfsPointers: path -> {oid, size} for LFS pointer files added
//   - deletes:     paths removed in this commit
func (s *state) appendCommit(r *repo, summary, desc string,
	inlineFiles map[string][]byte,
	lfsPointers map[string]lfsPointerSpec,
	deletes []string,
) string {
	now := time.Now().UTC()
	parent := r.branches["main"]
	var parentFiles map[string]*fileEntry
	if parent != "" && r.commits[parent] != nil {
		parentFiles = r.commits[parent].files
	}
	newFiles := make(map[string]*fileEntry, len(parentFiles)+len(inlineFiles)+len(lfsPointers))
	for p, f := range parentFiles {
		newFiles[p] = f
	}
	for _, d := range deletes {
		delete(newFiles, d)
	}
	commitSHA := deriveSHA(summary, now, parent)
	for path, body := range inlineFiles {
		oid := sha256Hex(body)
		newFiles[path] = &fileEntry{
			Path:       path,
			Size:       int64(len(body)),
			OID:        oid,
			IsLFS:      false,
			Body:       append([]byte(nil), body...),
			CommitOID:  commitSHA,
			CommitTime: now,
		}
	}
	for path, ptr := range lfsPointers {
		newFiles[path] = &fileEntry{
			Path:       path,
			Size:       ptr.size,
			OID:        ptr.oid,
			IsLFS:      true,
			CommitOID:  commitSHA,
			CommitTime: now,
		}
	}
	c := &commit{
		OID:       commitSHA,
		Summary:   summary,
		Desc:      desc,
		ParentOID: parent,
		Time:      now,
		files:     newFiles,
	}
	r.commits[commitSHA] = c
	r.branches["main"] = commitSHA
	return commitSHA
}

type lfsPointerSpec struct {
	oid  string
	size int64
}

// deriveSHA produces a stable 40-char hex string suitable for a fake
// commit hash. We hash summary+parent+time so successive commits with
// the same summary differ.
func deriveSHA(summary string, now time.Time, parent string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", summary, parent, now.UnixNano())))
	return hex.EncodeToString(h[:])[:40]
}

// sha256Hex returns the lower-case hex-encoded sha256 of body. HF Hub
// uses this same value as the LFS object oid.
func sha256Hex(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// listRepoFiles returns the file map for a (repo, revision) pair.
func (s *state) listRepoFiles(repoType, name, rev string) (map[string]*fileEntry, *commit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(repoType, name)]
	if !ok {
		return nil, nil, false
	}
	c, ok := r.headCommit(rev)
	if !ok {
		return nil, nil, false
	}
	out := make(map[string]*fileEntry, len(c.files))
	for k, v := range c.files {
		out[k] = v
	}
	return out, c, true
}

// fileBytes returns (entry, body, ok). For inline files body comes
// from the entry directly; for LFS pointer files body is looked up in
// lfsStore.
func (s *state) fileBytes(repoType, name, rev, path string) (*fileEntry, []byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(repoType, name)]
	if !ok {
		return nil, nil, false
	}
	c, ok := r.headCommit(rev)
	if !ok {
		return nil, nil, false
	}
	f, ok := c.files[path]
	if !ok {
		return nil, nil, false
	}
	if f.IsLFS {
		body, ok := s.lfsStore[f.OID]
		if !ok {
			return f, nil, false
		}
		return f, body, true
	}
	return f, f.Body, true
}

// putLFSObject stores body under its oid. Returns the canonical oid.
func (s *state) putLFSObject(body []byte) string {
	oid := sha256Hex(body)
	s.mu.Lock()
	s.lfsStore[oid] = append([]byte(nil), body...)
	s.mu.Unlock()
	return oid
}

// hasLFSObject reports whether oid is stored.
func (s *state) hasLFSObject(oid string) bool {
	s.mu.Lock()
	_, ok := s.lfsStore[oid]
	s.mu.Unlock()
	return ok
}

// hasFile reports whether the file exists at HEAD (for preupload dedup).
func (s *state) hasFile(repoType, name, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(repoType, name)]
	if !ok {
		return false
	}
	c, ok := r.headCommit("main")
	if !ok {
		return false
	}
	_, ok = c.files[path]
	return ok
}

// commitFromRequest applies a parsed NDJSON commit (additions + LFS
// pointers + deletes). Returns the new commit SHA or an error.
func (s *state) commitFromRequest(repoType, name, rev string, summary, desc string,
	inline map[string][]byte, lfsPtr map[string]lfsPointerSpec, deletes []string,
) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(repoType, name)]
	if !ok {
		return "", fmt.Errorf("repo %s/%s not found", repoType, name)
	}
	if rev != "" && rev != "main" {
		if _, ok := r.branches[rev]; !ok {
			// Auto-create feature branches on first commit, mirroring
			// huggingface_hub behavior with create_pr=False.
			r.branches[rev] = ""
		}
	}
	for path, ptr := range lfsPtr {
		if !s.hasObjectLocked(ptr.oid) {
			return "", fmt.Errorf("LFS object %s for path %s not uploaded yet", ptr.oid, path)
		}
		_ = path
	}
	return s.appendCommit(r, summary, desc, inline, lfsPtr, deletes), nil
}

func (s *state) hasObjectLocked(oid string) bool {
	_, ok := s.lfsStore[oid]
	return ok
}

// sortedPaths returns the paths in a file map, sorted for stable test
// output.
func sortedPaths(files map[string]*fileEntry) []string {
	out := make([]string, 0, len(files))
	for p := range files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// incCall bumps the counter for the (method, pathPattern) pair.
func (s *state) incCall(method, pathPattern string) {
	key := method + " " + pathPattern
	v, _ := s.callCounts.LoadOrStore(key, new(uint64))
	atomic.AddUint64(v.(*uint64), 1)
}

// readCall reads the counter for the (method, pathPattern) pair.
func (s *state) readCall(method, pathPattern string) uint64 {
	v, ok := s.callCounts.Load(method + " " + pathPattern)
	if !ok {
		return 0
	}
	return atomic.LoadUint64(v.(*uint64))
}
