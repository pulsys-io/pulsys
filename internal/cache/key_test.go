// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import "testing"

func TestMergeSpans(t *testing.T) {
	in := []Span{{0, 10}, {5, 15}, {20, 30}}
	got := MergeSpans(in)
	if len(got) != 2 || got[0].Start != 0 || got[0].End != 15 || got[1].Start != 20 || got[1].End != 30 {
		t.Fatalf("%+v", got)
	}
}

func TestCovers(t *testing.T) {
	sp := []Span{{0, 100}, {200, 300}}
	if !Covers(sp, 0, 50) {
		t.Fatal()
	}
	if Covers(sp, 0, 150) {
		t.Fatal("gap should not cover")
	}
	if !Covers(sp, 200, 250) {
		t.Fatal()
	}
}
