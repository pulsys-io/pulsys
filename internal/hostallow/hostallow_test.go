// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package hostallow

import "testing"

func TestMatcher(t *testing.T) {
	allow := New([]string{"huggingface.co", "hf.co", "cas-bridge.xethub.hf.co"})

	cases := []struct {
		host string
		want bool
	}{
		// Allowlisted exact + suffix matches, with and without port.
		{"huggingface.co", true},
		{"huggingface.co:443", true},
		{"cdn-lfs.huggingface.co", true},
		{"cas-bridge.xethub.hf.co", true},
		{"HuggingFace.CO", true},

		// Off-allowlist public hosts.
		{"evil.com", false},
		{"huggingface.co.evil.com", false},
		{"", false},

		// SSRF deny gate: loopback, link-local/IMDS, RFC1918, metadata,
		// unspecified. Denied even though not on the allowlist anyway.
		{"127.0.0.1", false},
		{"localhost", false},
		{"[::1]", false},
		{"169.254.169.254", false},
		{"metadata.google.internal", false},
		{"metadata.aws.internal", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"192.168.0.1", false},
		{"[fe80::1]", false},
		{"0.0.0.0", false},
	}
	for _, c := range cases {
		if got := allow(c.host); got != c.want {
			t.Errorf("allow(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestMatcherDeniesEvenWhenExplicitlyAllowed(t *testing.T) {
	// An operator that mistakenly lists a private/loopback host must
	// still be denied: the SSRF gate precedes the allowlist.
	allow := New([]string{"127.0.0.1", "169.254.169.254", "localhost"})
	for _, h := range []string{"127.0.0.1", "169.254.169.254", "localhost"} {
		if allow(h) {
			t.Errorf("allow(%q) = true, want false (deny gate must win)", h)
		}
	}
}

func TestMatcherCopiesAllowlist(t *testing.T) {
	src := []string{"huggingface.co"}
	allow := New(src)
	src[0] = "evil.com" // must not retroactively widen the matcher
	if allow("evil.com") {
		t.Fatal("mutating the caller slice widened the allowlist")
	}
	if !allow("huggingface.co") {
		t.Fatal("original allowlist entry lost")
	}
}
