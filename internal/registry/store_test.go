// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package registry

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pulsys-io/pulsys/internal/blobstore"
	"github.com/pulsys-io/pulsys/internal/testpg"
)

// testPool returns a pgxpool against a fresh, fully-migrated test
// database via the testpg helper. Skips when no admin DSN is
// configured. Cleanup is registered automatically by testpg.
func testPool(t *testing.T) *pgxpool.Pool { return testpg.Acquire(t) }

// seedDefaultTenant inserts the single "default" tenant the Pulsys
// product expects at runtime and returns its uuid. Since every test
// uses a fresh database (template-cloned), there's no name conflict
// across tests or packages.
func seedDefaultTenant(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
INSERT INTO tenants (name, display_name) VALUES ('default', 'Default')
RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func TestRegistry_CreateRepo_ListFiles(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	tenantID := seedDefaultTenant(t, pool)
	bs, _ := blobstore.NewLocal(t.TempDir())

	r, err := s.CreateRepo(context.Background(), tenantID, "models", "acme", "widget", false, "")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if r.FullName() != "acme/widget" {
		t.Fatalf("full name=%s", r.FullName())
	}

	// Duplicate create -> ErrAlreadyExists.
	if _, err := s.CreateRepo(context.Background(), tenantID, "models", "acme", "widget", false, ""); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err=%v want ErrAlreadyExists", err)
	}

	// CommitTx with two inline files.
	cfg := []byte("{\"hidden_size\":768}")
	readme := []byte("# acme/widget")
	cfgStat, err := bs.Put(context.Background(), bytes.NewReader(cfg), blobstore.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	readmeStat, _ := bs.Put(context.Background(), bytes.NewReader(readme), blobstore.PutOptions{})

	if err := s.UpsertBlob(context.Background(), cfgStat.OID, cfgStat.Size, cfgStat.StorageURL); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertBlob(context.Background(), readmeStat.OID, readmeStat.Size, readmeStat.StorageURL); err != nil {
		t.Fatal(err)
	}

	res, err := s.CommitTx(context.Background(), CommitInput{
		RepoID:  r.ID,
		Branch:  "main",
		Summary: "Initial commit",
		Inline: map[string]InlineCommitFile{
			"config.json": {BlobOID: cfgStat.OID, Size: cfgStat.Size},
			"README.md":   {BlobOID: readmeStat.OID, Size: readmeStat.Size},
		},
	})
	if err != nil {
		t.Fatalf("CommitTx: %v", err)
	}
	if res.SHA == "" || res.Branch != "main" {
		t.Fatalf("bad commit result: %+v", res)
	}

	files, c, err := s.ListFiles(context.Background(), r.ID, res.SHA)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if c.SHA != res.SHA {
		t.Fatalf("commit sha=%s want %s", c.SHA, res.SHA)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files got %d", len(files))
	}
	if files["config.json"].BlobOID != cfgStat.OID {
		t.Fatalf("config.json oid mismatch")
	}
	if files["config.json"].IsLFS {
		t.Fatal("inline file marked LFS")
	}
}

func TestRegistry_CommitTx_LFSPointer(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	tenantID := seedDefaultTenant(t, pool)
	bs, _ := blobstore.NewLocal(t.TempDir())

	r, _ := s.CreateRepo(context.Background(), tenantID, "models", "ns", "name", false, "")

	body := bytes.Repeat([]byte{0xAA}, 4096)
	st, _ := bs.Put(context.Background(), bytes.NewReader(body), blobstore.PutOptions{})
	if err := s.UpsertBlob(context.Background(), st.OID, st.Size, st.StorageURL); err != nil {
		t.Fatal(err)
	}

	res, err := s.CommitTx(context.Background(), CommitInput{
		RepoID:  r.ID,
		Summary: "add LFS",
		LFSPointers: map[string]LFSCommitFile{
			"weights.safetensors": {BlobOID: st.OID, Size: st.Size},
		},
	})
	if err != nil {
		t.Fatalf("CommitTx: %v", err)
	}
	files, _, _ := s.ListFiles(context.Background(), r.ID, res.SHA)
	fr := files["weights.safetensors"]
	if !fr.IsLFS {
		t.Fatal("expected is_lfs=true")
	}
	if fr.BlobOID != st.OID || fr.Size != int64(len(body)) {
		t.Fatalf("file rev=%+v", fr)
	}
}

func TestRegistry_CommitTx_RejectsMissingBlob(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	tenantID := seedDefaultTenant(t, pool)
	r, err := s.CreateRepo(context.Background(), tenantID, "models", "aa", "bb", false, "")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	bogus := strings.Repeat("0", 64)
	_, err = s.CommitTx(context.Background(), CommitInput{
		RepoID:  r.ID,
		Summary: "bogus",
		Inline:  map[string]InlineCommitFile{"x": {BlobOID: bogus, Size: 1}},
	})
	if !errors.Is(err, ErrBlobMissing) {
		t.Fatalf("err=%v want ErrBlobMissing", err)
	}
}

func TestRegistry_CommitTx_DeletesPath(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	tenantID := seedDefaultTenant(t, pool)
	bs, _ := blobstore.NewLocal(t.TempDir())
	r, _ := s.CreateRepo(context.Background(), tenantID, "models", "a", "b", false, "")

	st1, _ := bs.Put(context.Background(), bytes.NewReader([]byte("v1")), blobstore.PutOptions{})
	st2, _ := bs.Put(context.Background(), bytes.NewReader([]byte("keep")), blobstore.PutOptions{})
	_ = s.UpsertBlob(context.Background(), st1.OID, st1.Size, st1.StorageURL)
	_ = s.UpsertBlob(context.Background(), st2.OID, st2.Size, st2.StorageURL)

	if _, err := s.CommitTx(context.Background(), CommitInput{
		RepoID:  r.ID,
		Summary: "seed",
		Inline: map[string]InlineCommitFile{
			"old.txt":  {BlobOID: st1.OID, Size: st1.Size},
			"keep.txt": {BlobOID: st2.OID, Size: st2.Size},
		},
	}); err != nil {
		t.Fatal(err)
	}

	res2, err := s.CommitTx(context.Background(), CommitInput{
		RepoID:  r.ID,
		Summary: "drop old",
		Deletes: []string{"old.txt"},
	})
	if err != nil {
		t.Fatalf("commit2: %v", err)
	}
	files, _, _ := s.ListFiles(context.Background(), r.ID, res2.SHA)
	if _, ok := files["old.txt"]; ok {
		t.Fatal("old.txt should be deleted")
	}
	if _, ok := files["keep.txt"]; !ok {
		t.Fatal("keep.txt should still be present")
	}
}

func TestRegistry_CommitTx_AdvancesBranch(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	tenantID := seedDefaultTenant(t, pool)
	bs, _ := blobstore.NewLocal(t.TempDir())
	r, _ := s.CreateRepo(context.Background(), tenantID, "models", "a", "b", false, "")

	st, _ := bs.Put(context.Background(), bytes.NewReader([]byte("c")), blobstore.PutOptions{})
	_ = s.UpsertBlob(context.Background(), st.OID, st.Size, st.StorageURL)

	r1, _ := s.CommitTx(context.Background(), CommitInput{
		RepoID:  r.ID,
		Summary: "c1",
		Inline:  map[string]InlineCommitFile{"f": {BlobOID: st.OID, Size: st.Size}},
	})
	r2, _ := s.CommitTx(context.Background(), CommitInput{
		RepoID:  r.ID,
		Summary: "c2",
		Inline:  map[string]InlineCommitFile{"g": {BlobOID: st.OID, Size: st.Size}},
	})
	if r1.SHA == r2.SHA {
		t.Fatal("expected different shas")
	}
	sha, err := s.ResolveBranch(context.Background(), r.ID, "main")
	if err != nil || sha != r2.SHA {
		t.Fatalf("branch sha=%s want %s err=%v", sha, r2.SHA, err)
	}
}

func TestRegistry_ResolveBranch_BranchShaShortSha(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	tenantID := seedDefaultTenant(t, pool)
	bs, _ := blobstore.NewLocal(t.TempDir())
	r, _ := s.CreateRepo(context.Background(), tenantID, "models", "a", "b", false, "")

	st, _ := bs.Put(context.Background(), bytes.NewReader([]byte("c")), blobstore.PutOptions{})
	_ = s.UpsertBlob(context.Background(), st.OID, st.Size, st.StorageURL)

	rc, _ := s.CommitTx(context.Background(), CommitInput{
		RepoID:  r.ID,
		Summary: "c",
		Inline:  map[string]InlineCommitFile{"f": {BlobOID: st.OID, Size: st.Size}},
	})

	for _, ref := range []string{"main", rc.SHA, rc.SHA[:8]} {
		got, err := s.ResolveBranch(context.Background(), r.ID, ref)
		if err != nil {
			t.Fatalf("ref=%s err=%v", ref, err)
		}
		if got != rc.SHA {
			t.Fatalf("ref=%s got %s want %s", ref, got, rc.SHA)
		}
	}

	if _, err := s.ResolveBranch(context.Background(), r.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestRegistry_BlobRefcount(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	tenantID := seedDefaultTenant(t, pool)
	bs, _ := blobstore.NewLocal(t.TempDir())
	r, _ := s.CreateRepo(context.Background(), tenantID, "models", "a", "b", false, "")

	st, _ := bs.Put(context.Background(), bytes.NewReader([]byte("hello")), blobstore.PutOptions{})
	_ = s.UpsertBlob(context.Background(), st.OID, st.Size, st.StorageURL)

	// First commit references blob.
	if _, err := s.CommitTx(context.Background(), CommitInput{
		RepoID:  r.ID,
		Summary: "c1",
		Inline:  map[string]InlineCommitFile{"f": {BlobOID: st.OID, Size: st.Size}},
	}); err != nil {
		t.Fatal(err)
	}
	// Second commit references the SAME blob at SAME path - no bump.
	if _, err := s.CommitTx(context.Background(), CommitInput{
		RepoID:  r.ID,
		Summary: "c2 no-op",
		Inline:  map[string]InlineCommitFile{"f": {BlobOID: st.OID, Size: st.Size}},
	}); err != nil {
		t.Fatal(err)
	}
	// Third commit adds the SAME blob at a NEW path - one bump.
	if _, err := s.CommitTx(context.Background(), CommitInput{
		RepoID:  r.ID,
		Summary: "c3 new path same blob",
		Inline:  map[string]InlineCommitFile{"g": {BlobOID: st.OID, Size: st.Size}},
	}); err != nil {
		t.Fatal(err)
	}

	b, ok, err := s.HasBlob(context.Background(), st.OID)
	if err != nil || !ok {
		t.Fatalf("HasBlob err=%v ok=%v", err, ok)
	}
	if b.RefCount != 2 {
		t.Fatalf("refcount=%d want 2 (c1 first ref, c3 new path; c2 no bump)", b.RefCount)
	}
}

func TestRegistry_ConcurrentCommits_NoDeadlock(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	tenantID := seedDefaultTenant(t, pool)
	bs, _ := blobstore.NewLocal(t.TempDir())
	r, _ := s.CreateRepo(context.Background(), tenantID, "models", "a", "b", false, "")

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			st, _ := bs.Put(context.Background(),
				bytes.NewReader([]byte{byte(i)}), blobstore.PutOptions{})
			_ = s.UpsertBlob(context.Background(), st.OID, st.Size, st.StorageURL)
			for attempt := 0; attempt < 3; attempt++ {
				_, err := s.CommitTx(context.Background(), CommitInput{
					RepoID:  r.ID,
					Summary: "c",
					Inline:  map[string]InlineCommitFile{"f": {BlobOID: st.OID, Size: st.Size}},
				})
				if err == nil {
					return
				}
				// Serializable can produce 40001 retries; retry on conflict.
				if !strings.Contains(err.Error(), "40001") && !strings.Contains(err.Error(), "could not serialize") {
					t.Errorf("commit: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestRegistry_Mirrors(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	tenantID := seedDefaultTenant(t, pool)

	m, err := s.CreateMirror(context.Background(), tenantID, "models", "facebook", "opt-125m", "", "", "")
	if err != nil {
		t.Fatalf("CreateMirror: %v", err)
	}
	if m.UpstreamHost != "huggingface.co" {
		t.Fatalf("default upstream=%s", m.UpstreamHost)
	}

	got, err := s.GetMirror(context.Background(), tenantID, "models", "facebook", "opt-125m")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != m.ID {
		t.Fatal("mismatch")
	}

	if _, err := s.CreateMirror(context.Background(), tenantID, "models", "facebook", "opt-125m", "", "", ""); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err=%v want ErrAlreadyExists", err)
	}

	if _, err := s.GetMirror(context.Background(), tenantID, "models", "missing", "name"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestRegistry_InvalidRepoSlug(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	tenantID := seedDefaultTenant(t, pool)
	for _, bad := range []string{"", "_starts-with-underscore", "../etc/passwd", "has space", strings.Repeat("a", 200)} {
		_, err := s.CreateRepo(context.Background(), tenantID, "models", "ns", bad, false, "")
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("name=%q err=%v want ErrInvalidInput", bad, err)
		}
	}
	for _, bt := range []string{"", "modelz", "users"} {
		_, err := s.CreateRepo(context.Background(), tenantID, bt, "ns", "name", false, "")
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("type=%q err=%v want ErrInvalidInput", bt, err)
		}
	}
}
