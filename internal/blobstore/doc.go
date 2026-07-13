// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package blobstore is the content-addressed object store backing
// the Pulsys self-hosted model registry. Every blob is keyed by the
// hex sha256 of its bytes (the "oid"), matching the LFS pointer
// addressing huggingface_hub already uses.
//
// Design constraints:
//
//   - Writes MUST be atomic. A partial blob must never become
//     visible under its final oid path - readers either see the
//     complete bytes or no file at all. The local-disk implementation
//     achieves this with a temp-file + rename(2) pattern.
//
//   - Writes MUST verify the declared oid against the bytes received,
//     so a mis-uploaded LFS object can't shadow the correct blob.
//     Implementations return ErrOIDMismatch on detected divergence.
//
//   - Reads MUST be Range-friendly. The HTTP read handlers in
//     internal/proxy serve hf_transfer's parallel range fetches and
//     huggingface_hub's HEAD probes; ReaderAt and Seeker are both
//     part of the contract.
//
//   - Sizes can be multi-GB; nothing buffers the full body in memory.
//     Put streams from io.Reader; Open returns an os.File-shaped
//     handle that supports Range without slurp.
//
// Backends:
//
//   - LocalStore (v1): files under {root}/blobs/<aa>/<oid> on local
//     disk. Single-machine deployments and the test harness.
//
//   - S3Store (deferred): same interface against any S3-compatible
//     bucket. Selected by config flag in a later phase.
package blobstore
