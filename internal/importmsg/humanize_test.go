// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package importmsg

import "testing"

func TestHumanizeMessage(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		title string
		hint  string
	}{
		{
			name:  "timeout",
			in:    "context deadline exceeded",
			title: "Import timed out",
			hint:  "The job ran longer than the configured time limit.",
		},
		{
			name:  "canceled is not a failure",
			in:    "context canceled",
			title: "",
			hint:  "",
		},
		{
			name:  "river remote cancel",
			in:    "JobCancelError: job canceled remotely",
			title: "",
			hint:  "",
		},
		{
			name:  "quota",
			in:    "cache storage quota exceeded; purge unused models on /models or raise -cache-max-bytes",
			title: "Cache is full",
			hint:  "Free space on the Models page, then try again.",
		},
		{
			name:  "quota via 507",
			in:    "PUT /foo: 507 Insufficient Storage",
			title: "Cache is full",
			hint:  "Free space on the Models page, then try again.",
		},
		{
			name:  "auth",
			in:    "tree Qwen/Qwen2.5-0.5B: 401 Unauthorized: invalid token",
			title: "Hugging Face rejected the download token",
			hint:  "The proxy's read token is missing access to this repository.",
		},
		{
			name:  "not found",
			in:    "tree missing/model: 404 Not Found",
			title: "Repository not found",
			hint:  "Check the repo ID and revision, then try again.",
		},
		{
			name:  "rate limit",
			in:    "GET /resolve/main: 429 Too Many Requests",
			title: "Rate-limited by Hugging Face",
			hint:  "Wait a few minutes, then retry.",
		},
		{
			name:  "retries",
			in:    "tree org/model: gave up after 5 retries: 502 Bad Gateway",
			title: "Download failed",
			hint:  "Hugging Face returned errors after several retries.",
		},
		{
			name:  "upstream 503",
			in:    "GET /resolve/main: 503 Service Unavailable",
			title: "Hugging Face is unavailable",
			hint:  "The upstream returned a temporary error. Try again shortly.",
		},
		{
			name:  "network",
			in:    "GET http://127.0.0.1:8080/foo: dial tcp 127.0.0.1:8080: connect: connection refused",
			title: "Network error",
			hint:  "The proxy could not reach Hugging Face.",
		},
		{
			name:  "fallback",
			in:    "boom: some unrecognized failure mode",
			title: "Import failed",
			hint:  "Open technical details below for the underlying error.",
		},
		{
			name:  "empty",
			in:    "",
			title: "",
			hint:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := HumanizeMessage(tc.in)
			if got.Title != tc.title || got.Hint != tc.hint {
				t.Fatalf("HumanizeMessage(%q) = {%q, %q}, want {%q, %q}",
					tc.in, got.Title, got.Hint, tc.title, tc.hint)
			}
			if Humanize(tc.in) != tc.title {
				t.Fatalf("Humanize(%q) = %q, want %q", tc.in, Humanize(tc.in), tc.title)
			}
		})
	}
}

func TestMessageIsZero(t *testing.T) {
	if !(Message{}).IsZero() {
		t.Fatal("empty Message should be zero")
	}
	if (Message{Title: "x"}).IsZero() {
		t.Fatal("Message with title should not be zero")
	}
	if (Message{Hint: "x"}).IsZero() {
		t.Fatal("Message with hint should not be zero")
	}
}
