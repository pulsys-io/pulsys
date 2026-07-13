// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"io"
	"os"
)

// HotBackend is the warm-path storage contract.
//
// "Hot" here means the tier the proxy *serves clients from*.  By
// construction the proxy always serves bytes from a local
// `*os.File` so that the warm path can hand the file descriptor
// to `sendfile(2)` on Darwin / Linux and to `io_uring` SQEs on
// Linux without an intermediate userspace copy.  That requirement
// is encoded in the signature of OpenBody, which returns an
// `*os.File` rather than an `io.ReadCloser`: any backend that
// cannot satisfy that signature is not a hot backend.
//
// Cold backends (S3, NFS-via-presign, etc.) plug in below the
// tier as a separate ColdBackend interface (P11), and their reads
// are always staged into the hot tier before the proxy serves
// them.  This preserves the measured warm-cache behavior
// (sendfile/io_uring path, 0 allocs/op) for any topology.
//
// The local-FS *Store is the only HotBackend implementation today
// and the compile-time assertion at the bottom of this file
// guarantees it remains compatible as the interface evolves.
//
// Methods listed here mirror the subset of *Store consumed by
// internal/proxy/handler.go, internal/coreserver/server.go, and
// cmd/pulsys/main.go.  Helpers that are package-level
// (IsCacheableRedirectStatus, ParseSingleRange, KeyHex, …) are
// deliberately out of scope: they are pure functions and do not
// depend on a backend identity.
type HotBackend interface {
	// LoadMeta returns the cached *Meta for key, or (nil, nil)
	// when the key is absent.  The returned *Meta MUST be treated
	// as read-only: callers requiring mutation must clone it.
	LoadMeta(key string) (*Meta, error)

	// OpenBody returns the body file for reading.  The caller owns
	// the returned descriptor and must Close it.  The returned
	// type is *os.File so that sendfile/io_uring can be wired
	// directly against the fd; do not relax this signature.
	OpenBody(key string) (*os.File, error)

	// BeginSegment opens a writer at SegmentParams.Start in the
	// body file.  Closing the writer publishes a meta.json that
	// reflects the bytes actually written.
	BeginSegment(key string, p SegmentParams) (*SegmentWriter, error)

	// StoreRedirect persists a cached 30x response keyed by key.
	// No body file is created; meta.json records the status code
	// and the upstream (pre-rewrite) Location.
	StoreRedirect(
		key string,
		status int,
		host, path, rawQuery, location, contentType, etag string,
		extraHeaders map[string]string,
	) error

	// StoreAlias writes a meta-only cache entry at aliasKey
	// pointing at canonicalKey.  Used to share a body across two
	// equivalent cache keys (e.g. /resolve/main/ and
	// /resolve/<sha>/).
	StoreAlias(aliasKey, canonicalKey, host, path, rawQuery string) error

	// Lock acquires a per-key mutex for singleflight-style
	// serialization on concurrent misses for the same cache key.
	// The returned function MUST be called once to release the
	// lock (typically via defer).
	Lock(key string) func()
}

// Compile-time assertion that the canonical hot backend (the
// local-FS *Store) satisfies HotBackend.  Any drift between the
// interface and the implementation surfaces here before tests run.
var _ HotBackend = (*Store)(nil)

// ColdReader is the read half of a cold backend.  It returns an
// `io.ReadCloser` rather than `*os.File` because S3, NFS-presigned
// readers, and similar cannot satisfy the *os.File contract.
// A ColdBackend implementation will compose this with put / delete
// in P11; the type lives here as a stake in the ground so that
// callers and reviewers know cold-tier reads CANNOT be served
// directly to clients — they must be staged into a HotBackend
// first.
type ColdReader interface {
	// HasObject reports whether the cold tier has an object for
	// the given key.  Implementations must be cheap (HEAD, not
	// GET).
	HasObject(key string) (bool, error)

	// GetObject streams the cold-tier body for key.  The caller
	// is responsible for Close.  Implementations should expose
	// any Content-Length they know about via the returned size;
	// -1 means unknown.
	GetObject(key string) (rc io.ReadCloser, size int64, err error)
}
