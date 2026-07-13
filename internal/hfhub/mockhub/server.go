// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package mockhub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// RepoSpec describes a repository to pre-create on server boot.
type RepoSpec struct {
	Name         string            // "org/name", required
	Type         string            // "models" (default), "datasets", "spaces"
	Private      bool              // currently informational only
	Empty        bool              // when true no initial commit is seeded
	InitialFiles map[string][]byte // files in the seeded "Initial commit"
}

// Config controls the mock server. The zero value is valid and yields
// a permissive server that accepts any bearer token.
type Config struct {
	Tokens      []Token    // bearer tokens to recognize; empty = accept anything
	RequireAuth bool       // when true, requests without a token return 401
	Repos       []RepoSpec // pre-seeded repos
}

// Server is the mock HF Hub.
type Server struct {
	hs    *httptest.Server
	state *state
	url   string
}

// New starts a mockhub bound to a random port on 127.0.0.1 and
// registers t.Cleanup to close it. Returns a Server whose URL is
// safe to plug into HF_ENDPOINT / -default-upstream-host.
func New(t testing.TB, cfg Config) *Server {
	t.Helper()
	s := &Server{state: newState(cfg)}
	s.hs = httptest.NewServer(s.router())
	s.url = s.hs.URL
	t.Cleanup(s.hs.Close)
	return s
}

// NewUnstarted returns a Server with its httptest.Server unstarted
// so callers can adjust TLS or listeners before .Start(). Intended
// for advanced harness wiring.
func NewUnstarted(cfg Config) *Server {
	s := &Server{state: newState(cfg)}
	s.hs = httptest.NewUnstartedServer(s.router())
	return s
}

// Start activates an Unstarted mock and captures its URL.
func (s *Server) Start() {
	s.hs.Start()
	s.url = s.hs.URL
}

// Close shuts down the underlying httptest server.
func (s *Server) Close() {
	if s.hs != nil {
		s.hs.Close()
	}
}

// URL returns the http://127.0.0.1:PORT base used by HF clients.
func (s *Server) URL() string {
	return s.url
}

// Handler returns the underlying http.Handler so callers can mount
// the mock under custom listeners (e.g. in the proxy's coreserver).
func (s *Server) Handler() http.Handler {
	return s.hs.Config.Handler
}

// AddRepo creates a new (initially empty) repo.
func (s *Server) AddRepo(repoType, name string) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	key := repoKey(repoType, name)
	if _, ok := s.state.repos[key]; ok {
		return
	}
	s.state.repos[key] = newRepo(name, repoType)
}

// SeedFile creates or updates a file in the repo's HEAD on main as a
// fresh commit. Returns the resulting commit OID.
func (s *Server) SeedFile(repoType, name, path string, body []byte) string {
	key := repoKey(repoType, name)
	s.state.mu.Lock()
	if _, ok := s.state.repos[key]; !ok {
		s.state.repos[key] = newRepo(name, repoType)
	}
	r := s.state.repos[key]
	sha := s.state.appendCommit(r, "seed "+path, "", map[string][]byte{path: body}, nil, nil)
	s.state.mu.Unlock()
	return sha
}

// SeedLFSPointer adds an LFS pointer file (with a prior body upload).
// Returns the commit sha.
func (s *Server) SeedLFSPointer(repoType, name, path string, body []byte) string {
	oid := s.state.putLFSObject(body)
	key := repoKey(repoType, name)
	s.state.mu.Lock()
	if _, ok := s.state.repos[key]; !ok {
		s.state.repos[key] = newRepo(name, repoType)
	}
	r := s.state.repos[key]
	sha := s.state.appendCommit(r, "seed lfs "+path, "", nil,
		map[string]lfsPointerSpec{path: {oid: oid, size: int64(len(body))}}, nil)
	s.state.mu.Unlock()
	return sha
}

// HeadCommitSHA returns the current main branch SHA for the repo, or
// "" when the repo is missing.
func (s *Server) HeadCommitSHA(repoType, name string) string {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	r, ok := s.state.repos[repoKey(repoType, name)]
	if !ok {
		return ""
	}
	return r.branches["main"]
}

// FileBytes returns the bytes currently stored at (repo, rev, path).
func (s *Server) FileBytes(repoType, name, rev, path string) ([]byte, bool) {
	_, body, ok := s.state.fileBytes(repoType, name, rev, path)
	if !ok {
		return nil, false
	}
	return append([]byte(nil), body...), true
}

// CallCount returns the number of times a method+pathPattern has been
// served. Pattern keys are the same strings the handlers pass to
// state.incCall; see handlers.go.
func (s *Server) CallCount(method, pathPattern string) uint64 {
	return s.state.readCall(method, pathPattern)
}
