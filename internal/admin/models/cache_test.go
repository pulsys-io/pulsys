// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package models_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pulsys-io/pulsys/internal/admin/models"
	"github.com/pulsys-io/pulsys/internal/cache"
)

func writeCacheMeta(t *testing.T, dir, keyDir, meta string) {
	t.Helper()
	root := filepath.Join(dir, "v1", "objects", keyDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListFromCacheEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := models.ListFromCache(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil slice, got %v", got)
	}
}

func TestListFromCacheDedupe(t *testing.T) {
	dir := t.TempDir()
	writeCacheMeta(t, dir, "abc123", `{"upstream_host":"huggingface.co","path":"/gpt2/resolve/main/config.json","status_code":200,"total":42}`)
	got, err := models.ListFromCache(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len %d", len(got))
	}
	if got[0].Path != "/gpt2/resolve/main/config.json" {
		t.Fatalf("path %q", got[0].Path)
	}
}

func TestListGroupedFromCache_ParsesHFPaths(t *testing.T) {
	dir := t.TempDir()
	writeCacheMeta(t, dir, "k1", `{"upstream_host":"huggingface.co","path":"/Qwen/Qwen2.5-0.5B/resolve/main/config.json","status_code":200,"total":100}`)
	writeCacheMeta(t, dir, "k2", `{"upstream_host":"huggingface.co","path":"/Qwen/Qwen2.5-0.5B/resolve/main/model.safetensors","status_code":200,"total":200}`)
	writeCacheMeta(t, dir, "k3", `{"upstream_host":"huggingface.co","path":"/datasets/huggingface/squad/resolve/main/train.json","status_code":200,"total":50}`)

	got, err := models.ListGroupedFromCache(dir, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items=%d", len(got.Items))
	}
	if got.Items[0].Org != "Qwen" || got.Items[0].Name != "Qwen2.5-0.5B" {
		t.Fatalf("first group %+v", got.Items[0])
	}
	if len(got.Items[0].Files) != 2 {
		t.Fatalf("files=%d", len(got.Items[0].Files))
	}
	var foundDataset bool
	for _, g := range got.Items {
		if g.Org == "huggingface" && g.Name == "squad" {
			foundDataset = true
		}
	}
	if !foundDataset {
		t.Fatalf("missing dataset group: %+v", got.Items)
	}
}

func TestListGroupedFromCache_AggregatesSize(t *testing.T) {
	dir := t.TempDir()
	writeCacheMeta(t, dir, "k1", `{"upstream_host":"huggingface.co","path":"/a/b/resolve/main/x","status_code":200,"total":1000}`)
	writeCacheMeta(t, dir, "k2", `{"upstream_host":"huggingface.co","path":"/a/b/resolve/main/y","status_code":200,"total":2500}`)

	got, err := models.ListGroupedFromCache(dir, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items=%d", len(got.Items))
	}
	if got.Items[0].TotalBytes != 3500 || got.Items[0].FileCount != 2 {
		t.Fatalf("group %+v", got.Items[0])
	}
	if got.GrandTotalBytes != 3500 {
		t.Fatalf("grand=%d", got.GrandTotalBytes)
	}
}

func TestListGroupedFromCache_DropsUnparseable(t *testing.T) {
	dir := t.TempDir()
	writeCacheMeta(t, dir, "k1", `{"upstream_host":"huggingface.co","path":"/not-a-model-path","status_code":200,"total":10}`)
	writeCacheMeta(t, dir, "k2", `{"upstream_host":"huggingface.co","path":"/m/n/resolve/main/f","status_code":200,"total":20}`)

	got, err := models.ListGroupedFromCache(dir, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items=%d", len(got.Items))
	}
	if got.GrandTotalBytes != 20 {
		t.Fatalf("grand=%d (should ignore unparseable entries)", got.GrandTotalBytes)
	}
}

func TestListGroupedFromCache_SkipsRedirects(t *testing.T) {
	dir := t.TempDir()
	writeCacheMeta(t, dir, "k1", `{"upstream_host":"huggingface.co","path":"/o/n/resolve/main/f","status_code":302,"total":0}`)
	writeCacheMeta(t, dir, "k2", `{"upstream_host":"huggingface.co","path":"/o/n/resolve/main/g","status_code":200,"total":500}`)

	got, err := models.ListGroupedFromCache(dir, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].FileCount != 1 || got.Items[0].TotalBytes != 500 {
		t.Fatalf("group %+v", got.Items[0])
	}
}

func TestListGroupedFromCache_ResolveCachePath(t *testing.T) {
	dir := t.TempDir()
	writeCacheMeta(t, dir, "k1", `{"upstream_host":"huggingface.co","path":"/api/resolve-cache/models/Qwen/Qwen2.5-0.5B/abc123/config.json","status_code":200,"total":100}`)
	writeCacheMeta(t, dir, "k2", `{"upstream_host":"huggingface.co","path":"/api/resolve-cache/datasets/huggingface/squad/v1/train.json","status_code":200,"total":200}`)

	got, err := models.ListGroupedFromCache(dir, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items=%d", len(got.Items))
	}
	var qwen *models.ModelGroup
	for i := range got.Items {
		if got.Items[i].Name == "Qwen2.5-0.5B" {
			qwen = &got.Items[i]
		}
	}
	if qwen == nil || qwen.Org != "Qwen" || qwen.TotalBytes != 100 {
		t.Fatalf("qwen group missing: %+v", got.Items)
	}
	if len(qwen.Files) != 1 || qwen.Files[0].Path != "config.json" {
		t.Fatalf("files=%+v", qwen.Files)
	}
}

func TestListGroupedFromCache_DedupesAcrossPathShapes(t *testing.T) {
	dir := t.TempDir()
	writeCacheMeta(t, dir, "k1", `{"upstream_host":"huggingface.co","path":"/Org/M/resolve/main/config.json","status_code":200,"total":100}`)
	writeCacheMeta(t, dir, "k2", `{"upstream_host":"huggingface.co","path":"/api/resolve-cache/models/Org/M/main/config.json","status_code":200,"total":100}`)

	got, err := models.ListGroupedFromCache(dir, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].FileCount != 1 || got.Items[0].TotalBytes != 100 {
		t.Fatalf("expected 1 dedup'd file, got %+v", got.Items)
	}
}

// Content-addressed Xet/LFS bodies have no model identity in `path` -- it's
// just a chunk hash on cas-bridge.xethub.hf.co.  Without the proxy's
// origin_path attribution they'd be silently dropped from the listing
// (the cached LFS body is the bulk of any real model's disk usage).
func TestListGroupedFromCache_XetBodyAttributedViaOriginPath(t *testing.T) {
	dir := t.TempDir()
	// 1 KB resolve-cache metadata (small text files)
	writeCacheMeta(t, dir, "k1", `{"upstream_host":"huggingface.co","path":"/api/resolve-cache/models/Qwen/Qwen2.5-0.5B/abc123/config.json","status_code":200,"total":1024}`)
	// 942 MB cas-bridge body with origin_path threaded in
	writeCacheMeta(t, dir, "k2", `{"upstream_host":"cas-bridge.xethub.hf.co","path":"/xet-bridge-us/abc/def","origin_path":"/Qwen/Qwen2.5-0.5B/resolve/main/model.safetensors","status_code":200,"total":988097824}`)

	got, err := models.ListGroupedFromCache(dir, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items=%d", len(got.Items))
	}
	g := got.Items[0]
	if g.Org != "Qwen" || g.Name != "Qwen2.5-0.5B" {
		t.Fatalf("wrong group: %+v", g)
	}
	if g.FileCount != 2 {
		t.Fatalf("file_count=%d (want 2: config.json + model.safetensors)", g.FileCount)
	}
	if g.TotalBytes != 1024+988097824 {
		t.Fatalf("total_bytes=%d (Xet body bytes dropped)", g.TotalBytes)
	}
	var hasSafetensors bool
	for _, f := range g.Files {
		if f.Path == "model.safetensors" {
			hasSafetensors = true
			if f.TotalBytes == nil || *f.TotalBytes != 988097824 {
				t.Fatalf("safetensors size wrong: %+v", f)
			}
		}
	}
	if !hasSafetensors {
		t.Fatalf("model.safetensors missing from files: %+v", g.Files)
	}
}

func TestListGroupedFromCache_SortedBySizeDesc(t *testing.T) {
	dir := t.TempDir()
	writeCacheMeta(t, dir, "k1", `{"upstream_host":"huggingface.co","path":"/small/x/resolve/main/a","status_code":200,"total":10}`)
	writeCacheMeta(t, dir, "k2", `{"upstream_host":"huggingface.co","path":"/big/y/resolve/main/b","status_code":200,"total":9999}`)

	got, err := models.ListGroupedFromCache(dir, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items=%d", len(got.Items))
	}
	if got.Items[0].Name != "y" || got.Items[1].Name != "x" {
		t.Fatalf("order wrong: %s then %s", got.Items[0].Name, got.Items[1].Name)
	}
}

func TestPrimaryAttribution(t *testing.T) {
	cases := []struct {
		name string
		meta cache.Meta
		want [3]string
		ok   bool
	}{
		{
			name: "origin_path wins",
			meta: cache.Meta{
				OriginPath:  "/A/M/resolve/main/x",
				OriginPaths: []string{"/B/M/resolve/main/x"},
				Path:        "/xet-bridge-us/h/s",
			},
			want: [3]string{"A", "M", "main"}, ok: true,
		},
		{
			name: "fallback to first parseable origin_paths entry",
			meta: cache.Meta{
				OriginPaths: []string{"/not-a-model", "/B/M/resolve/v1/y"},
				Path:        "/xet-bridge-us/h/s",
			},
			want: [3]string{"B", "M", "v1"}, ok: true,
		},
		{
			name: "fallback to path when no owners",
			meta: cache.Meta{Path: "/C/N/resolve/main/z"},
			want: [3]string{"C", "N", "main"}, ok: true,
		},
		{
			name: "nothing parseable",
			meta: cache.Meta{Path: "/opaque/hash"},
			want: [3]string{"", "", ""}, ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, n, r, ok := models.PrimaryAttribution(&tc.meta)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v", ok, tc.ok)
			}
			if o != tc.want[0] || n != tc.want[1] || r != tc.want[2] {
				t.Fatalf("got (%q,%q,%q) want %v", o, n, r, tc.want)
			}
		})
	}
}

func TestOwnedBy_ConsidersAllOwners(t *testing.T) {
	m := &cache.Meta{
		OriginPath:  "/A/M/resolve/main/x",
		OriginPaths: []string{"/A/M/resolve/main/x", "/B/M/resolve/main/x"},
		Path:        "/xet-bridge-us/h/s",
	}
	if !models.OwnedBy(m, "A", "M") {
		t.Fatal("A/M expected to be an owner")
	}
	if !models.OwnedBy(m, "B", "M") {
		t.Fatal("B/M expected to be an owner via origin_paths")
	}
	if models.OwnedBy(m, "C", "M") {
		t.Fatal("C/M unexpectedly reported as owner")
	}
}

// Same-host metadata bodies have no OriginPath but their Path parses.
// OwnedBy must still see the model as the owner so purge can route
// these entries through DecisionRemove.  HasAnyOwner deliberately
// returns false post-trim so the handler removes the body outright
// (Path is the cache key's provenance, not a removable owner -- it
// names exactly one model and there is nothing left to "trim" to).
func TestOwnedBy_ViaPathForSameHostEntries(t *testing.T) {
	m := &cache.Meta{Path: "/C/N/resolve/main/z"}
	if !models.OwnedBy(m, "C", "N") {
		t.Fatal("C/N path owner missed")
	}
	trimmed := models.RemoveOwner(m, "C", "N")
	if models.HasAnyOwner(trimmed) {
		t.Fatal("HasAnyOwner expected false: Path is not a trimmable owner")
	}
}

func TestRemoveOwner_PromotesAndClears(t *testing.T) {
	t.Run("promotes_next_when_origin_path_matches", func(t *testing.T) {
		m := &cache.Meta{
			OriginPath:  "/A/M/resolve/main/x",
			OriginPaths: []string{"/A/M/resolve/main/x", "/B/M/resolve/main/x"},
			Path:        "/xet-bridge-us/h/s",
		}
		got := models.RemoveOwner(m, "A", "M")
		if got.OriginPath != "/B/M/resolve/main/x" {
			t.Fatalf("origin_path=%q want promoted to B", got.OriginPath)
		}
		if len(got.OriginPaths) != 1 || got.OriginPaths[0] != "/B/M/resolve/main/x" {
			t.Fatalf("origin_paths=%v want [B]", got.OriginPaths)
		}
		if !models.HasAnyOwner(got) {
			t.Fatal("HasAnyOwner expected true (B still owns)")
		}
	})

	t.Run("clears_when_last_owner_removed", func(t *testing.T) {
		m := &cache.Meta{
			OriginPath:  "/A/M/resolve/main/x",
			OriginPaths: []string{"/A/M/resolve/main/x"},
			Path:        "/xet-bridge-us/h/s",
		}
		got := models.RemoveOwner(m, "A", "M")
		if got.OriginPath != "" || len(got.OriginPaths) != 0 {
			t.Fatalf("not cleared: %+v", got)
		}
		if models.HasAnyOwner(got) {
			t.Fatal("HasAnyOwner expected false (no remaining owners and Path is opaque)")
		}
	})
}

func TestListGroupedFromCache_RevisionsSorted(t *testing.T) {
	dir := t.TempDir()
	writeCacheMeta(t, dir, "k1", `{"upstream_host":"huggingface.co","path":"/o/n/resolve/main/a","status_code":200,"total":1}`)
	writeCacheMeta(t, dir, "k2", `{"upstream_host":"huggingface.co","path":"/o/n/resolve/v1.0/b","status_code":200,"total":1}`)
	writeCacheMeta(t, dir, "k3", `{"upstream_host":"huggingface.co","path":"/o/n/resolve/abc123/c","status_code":200,"total":1}`)

	got, err := models.ListGroupedFromCache(dir, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items[0].Revisions) != 3 {
		t.Fatalf("revs=%v", got.Items[0].Revisions)
	}
	if got.Items[0].Revisions[0] != "abc123" || got.Items[0].Revisions[1] != "main" || got.Items[0].Revisions[2] != "v1.0" {
		t.Fatalf("revs=%v", got.Items[0].Revisions)
	}
}
