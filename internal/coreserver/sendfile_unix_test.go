// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux || darwin

package coreserver

import "testing"

func TestMaxSendfileSlice(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total int64
		rem   int64
		want  int64
	}{
		// Tiny bodies fit in a single sendfile call: cap == total.
		{name: "tiny-16KiB", total: 16 << 10, rem: 16 << 10, want: 16 << 10},
		{name: "small-256KiB", total: 256 << 10, rem: 256 << 10, want: 256 << 10},
		{name: "edge-4MiB-singleshot", total: 4 << 20, rem: 4 << 20, want: 4 << 20},

		// Mid-tier (HF chunk size band) caps at 16 MiB per syscall.
		{name: "mid-10MiB", total: 10 << 20, rem: 10 << 20, want: 10 << 20},
		{name: "mid-16MiB", total: 16 << 20, rem: 16 << 20, want: 16 << 20},
		{name: "mid-32MiB", total: 32 << 20, rem: 32 << 20, want: midSendfileChunk},
		{name: "mid-32MiB-tail", total: 32 << 20, rem: 4 << 20, want: 4 << 20},

		// Elephant bodies (> 32 MiB) use 32 MiB slices.
		{name: "elephant-64MiB", total: 64 << 20, rem: 64 << 20, want: elephantSendfileChunk},
		{name: "elephant-tail", total: 64 << 20, rem: 2 << 20, want: 2 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := maxSendfileSlice(tc.total, tc.rem); got != tc.want {
				t.Errorf("total=%d rem=%d: got %d want %d", tc.total, tc.rem, got, tc.want)
			}
		})
	}
}
