// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy

import (
	"strconv"
	"strings"
)

// parseContentRangeResponse parses "bytes start-end/total" or
// "bytes start-end/*" into half-open [start,end) and total (-1 if "*").
func parseContentRangeResponse(s string) (start, end, total int64, ok bool) {
	s = strings.TrimSpace(s)
	const pfx = "bytes "
	if !strings.HasPrefix(strings.ToLower(s), pfx) {
		return 0, 0, 0, false
	}
	s = s[len(pfx):]
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return 0, 0, 0, false
	}
	se := strings.TrimSpace(parts[0])
	totalStr := strings.TrimSpace(parts[1])
	dash := strings.Index(se, "-")
	if dash < 0 {
		return 0, 0, 0, false
	}
	a, err1 := strconv.ParseInt(se[:dash], 10, 64)
	b, err2 := strconv.ParseInt(se[dash+1:], 10, 64)
	if err1 != nil || err2 != nil || a < 0 || b < a {
		return 0, 0, 0, false
	}
	end = b + 1
	if totalStr == "*" {
		total = -1
	} else {
		t, err := strconv.ParseInt(totalStr, 10, 64)
		if err != nil || t < 0 {
			return 0, 0, 0, false
		}
		total = t
	}
	return a, end, total, true
}
