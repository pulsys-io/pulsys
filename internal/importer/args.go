// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package importer

const KindHFCacheImport = "hf_cache_import"

// CacheImportArgs is the River job payload for warming the local HF proxy cache.
type CacheImportArgs struct {
	RepoID   string `json:"repo_id"`
	Revision string `json:"revision"`
	RepoType string `json:"repo_type"`
}

func (CacheImportArgs) Kind() string { return KindHFCacheImport }
