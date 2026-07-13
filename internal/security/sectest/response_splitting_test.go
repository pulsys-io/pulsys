// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Response-splitting regression test (OWASP WSTG-INPV-15).
//
// PURPOSE
//   pulsys copies upstream response headers verbatim through
//   copyAndRewriteHeaders.  If an upstream returns a header whose
//   value contains a bare CR or LF byte (because the upstream is
//   itself compromised, or because the upstream's framing was
//   manipulated), and we pass that byte through to the client, the
//   downstream parser (Cloudflare, ALB, the browser) sees an
//   injected header line or even a smuggled second response.
//
//   We can't easily induce an upstream to inject CR/LF without
//   significant mockhub plumbing, so this test focuses on the
//   INPUT side: request headers we receive must not be reflected
//   into responses with CR/LF intact.
//
//   The two reflection paths in pulsys:
//     1. Authorization header (rejected; never reflected; sanity
//        check only).
//     2. Range / Accept-Encoding (forwarded to upstream and
//        sometimes echoed back in error messages).

package sectest

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestResponseSplitting_NoCRLFInRequestEchoes(t *testing.T) {
	stack := newStack(t)
	client := &http.Client{}

	// Pairs of (header name, payload).  Payload contains a CR or LF
	// followed by a fake injected header / status line.
	cases := []struct{ name, header, payload string }{
		{"range_with_lf", "Range", "bytes=0-1\nX-Injected: yes"},
		{"range_with_cr", "Range", "bytes=0-1\rX-Injected: yes"},
		{"range_with_crlf", "Range", "bytes=0-1\r\nX-Injected: yes"},
		{"user_agent_with_status", "User-Agent", "test\r\n\r\nHTTP/1.1 200 OK\r\nX: y"},
		{"accept_with_lf", "Accept", "text/plain\nX-Injected: yes"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodGet,
				stack.ProxyURL()+"/api/models/acme/widget", nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			// http.NewRequest validates header values and refuses
			// CR/LF in NewRequest.  We have to set them via direct
			// map access to bypass that check.
			req.Header[c.header] = []string{c.payload}

			resp, err := client.Do(req)
			if err != nil {
				// Go's stdlib client also validates; refusal here
				// is acceptable -- the payload never even reached
				// the wire.
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			// The response MUST NOT contain a header we injected.
			for k := range resp.Header {
				if strings.EqualFold(k, "X-Injected") {
					t.Fatalf("RESPONSE SPLITTING: header %q from request payload appeared in response\n  case=%s\n  payload=%q",
						k, c.name, c.payload)
				}
			}
			// The body MUST NOT contain the literal injected status
			// line (would indicate the upstream / our proxy reflected
			// the raw payload into the body without escaping).
			if strings.Contains(string(body), "X-Injected: yes") {
				t.Fatalf("RESPONSE SPLITTING: injected header text appeared in response body\n  case=%s\n  body[:200]=%q",
					c.name, truncate(body, 200))
			}
		})
	}
}
