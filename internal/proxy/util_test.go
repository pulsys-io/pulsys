// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy

import "testing"

func TestParseContentRangeResponse(t *testing.T) {
	s, e, tot, ok := parseContentRangeResponse("bytes 0-9/100")
	if !ok || s != 0 || e != 10 || tot != 100 {
		t.Fatalf("got %d %d %d %v", s, e, tot, ok)
	}
	s, e, tot, ok = parseContentRangeResponse("bytes 100-199/*")
	if !ok || s != 100 || e != 200 || tot != -1 {
		t.Fatalf("got %d %d %d %v", s, e, tot, ok)
	}
}
