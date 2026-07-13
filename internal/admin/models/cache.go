// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pulsys-io/pulsys/internal/cache"
)

// CachedModel is a cache-derived model/repo entry for the admin UI.
type CachedModel struct {
	Path         string `json:"path"`
	UpstreamHost string `json:"upstream_host"`
	StatusCode   int    `json:"status_code"`
	TotalBytes   *int64 `json:"total_bytes,omitempty"`
}

// ModelGroup aggregates cached files for one HF model/repo identity.
type ModelGroup struct {
	Org        string        `json:"org"`
	Name       string        `json:"name"`
	Upstream   string        `json:"upstream_host"`
	Revisions  []string      `json:"revisions"`
	FileCount  int           `json:"file_count"`
	TotalBytes int64         `json:"total_bytes"`
	Files      []CachedModel `json:"files,omitempty"`
}

// GroupedListing is the model-first cache inventory for the admin UI.
type GroupedListing struct {
	Items           []ModelGroup `json:"items"`
	GrandTotalBytes int64        `json:"grand_total_bytes"`
}

type metaFile struct {
	UpstreamHost string   `json:"upstream_host"`
	Path         string   `json:"path"`
	OriginPath   string   `json:"origin_path,omitempty"`
	OriginPaths  []string `json:"origin_paths,omitempty"`
	StatusCode   int      `json:"status_code"`
	Total        *int64   `json:"total,omitempty"`
}

// attributionPath returns the path we use to assign this cache entry
// to a model.  Content-addressed bodies (Xet/LFS) have an opaque hash
// in `path` and only carry model identity via `origin_path`, which the
// proxy threads through cross-host redirects.
func (m metaFile) attributionPath() string {
	if m.OriginPath != "" {
		return m.OriginPath
	}
	for _, op := range m.OriginPaths {
		if op != "" {
			return op
		}
	}
	return m.Path
}

// PrimaryAttribution picks the canonical (org, name, rev) for `m`.
// Order of precedence:
//  1. OriginPath (first-writer hint, kept for back-compat with
//     entries written before OriginPaths existed).
//  2. The first parseable entry in OriginPaths (already sorted).
//  3. m.Path itself (the same-host metadata case).
//
// Centralizing this decision is the whole point of the helper: every
// consumer that needs "which model does this entry belong to" goes
// through PrimaryAttribution, so the listing and the purge can never
// disagree the way they did before this change.
func PrimaryAttribution(m *cache.Meta) (org, name, rev string, ok bool) {
	if m == nil {
		return "", "", "", false
	}
	if m.OriginPath != "" {
		if o, n, r, ok := ParseHFModelPath(m.OriginPath); ok {
			return o, n, r, true
		}
	}
	for _, op := range m.OriginPaths {
		if o, n, r, ok := ParseHFModelPath(op); ok {
			return o, n, r, true
		}
	}
	return ParseHFModelPath(m.Path)
}

// OwnedBy reports whether (org, name) appears anywhere in the entry's
// owner set: OriginPath, any element of OriginPaths, or the entry's
// own Path (covering same-host metadata bodies that have no
// OriginPath at all).  Used by the purge handler so a shared body's
// deletion is gated by "is this the last owner of the body".
func OwnedBy(m *cache.Meta, org, name string) bool {
	if m == nil {
		return false
	}
	if matches(m.OriginPath, org, name) {
		return true
	}
	for _, op := range m.OriginPaths {
		if matches(op, org, name) {
			return true
		}
	}
	return matches(m.Path, org, name)
}

// matches is the per-path predicate shared by OwnedBy and
// RemoveOwner.  Returns true iff p parses as an HF model path and
// (org, name) match.  Centralized so we never compare the wrong
// segment of the parse result.
func matches(p, org, name string) bool {
	if p == "" {
		return false
	}
	o, n, _, ok := ParseHFModelPath(p)
	return ok && o == org && n == name
}

// RemoveOwner returns a copy of `m` with every owner reference to
// (org, name) stripped:
//
//   - OriginPath cleared when it parses to (org, name); the next
//     remaining OriginPaths entry (if any) is promoted into the
//     scalar so first-writer-attribution callers stay self-consistent.
//   - OriginPaths filtered to drop matching entries.
//
// The body file is NOT touched; this is the data-only part of the
// trim.  HasAnyOwner on the returned meta tells the caller whether
// the body should be deleted (no owners left) or just rewritten.
func RemoveOwner(m *cache.Meta, org, name string) *cache.Meta {
	if m == nil {
		return nil
	}
	out := *m
	out.OriginPaths = make([]string, 0, len(m.OriginPaths))
	for _, op := range m.OriginPaths {
		if !matches(op, org, name) {
			out.OriginPaths = append(out.OriginPaths, op)
		}
	}
	if len(out.OriginPaths) == 0 {
		out.OriginPaths = nil
	}
	if matches(out.OriginPath, org, name) {
		out.OriginPath = ""
		if len(out.OriginPaths) > 0 {
			out.OriginPath = out.OriginPaths[0]
		}
	}
	return &out
}

// HasAnyOwner reports whether the entry still claims at least one
// HF-parseable owner via OriginPath or OriginPaths.  Deliberately
// excludes m.Path: Path is the cache key's provenance and cannot be
// trimmed by meta rewrite, so a same-host metadata entry with no
// OriginPath always ends up with HasAnyOwner == false after
// RemoveOwner -- which routes the purge handler to DecisionRemove
// (the correct outcome: one owner per same-host entry).
func HasAnyOwner(m *cache.Meta) bool {
	if m == nil {
		return false
	}
	if m.OriginPath != "" {
		if _, _, _, ok := ParseHFModelPath(m.OriginPath); ok {
			return true
		}
	}
	for _, op := range m.OriginPaths {
		if _, _, _, ok := ParseHFModelPath(op); ok {
			return true
		}
	}
	return false
}

// ListFromCache scans cacheDir/v1/objects/*/meta.json and returns unique
// model paths (HF-style /org/name/...).  Scan stops after maxEntries.
func ListFromCache(cacheDir string, maxEntries int) ([]CachedModel, error) {
	if maxEntries <= 0 || maxEntries > 5000 {
		maxEntries = 500
	}
	root := filepath.Join(cacheDir, "v1", "objects")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	seen := make(map[string]CachedModel)
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		metaPath := filepath.Join(root, ent.Name(), "meta.json")
		b, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var m metaFile
		if err := json.Unmarshal(b, &m); err != nil || m.Path == "" {
			continue
		}
		// Keep only model-like paths (resolve/main or datasets).
		if !strings.Contains(m.Path, "/") {
			continue
		}
		key := m.UpstreamHost + "|" + m.Path
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = CachedModel{
			Path:         m.Path,
			UpstreamHost: m.UpstreamHost,
			StatusCode:   m.StatusCode,
			TotalBytes:   m.Total,
		}
		if len(seen) >= maxEntries {
			break
		}
	}
	out := make([]CachedModel, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	return out, nil
}

// ParseHFModelPath extracts (org, name, revision) from any of the cache
// path shapes the proxy persists today:
//
//   - /<org>/<name>/resolve/<rev>/<file...>           (public resolve URL)
//   - /datasets/<org>/<name>/resolve/<rev>/<file...>  (datasets/spaces)
//   - /spaces/<org>/<name>/resolve/<rev>/<file...>
//   - /api/resolve-cache/(models|datasets|spaces)/<org>/<name>/<rev>/<file...>
//     (internal hub resolve-cache; how huggingface_hub fetches blobs)
func ParseHFModelPath(p string) (org, name, rev string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) >= 3 && parts[0] == "api" && parts[1] == "resolve-cache" {
		parts = parts[2:]
		if len(parts) > 0 && (parts[0] == "models" || parts[0] == "datasets" || parts[0] == "spaces") {
			parts = parts[1:]
		}
		if len(parts) < 4 {
			return "", "", "", false
		}
		return parts[0], parts[1], parts[2], true
	}
	if len(parts) > 0 && (parts[0] == "datasets" || parts[0] == "spaces") {
		parts = parts[1:]
	}
	if len(parts) < 5 || parts[2] != "resolve" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[3], true
}

// groupKey identifies a logical model in the listing.  Cache entries
// for the same model may legitimately come from multiple upstreams
// (huggingface.co for resolve-cache metadata + cas-bridge.xethub.hf.co
// for Xet bodies that carry the model identity in OriginPath), so the
// upstream host is NOT part of the key -- the model is owned by its
// (org, name) regardless of where individual files were fetched from.
type groupKey struct {
	org, name string
}

// ListGroupedFromCache scans the cache and returns model-first aggregates.
func ListGroupedFromCache(cacheDir string, maxEntries int, includeFiles bool) (GroupedListing, error) {
	if maxEntries <= 0 || maxEntries > 5000 {
		maxEntries = 500
	}
	root := filepath.Join(cacheDir, "v1", "objects")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return GroupedListing{}, nil
		}
		return GroupedListing{}, err
	}

	groups := make(map[groupKey]*ModelGroup)
	revSets := make(map[groupKey]map[string]struct{})
	// Dedupe (group, rev, file-basename) so the same artifact cached
	// under multiple internal path shapes (eg /Org/Name/resolve/main/x
	// + /api/resolve-cache/models/Org/Name/<sha>/x) only counts once
	// against the model's total size and file count.
	seenFiles := make(map[string]struct{})
	var grandTotal int64
	scanned := 0

	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		metaPath := filepath.Join(root, ent.Name(), "meta.json")
		b, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var m metaFile
		if err := json.Unmarshal(b, &m); err != nil || m.Path == "" {
			continue
		}
		scanned++
		if scanned > maxEntries {
			break
		}

		// Redirect stubs (302/307) hold no body bytes and would just
		// show up as confusing 0-byte rows. Skip them entirely; the
		// real artifact is cached under its resolved path.
		if m.StatusCode < 200 || m.StatusCode >= 300 {
			continue
		}

		attribution := m.attributionPath()
		org, name, rev, ok := ParseHFModelPath(attribution)
		if !ok {
			continue
		}

		var fileBytes int64
		if m.Total != nil {
			fileBytes = *m.Total
		}

		base := attribution
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		// Dedupe by (org/name/rev/file): the same artifact may be cached
		// under multiple path shapes (resolve URL vs internal resolve-
		// cache vs Xet body with origin_path) and we only want to count
		// the bytes once against the model.
		dedupeKey := org + "|" + name + "|" + rev + "|" + base
		if _, dup := seenFiles[dedupeKey]; dup {
			continue
		}
		seenFiles[dedupeKey] = struct{}{}

		grandTotal += fileBytes

		gk := groupKey{org: org, name: name}
		g, exists := groups[gk]
		if !exists {
			g = &ModelGroup{
				Org:  org,
				Name: name,
				// Upstream is left as the first host we see for this
				// model; the field is increasingly a presentation hint
				// rather than an identity component since a single
				// model can span multiple upstream hosts (HF for JSON
				// metadata + cas-bridge for Xet bodies).
				Upstream: m.UpstreamHost,
			}
			groups[gk] = g
			revSets[gk] = make(map[string]struct{})
		}
		g.FileCount++
		g.TotalBytes += fileBytes
		revSets[gk][rev] = struct{}{}
		if includeFiles {
			g.Files = append(g.Files, CachedModel{
				Path:         base,
				UpstreamHost: m.UpstreamHost,
				StatusCode:   m.StatusCode,
				TotalBytes:   m.Total,
			})
		}
	}

	items := make([]ModelGroup, 0, len(groups))
	for gk, g := range groups {
		revisions := make([]string, 0, len(revSets[gk]))
		for rev := range revSets[gk] {
			revisions = append(revisions, rev)
		}
		sort.Strings(revisions)
		g.Revisions = revisions
		if includeFiles {
			sort.Slice(g.Files, func(i, j int) bool {
				return g.Files[i].Path < g.Files[j].Path
			})
		}
		items = append(items, *g)
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].TotalBytes != items[j].TotalBytes {
			return items[i].TotalBytes > items[j].TotalBytes
		}
		if items[i].Org != items[j].Org {
			return items[i].Org < items[j].Org
		}
		return items[i].Name < items[j].Name
	})

	return GroupedListing{
		Items:           items,
		GrandTotalBytes: grandTotal,
	}, nil
}
