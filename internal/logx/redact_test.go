// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package logx

import (
	"strings"
	"testing"
)

func TestScrubURLString(t *testing.T) {
	got := ScrubURLString("https://huggingface.co/api/models?token=secret&foo=bar")
	if !strings.Contains(got, "REDACTED") || !strings.Contains(got, "foo=bar") {
		t.Fatalf("unexpected scrub: %q", got)
	}
}
