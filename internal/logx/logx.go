// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package logx

import (
	"log/slog"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"
)

var sensitiveHeaderKeys = map[string]struct{}{
	"authorization":        {},
	"cookie":               {},
	"set-cookie":           {},
	"x-xet-access-token":   {},
	"x-amz-security-token": {},
	"proxy-authorization":  {},
}

var sensitiveQueryKeys = map[string]struct{}{
	"token": {},
}

// New returns a slog logger with redaction for secrets.
func New(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == "url" {
				if s, ok := a.Value.Any().(string); ok {
					a.Value = slog.StringValue(ScrubURLString(s))
				}
			}
			if len(groups) == 0 && strings.EqualFold(a.Key, "request_headers") {
				if m, ok := a.Value.Any().(map[string]string); ok {
					cp := make(map[string]string, len(m))
					for k, v := range m {
						if _, bad := sensitiveHeaderKeys[strings.ToLower(k)]; bad {
							cp[k] = "[REDACTED]"
						} else {
							cp[k] = v
						}
					}
					a.Value = slog.GroupValue(slog.Any("", cp))
				}
			}
			return a
		},
	})
	return slog.New(h)
}

// maxLogValueLen caps a sanitized value. Request-derived strings are
// attacker-sized; without a cap one request can bloat the log stream.
const maxLogValueLen = 512

// SanitizeValue makes a request-derived string safe to place in a log
// record. Carriage returns and newlines are removed so a crafted path or
// host cannot forge additional log lines, remaining control characters
// are dropped, and the result is truncated on a rune boundary.
//
// The JSON handler returned by New already escapes control characters,
// so this is defense in depth: it keeps the guarantee if the handler is
// ever swapped for a text one, and it is what makes the sanitisation
// visible to static analysis.
func SanitizeValue(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > maxLogValueLen {
		// Cut on a rune boundary so the record stays valid UTF-8.
		cut := maxLogValueLen
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "...(truncated)"
	}
	return s
}

// ScrubURLString removes sensitive query parameters for logging.
func ScrubURLString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid-url]"
	}
	q := u.Query()
	for k := range q {
		if _, bad := sensitiveQueryKeys[strings.ToLower(k)]; bad {
			q.Set(k, "[REDACTED]")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
