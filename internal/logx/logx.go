// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package logx

import (
	"log/slog"
	"net/url"
	"os"
	"strings"
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
