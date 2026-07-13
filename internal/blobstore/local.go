// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LocalStore is a single-machine content-addressed store on the local
// filesystem. Layout under root:
//
//	{root}/blobs/<aa>/<aabbcc...>
//	{root}/tmp/<random>
//
// The two-character fan-out keeps any one directory under ~256 / 4096
// entries even when the repo population is tens of millions of files.
// rename(2) within the same filesystem is atomic on all POSIX
// platforms, so a crash mid-stream can leave a temp file but never a
// partial blob under the canonical path.
type LocalStore struct {
	root    string
	blobDir string
	tmpDir  string
}

// NewLocal creates the on-disk layout under root and returns a Store.
// root may pre-exist; missing directories are created with 0o755.
func NewLocal(root string) (*LocalStore, error) {
	if root == "" {
		return nil, errors.New("blobstore: empty root")
	}
	blobDir := filepath.Join(root, "blobs")
	tmpDir := filepath.Join(root, "tmp")
	for _, d := range []string{blobDir, tmpDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("blobstore: mkdir %s: %w", d, err)
		}
	}
	return &LocalStore{root: root, blobDir: blobDir, tmpDir: tmpDir}, nil
}

// path returns the canonical on-disk path for oid. Caller must ensure
// oid is a valid 64-char hex string (Put / Open both validate).
func (s *LocalStore) path(oid string) string {
	return filepath.Join(s.blobDir, oid[:2], oid)
}

// storageURL returns the opaque URL persisted in the blobs table.
func (s *LocalStore) storageURL(oid string) string {
	return "file://" + s.path(oid)
}

// Put streams body to a temp file, hashing as it goes, then renames
// into place atomically. On any error the temp file is removed.
func (s *LocalStore) Put(ctx context.Context, body io.Reader, opts PutOptions) (Stat, error) {
	if opts.ExpectedOID != "" && !validOID(opts.ExpectedOID) {
		return Stat{}, fmt.Errorf("blobstore: invalid oid %q: %w", opts.ExpectedOID, ErrOIDMismatch)
	}

	tmpFile, err := os.CreateTemp(s.tmpDir, "blob-*.part")
	if err != nil {
		return Stat{}, fmt.Errorf("blobstore: create temp: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	committed := false
	defer func() {
		if !committed {
			cleanup()
		}
	}()

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmpFile, h), ctxReader{ctx: ctx, r: body})
	if err != nil {
		_ = tmpFile.Close()
		return Stat{}, fmt.Errorf("blobstore: copy: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return Stat{}, fmt.Errorf("blobstore: close temp: %w", err)
	}

	if opts.ExpectedSize > 0 && written != opts.ExpectedSize {
		return Stat{}, fmt.Errorf("blobstore: declared %d got %d: %w",
			opts.ExpectedSize, written, ErrSizeMismatch)
	}

	oid := hex.EncodeToString(h.Sum(nil))
	if opts.ExpectedOID != "" && oid != opts.ExpectedOID {
		return Stat{}, fmt.Errorf("blobstore: declared %s got %s: %w",
			opts.ExpectedOID, oid, ErrOIDMismatch)
	}

	finalPath := s.path(oid)
	if _, err := os.Stat(finalPath); err == nil {
		// Already present. Discard our temp file (`committed` stays
		// false so the deferred cleanup removes it) and return the
		// pre-existing canonical descriptor. This is the dedup hot
		// path: two concurrent uploads of the same bytes resolve
		// here without stomping each other.
		return Stat{OID: oid, Size: written, StorageURL: s.storageURL(oid)}, nil
	}

	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return Stat{}, fmt.Errorf("blobstore: mkdir fanout: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		// Another concurrent Put may have populated the canonical path
		// between our Stat and Rename. rename(2) is atomic; if it
		// failed because the destination already exists on a filesystem
		// that disallows overwrite-rename we accept the existing blob.
		if _, statErr := os.Stat(finalPath); statErr == nil {
			return Stat{OID: oid, Size: written, StorageURL: s.storageURL(oid)}, nil
		}
		return Stat{}, fmt.Errorf("blobstore: rename: %w", err)
	}
	committed = true
	return Stat{OID: oid, Size: written, StorageURL: s.storageURL(oid)}, nil
}

// Open returns a *os.File-backed handle on the blob. The file
// satisfies io.ReadSeekCloser + io.ReaderAt so handlers can do
// ranged reads without extra wrapping.
func (s *LocalStore) Open(ctx context.Context, oid string) (ReadSeekCloser, Stat, error) {
	if !validOID(oid) {
		return nil, Stat{}, ErrNotFound
	}
	f, err := os.Open(s.path(oid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Stat{}, ErrNotFound
		}
		return nil, Stat{}, fmt.Errorf("blobstore: open: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Stat{}, fmt.Errorf("blobstore: stat: %w", err)
	}
	return f, Stat{OID: oid, Size: fi.Size(), StorageURL: s.storageURL(oid)}, nil
}

// Stat returns the on-disk descriptor without opening the file.
func (s *LocalStore) Stat(ctx context.Context, oid string) (Stat, error) {
	if !validOID(oid) {
		return Stat{}, ErrNotFound
	}
	fi, err := os.Stat(s.path(oid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Stat{}, ErrNotFound
		}
		return Stat{}, err
	}
	return Stat{OID: oid, Size: fi.Size(), StorageURL: s.storageURL(oid)}, nil
}

// Delete removes the blob bytes. Missing files are NOT an error so
// retried GCs are safe.
func (s *LocalStore) Delete(ctx context.Context, oid string) error {
	if !validOID(oid) {
		return nil
	}
	err := os.Remove(s.path(oid))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// validOID checks that s is a 64-character lowercase hex string.
// Hashes coming back from sha256.Sum already satisfy this; we still
// validate caller-supplied oids defensively to avoid path traversal.
func validOID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// ctxReader makes io.Copy honor ctx cancellation between chunks.
// Without it, a stalled multi-GB upload would block until the body
// reader itself unblocks (which can be tied to the network).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
