// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package importmsg renders import-job error strings as concise,
// user-facing messages.
//
// Style follows Apple HIG, GitHub Actions, Vercel, and Stripe Dashboard
// alert patterns:
//
//   - Title states what happened in one short phrase, sentence case, no
//     trailing period. ("Import timed out", not "Import timed out before
//     finishing because the deadline elapsed.")
//   - Hint, when present, gives the operator one concrete next step in
//     plain English. No internal identifiers (env var names, flag names),
//     no conditional lectures ("if it keeps failing, do X"); those belong
//     in docs and in the raw technical details that the UI already shows
//     in a collapsible disclosure.
//   - Canceled is not an error and intentionally returns an empty Message;
//     the status badge alone communicates it.
package importmsg

import (
	"strings"
)

// Message is the rendered, user-facing form of an import error.
//
// Title is mandatory for any failure surface. Hint is optional; when empty,
// the UI shows only the title (and the technical details disclosure).
type Message struct {
	Title string
	Hint  string
}

// IsZero reports whether the message has no user-facing content. Used by
// callers (e.g. the admin store) to suppress the error block for jobs that
// were intentionally canceled.
func (m Message) IsZero() bool {
	return m.Title == "" && m.Hint == ""
}

// HumanizeMessage maps a raw error string to a structured Message.
//
// Order matters: more-specific patterns (quota, cancel, timeout) are
// matched before generic HTTP status patterns so that, for example, a
// quota error that includes a 507 status code resolves to the quota copy.
func HumanizeMessage(raw string) Message {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Message{}
	}
	lower := strings.ToLower(raw)

	switch {
	case strings.Contains(lower, "cache storage quota exceeded"),
		strings.Contains(lower, "507"),
		strings.Contains(lower, "insufficient storage"):
		return Message{
			Title: "Cache is full",
			Hint:  "Free space on the Models page, then try again.",
		}

	case strings.Contains(lower, "context canceled"),
		strings.Contains(lower, "job canceled remotely"),
		strings.Contains(lower, "jobcancelerror"):
		// Canceled is not a failure surface.
		return Message{}

	case strings.Contains(lower, "context deadline exceeded"):
		return Message{
			Title: "Import timed out",
			Hint:  "The job ran longer than the configured time limit.",
		}

	case strings.Contains(lower, "401"),
		strings.Contains(lower, "403"),
		strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "forbidden"):
		return Message{
			Title: "Hugging Face rejected the download token",
			Hint:  "The proxy's read token is missing access to this repository.",
		}

	case strings.Contains(lower, "404"),
		strings.Contains(lower, "not found"):
		return Message{
			Title: "Repository not found",
			Hint:  "Check the repo ID and revision, then try again.",
		}

	case strings.Contains(lower, "429"),
		strings.Contains(lower, "rate limit"):
		return Message{
			Title: "Rate-limited by Hugging Face",
			Hint:  "Wait a few minutes, then retry.",
		}

	case strings.Contains(lower, "gave up after") && strings.Contains(lower, "retries"):
		return Message{
			Title: "Download failed",
			Hint:  "Hugging Face returned errors after several retries.",
		}

	case strings.Contains(lower, "502"),
		strings.Contains(lower, "503"),
		strings.Contains(lower, "504"),
		strings.Contains(lower, "bad gateway"),
		strings.Contains(lower, "service unavailable"),
		strings.Contains(lower, "gateway timeout"):
		return Message{
			Title: "Hugging Face is unavailable",
			Hint:  "The upstream returned a temporary error. Try again shortly.",
		}

	case strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "no such host"),
		strings.Contains(lower, "network is unreachable"),
		strings.Contains(lower, "i/o timeout"):
		return Message{
			Title: "Network error",
			Hint:  "The proxy could not reach Hugging Face.",
		}
	}

	return Message{
		Title: "Import failed",
		Hint:  "Open technical details below for the underlying error.",
	}
}

// Humanize returns just the Title of HumanizeMessage. Retained so existing
// callers that want a single string keep working; new code should prefer
// HumanizeMessage so the UI can render a title and hint independently.
func Humanize(raw string) string {
	return HumanizeMessage(raw).Title
}
