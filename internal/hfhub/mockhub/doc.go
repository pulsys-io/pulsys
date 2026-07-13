// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package mockhub is an in-process, spec-compliant Hugging Face Hub
// mock for tests. It implements the subset of the HF HTTP API that
// pulsys and huggingface_hub clients interact with, including the
// full upload lifecycle (preupload, commit, LFS batch, LFS object
// PUT, LFS verify) so the proxy's read AND write paths can be
// exercised without touching huggingface.co.
//
// Design notes:
//
//   - All state lives in memory. The mock is meant to be cheap to
//     spin up per test (typical Setup() is <2ms). Tests get a fresh
//     Server via mockhub.New(t).
//
//   - The mock exposes the same HTTP shape as huggingface.co for the
//     endpoints listed below, including the headers huggingface_hub
//     parses (`X-Repo-Commit`, `X-Linked-Etag`, `X-Linked-Size`,
//     `Accept-Ranges`).
//
//   - The mock honors `Range:` so hf_transfer's parallel ranged GETs
//     can be exercised end-to-end through the proxy.
//
//   - Auth: by default the mock accepts ANY bearer token. To exercise
//     read-only / write / expired token paths, configure tokens via
//     Config.Tokens.
//
//   - LFS storage uses a "mock S3" handler mounted on the same server
//     (path `/lfs-storage/{oid}`). The LFS batch endpoint returns
//     URLs pointing at this path so the proxy's S3 redirect logic is
//     still exercised (a full cdn-lfs.* host name can be enabled via
//     Config.LFSHost when the proxy's allowed-host list needs to be
//     hit).
//
// Endpoints implemented:
//
//	GET  /api/models/{repo}                          model info
//	GET  /api/models/{repo}/tree/{rev}               flat file listing
//	POST /api/models/{repo}/paths-info/{rev}         paths-info batch
//	HEAD /{repo}/resolve/{rev}/{path}                metadata probe
//	GET  /{repo}/resolve/{rev}/{path}                download (+ Range)
//	POST /api/models/{repo}/preupload/{rev}          preupload dedup
//	POST /api/models/{repo}/commit/{rev}             atomic commit (NDJSON)
//	POST /{repo}.git/info/lfs/objects/batch          LFS batch
//	PUT  /lfs-storage/{oid}                          LFS object upload (mock S3)
//	POST /{repo}.git/info/lfs/verify                 LFS verify
//
// Tests should treat the mock as a deterministic surface. Race
// detection is enabled in CI; the mock's internal state is guarded
// by a single mutex.
package mockhub
