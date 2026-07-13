// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package businesslogic

import (
	"errors"
	"strings"
	"testing"

	"github.com/pulsys-io/pulsys/internal/registry"
)

// TestCommitTx_LFSPointerSize_Honest pins the happy path:
// when the declared size matches the stored blob size, the
// commit succeeds.  Without this baseline the failure tests
// below could pass for the wrong reason (e.g. "all LFS
// commits are rejected").
func TestCommitTx_LFSPointerSize_Honest(t *testing.T) {
	rh := newRegistryHarness(t)

	in := registry.CommitInput{
		RepoID:  rh.repoID,
		Branch:  "main",
		Summary: "honest lfs commit",
		LFSPointers: map[string]registry.LFSCommitFile{
			"weights/model.bin": {BlobOID: rh.blobOID, Size: rh.blobSize},
		},
	}
	if _, err := rh.store.CommitTx(rh.ctx, in); err != nil {
		t.Fatalf("honest commit: %v", err)
	}
}

// TestCommitTx_LFSPointerSize_LiesSmaller pins the central
// invariant: an attacker who declares size=10 for a blob of
// true size N>10 MUST be rejected with ErrLFSSizeMismatch.
// Pre-Phase-5 the lie would have been stored and surfaced in
// downstream LFS batch responses and file listings as size=10,
// while the served bytes were still N.
func TestCommitTx_LFSPointerSize_LiesSmaller(t *testing.T) {
	rh := newRegistryHarness(t)

	in := registry.CommitInput{
		RepoID:  rh.repoID,
		Branch:  "main",
		Summary: "lying lfs commit (smaller)",
		LFSPointers: map[string]registry.LFSCommitFile{
			"weights/model.bin": {BlobOID: rh.blobOID, Size: 1},
		},
	}
	_, err := rh.store.CommitTx(rh.ctx, in)
	if !errors.Is(err, registry.ErrLFSSizeMismatch) {
		t.Fatalf("err=%v want ErrLFSSizeMismatch", err)
	}
	if !strings.Contains(err.Error(), "declared=1") || !strings.Contains(err.Error(), "stored=") {
		t.Fatalf("err=%q want diagnostic with declared/stored sizes", err.Error())
	}
}

// TestCommitTx_LFSPointerSize_LiesLarger is the symmetric
// case: a client that under-declares vs over-declares are
// both lies.  Tested separately because a future bug might
// only check ">=" or "<=".
func TestCommitTx_LFSPointerSize_LiesLarger(t *testing.T) {
	rh := newRegistryHarness(t)

	in := registry.CommitInput{
		RepoID:  rh.repoID,
		Branch:  "main",
		Summary: "lying lfs commit (larger)",
		LFSPointers: map[string]registry.LFSCommitFile{
			"weights/model.bin": {BlobOID: rh.blobOID, Size: rh.blobSize + 1},
		},
	}
	_, err := rh.store.CommitTx(rh.ctx, in)
	if !errors.Is(err, registry.ErrLFSSizeMismatch) {
		t.Fatalf("err=%v want ErrLFSSizeMismatch", err)
	}
}

// TestCommitTx_LFSPointerSize_ZeroIsTolerated documents the
// soft contract: a declared size of 0 is treated as "unknown"
// and the cross-check is skipped.  This is the behavior the
// HF NDJSON pre-Phase-5 already produced for clients that
// omitted the size field; we don't want a defensive ZERO
// rejection to break older clients.  If we ever want to
// REQUIRE a positive size, that change moves through a
// separate Plan: trailer.
func TestCommitTx_LFSPointerSize_ZeroIsTolerated(t *testing.T) {
	rh := newRegistryHarness(t)

	in := registry.CommitInput{
		RepoID:  rh.repoID,
		Branch:  "main",
		Summary: "size omitted (zero)",
		LFSPointers: map[string]registry.LFSCommitFile{
			"weights/model.bin": {BlobOID: rh.blobOID, Size: 0},
		},
	}
	if _, err := rh.store.CommitTx(rh.ctx, in); err != nil {
		t.Fatalf("size=0 should be tolerated: err=%v", err)
	}
}

// TestCommitTx_Inline_SizeNotCrossChecked confirms the inverse
// for inline files: their Size is derived from the in-line
// bytes, not client-asserted, so no cross-check applies.  An
// inline entry with the seeded blob's oid + a wrong size goes
// through cleanly.  This guards against a refactor that
// accidentally widens the LFS check to inline files.
func TestCommitTx_Inline_SizeNotCrossChecked(t *testing.T) {
	rh := newRegistryHarness(t)

	in := registry.CommitInput{
		RepoID:  rh.repoID,
		Branch:  "main",
		Summary: "inline lies, but no cross-check",
		Inline: map[string]registry.InlineCommitFile{
			"README.md": {BlobOID: rh.blobOID, Size: 999_999},
		},
	}
	if _, err := rh.store.CommitTx(rh.ctx, in); err != nil {
		t.Fatalf("inline commit with wrong size: %v (LFS check shouldn't apply)", err)
	}
}
