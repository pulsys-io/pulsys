// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Path-traversal regression test (OWASP WSTG-INPV-11).
//
// PURPOSE
//   pulsys routes URLs of the form
//     /<org>/<repo>/resolve/<rev>/<file>
//   to (a) an on-disk cache key derived from the path and (b) an
//   upstream HuggingFace fetch.  Both must reject any traversal
//   sequence the client tries to smuggle through the path component.
//
//   We probe the well-known traversal forms:
//     ../  ..%2f  %2e%2e/  %2e%2e%2f  ..%5c  ..%c0%af  ..%252f
//     %00  /./  /%2e/  long-NUL-suffixes
//
//   ACCEPTANCE
//     Either the proxy returns 4xx (rejected) OR a normalised path
//     that does NOT escape /<org>/<repo>/.  The latter is the
//     fallback semantic the stdlib path cleaner provides; we don't
//     mandate one or the other, only that no test request returns
//     content from outside the namespace.

package sectest

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPathTraversal_NoEscape(t *testing.T) {
	stack := newStack(t)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // don't follow redirects
		},
	}

	vectors := []struct{ name, path string }{
		{"dotdot_slash", "/acme/widget/resolve/main/../../../etc/passwd"},
		{"encoded_dotdot", "/acme/widget/resolve/main/%2e%2e/%2e%2e/etc/passwd"},
		{"mixed_encoded", "/acme/widget/resolve/main/..%2fetc/passwd"},
		{"double_encoded", "/acme/widget/resolve/main/%252e%252e/etc/passwd"},
		{"backslash", "/acme/widget/resolve/main/..\\..\\etc/passwd"},
		{"overlong_utf8", "/acme/widget/resolve/main/..%c0%afetc/passwd"},
		{"nul_byte", "/acme/widget/resolve/main/file.bin%00.txt"},
		{"trailing_dot_segments", "/acme/widget/resolve/main/./././../etc/passwd"},
		{"absolute_path_replace", "/acme/widget/resolve/main//etc/passwd"},
		{"home_dir_expansion", "/acme/widget/resolve/main/~root/.ssh/id_rsa"},
	}

	for _, v := range vectors {
		v := v
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodGet, stack.ProxyURL()+v.path, nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				// Network-level rejection (parser refused) is fine.
				return
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			// Two acceptable outcomes:
			//   1. 4xx -- explicit rejection.
			//   2. 404 -- normalised path doesn't match anything.
			// FAIL condition: 200 OK -- means we served content
			// from outside the intended namespace.
			if resp.StatusCode == 200 {
				t.Fatalf("PATH TRAVERSAL: %s returned 200; body[:200]=%q",
					v.path, truncate(body, 200))
			}

			// Body must not contain canonical /etc/passwd markers
			// regardless of status (defense in depth against a
			// future regression that lets the upstream see the
			// raw traversal sequence).
			lower := strings.ToLower(string(body))
			for _, leak := range []string{"root:x:", "root:!:", "nobody:x:"} {
				if strings.Contains(lower, leak) {
					t.Fatalf("PATH TRAVERSAL: %s leaked /etc/passwd marker %q\n  body[:200]=%q",
						v.path, leak, truncate(body, 200))
				}
			}
		})
	}
}
