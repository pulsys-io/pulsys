// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package coreserver

import "unsafe"

// bytesToStringUnsafe aliases b as a string with no copy and no allocation.
//
// # SAFETY CONTRACT
//
// The returned string shares b's storage.  Callers MUST guarantee that:
//
//  1. b is not mutated for as long as the string is reachable, AND
//  2. the string does not escape past the lifetime of b.
//
// In coreserver this helper is only used to feed []byte slices that point
// into the per-request scratch buffer (Header / Path / Query / Auth) into
// downstream functions that accept strings (cache.KeyHex, cache.ParseSingleRange,
// classify.ArtifactGET).  All such call sites consume their argument
// synchronously inside readRequest's caller and never retain it past the
// next request on the same connection — which is exactly what scratch's
// lifetime allows.
func bytesToStringUnsafe(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}
