// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package logx

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSanitizeValueStripsLineBreaks is the property that matters: a
// request-derived string must not be able to introduce a line break, or
// a crafted path can forge log records that look like they came from the
// proxy itself.
func TestSanitizeValueStripsLineBreaks(t *testing.T) {
	forged := "/model.safetensors\n{\"level\":\"INFO\",\"msg\":\"cache warm\"}"
	got := SanitizeValue(forged)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("SanitizeValue kept a line break: %q", got)
	}
	if !strings.Contains(got, "/model.safetensors") {
		t.Errorf("SanitizeValue dropped legitimate content: %q", got)
	}
}

func TestSanitizeValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain path unchanged", "/org/model/resolve/main/f.bin", "/org/model/resolve/main/f.bin"},
		{"carriage return removed", "a\rb", "ab"},
		{"crlf removed", "a\r\nb", "ab"},
		{"tab and null removed", "a\tb\x00c", "abc"},
		{"del removed", "a\x7fb", "ab"},
		{"empty stays empty", "", ""},
		{"utf8 preserved", "/模型/文件.bin", "/模型/文件.bin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeValue(tc.in); got != tc.want {
				t.Fatalf("SanitizeValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeValueTruncatesOnRuneBoundary guards the cap: an attacker
// sizes the input, and a naive byte slice would split a multi-byte rune
// and emit invalid UTF-8 into the log stream.
func TestSanitizeValueTruncatesOnRuneBoundary(t *testing.T) {
	got := SanitizeValue(strings.Repeat("模", 400))
	if !utf8.ValidString(got) {
		t.Fatalf("SanitizeValue produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("oversized value was not marked truncated: %q", got)
	}
	if len(got) > maxLogValueLen+len("...(truncated)") {
		t.Errorf("truncated value is %d bytes, want <= %d", len(got), maxLogValueLen+len("...(truncated)"))
	}
}
