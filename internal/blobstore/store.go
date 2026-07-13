// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package blobstore

import (
	"context"
	"errors"
	"io"
)

// Errors returned by Store implementations.
var (
	// ErrNotFound is returned by Open / Stat when no blob exists for oid.
	ErrNotFound = errors.New("blobstore: blob not found")
	// ErrOIDMismatch is returned by Put when the actual sha256 of the
	// streamed bytes differs from the declared oid (or, when no oid
	// was declared, when the resulting size is zero).
	ErrOIDMismatch = errors.New("blobstore: oid mismatch")
	// ErrSizeMismatch is returned by Put when the declared size is set
	// and differs from the actual byte count read from the reader.
	ErrSizeMismatch = errors.New("blobstore: size mismatch")
)

// Stat is a lightweight blob descriptor.
type Stat struct {
	OID  string
	Size int64
	// StorageURL is the opaque URL the underlying backend uses to
	// locate the bytes. Persisted in the blobs table; never parsed by
	// higher layers.
	StorageURL string
}

// ReadSeekCloser is the interface returned by Open. The OS file
// returned by the local-disk backend already satisfies it.
type ReadSeekCloser interface {
	io.ReadCloser
	io.Seeker
	io.ReaderAt
}

// PutOptions controls Put behavior.
type PutOptions struct {
	// ExpectedOID is the sha256 hex the caller expects to compute over
	// the streamed bytes. If non-empty and the actual oid differs, Put
	// returns ErrOIDMismatch and discards the temp file.
	ExpectedOID string
	// ExpectedSize is the size the caller declared. If > 0 and the
	// actual byte count differs, Put returns ErrSizeMismatch.
	ExpectedSize int64
}

// Store is the content-addressed object store interface. Tests and
// the production handlers depend only on this contract.
type Store interface {
	// Put streams body into the backend, computing the sha256 along
	// the way. On success returns the canonical Stat with the bytes
	// committed under their oid; on hash / size mismatch the temp
	// file is removed and an error is returned.
	//
	// If an object with the same oid already exists, Put is a no-op
	// after the stream has been fully consumed and verified - the
	// returned Stat references the pre-existing blob. This makes
	// concurrent uploads of the same content idempotent.
	Put(ctx context.Context, body io.Reader, opts PutOptions) (Stat, error)

	// Open returns a Range-capable handle for oid, or ErrNotFound.
	Open(ctx context.Context, oid string) (ReadSeekCloser, Stat, error)

	// Stat returns the descriptor for oid without opening the body.
	Stat(ctx context.Context, oid string) (Stat, error)

	// Delete removes oid's bytes. Callers MUST verify refcount in the
	// database before invoking; the store does not track references.
	// Implementations make Delete idempotent (no error on missing).
	Delete(ctx context.Context, oid string) error
}
