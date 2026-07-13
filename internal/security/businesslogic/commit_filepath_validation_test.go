// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package businesslogic

import (
	"errors"
	"strings"
	"testing"

	"github.com/pulsys-io/pulsys/internal/registry"
)

// TestValidateCommitFilePath_RejectsAttackShapes pins the
// Phase 5 contract on path strings persisted in
// file_revisions.path.  Each row is an attack shape the recon
// flagged: paths that would survive in the DB column but cause
// downstream confusion (archive exporters, log consumers,
// admin UI listings).
func TestValidateCommitFilePath_RejectsAttackShapes(t *testing.T) {
	cases := []struct {
		desc, path string
	}{
		{"empty", ""},
		{"leading slash", "/etc/passwd"},
		{"parent dotdot segment", "../etc/passwd"},
		{"parent dotdot mid-path", "weights/../../../etc/passwd"},
		{"parent dotdot trailing", "weights/.."},
		{"NUL byte", "weights\x00bin"},
		{"backslash", "weights\\bin"},
		{"CR injection", "weights\rINJECTED: 1"},
		{"LF injection", "weights\nINJECTED: 1"},
		{"oversize", strings.Repeat("a", registry.MaxCommitFilePathBytes+1)},
	}
	for _, c := range cases {
		c := c
		t.Run(c.desc, func(t *testing.T) {
			err := registry.ValidateCommitFilePath(c.path)
			if !errors.Is(err, registry.ErrCommitPathInvalid) {
				t.Fatalf("path=%q err=%v want ErrCommitPathInvalid", c.path, err)
			}
		})
	}
}

// TestValidateCommitFilePath_AcceptsLegitimateShapes pins the
// negative: every path shape we WANT to accept does pass.  An
// over-strict validator would break legitimate HF layouts.
func TestValidateCommitFilePath_AcceptsLegitimateShapes(t *testing.T) {
	good := []string{
		"README.md",
		"config.json",
		"model.safetensors",
		"weights/00001-of-00010.bin",
		"tokenizer/special_tokens_map.json",
		"my.cool.model..weights.bin",   // dots inside a name (NOT a ".." segment)
		"path/with-dashes_and_under.x", // hyphens, underscores
		"deep/nested/path/inside/this/repo/file.txt",
		strings.Repeat("a", registry.MaxCommitFilePathBytes), // exactly the cap
		"a",                       // single char
		"chinese-中文-filename.bin", // UTF-8 (no NUL, no CR/LF, no backslash)
	}
	for _, p := range good {
		p := p
		t.Run(p, func(t *testing.T) {
			if err := registry.ValidateCommitFilePath(p); err != nil {
				t.Fatalf("path=%q err=%v want nil", p, err)
			}
		})
	}
}

// TestCommitTx_RejectsInvalidPathInInline pins the wiring:
// ValidateCommitFilePath is actually called from CommitTx for
// every path in Inline/LFSPointers/Deletes, not just unit-
// tested in isolation.  This guards against a future refactor
// that drops the call.
func TestCommitTx_RejectsInvalidPathInInline(t *testing.T) {
	rh := newRegistryHarness(t)

	in := registry.CommitInput{
		RepoID:  rh.repoID,
		Branch:  "main",
		Summary: "test",
		Inline: map[string]registry.InlineCommitFile{
			"../etc/passwd": {BlobOID: rh.blobOID, Size: rh.blobSize},
		},
	}
	_, err := rh.store.CommitTx(rh.ctx, in)
	if !errors.Is(err, registry.ErrCommitPathInvalid) {
		t.Fatalf("err=%v want ErrCommitPathInvalid", err)
	}
}

// TestCommitTx_RejectsInvalidPathInLFSPointers covers the LFS
// pointer arm of the same wiring -- a future bug that
// validated Inline but not LFSPointers would slip past the
// previous test.
func TestCommitTx_RejectsInvalidPathInLFSPointers(t *testing.T) {
	rh := newRegistryHarness(t)

	in := registry.CommitInput{
		RepoID:  rh.repoID,
		Branch:  "main",
		Summary: "test",
		LFSPointers: map[string]registry.LFSCommitFile{
			"weights\x00.bin": {BlobOID: rh.blobOID, Size: rh.blobSize},
		},
	}
	_, err := rh.store.CommitTx(rh.ctx, in)
	if !errors.Is(err, registry.ErrCommitPathInvalid) {
		t.Fatalf("err=%v want ErrCommitPathInvalid", err)
	}
}

// TestCommitTx_RejectsInvalidPathInDeletes covers the Deletes
// arm so a delete-by-bogus-path can't be used to probe the
// store for path-parsing differential behavior.
func TestCommitTx_RejectsInvalidPathInDeletes(t *testing.T) {
	rh := newRegistryHarness(t)

	in := registry.CommitInput{
		RepoID:  rh.repoID,
		Branch:  "main",
		Summary: "test",
		Deletes: []string{"/absolute/path"},
	}
	_, err := rh.store.CommitTx(rh.ctx, in)
	if !errors.Is(err, registry.ErrCommitPathInvalid) {
		t.Fatalf("err=%v want ErrCommitPathInvalid", err)
	}
}
