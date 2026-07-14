// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package hostallow is the single source of truth for deciding whether
// pulsys may open an upstream connection to a given host.
//
// It combines an unconditional SSRF deny gate with the operator's
// allowlist:
//
//   - Loopback literals: 127.0.0.1, ::1, localhost (any case).
//   - Link-local IPv4: 169.254.0.0/16 (AWS / GCP / Azure IMDS).
//   - Link-local IPv6: fe80::/10.
//   - Private IPv4: 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16.
//   - Unique local IPv6: fc00::/7.
//   - Cloud metadata DNS: metadata.google.internal, metadata.aws.internal.
//
// A host in one of those classes is denied even if the operator lists
// it, because it can never be a legitimate public Hugging Face mirror.
// The allowlist is consulted only after a host clears the deny gate.
//
// The matcher is enforced twice: when the proxy routes an inbound
// /_p/<host>/ request (internal/proxy) and again at the outbound sink
// (internal/upstream), so a routing regression cannot become an SSRF.
package hostallow

import (
	"net"
	"strings"
)

// Matcher reports whether host (optionally "host:port", possibly a
// bracketed IPv6 literal) may be contacted upstream.
type Matcher func(host string) bool

// denyHostnames are literal names that are never a legitimate upstream
// and are the canonical SSRF payloads.
var denyHostnames = map[string]struct{}{
	"localhost":                {},
	"metadata.google.internal": {},
	"metadata.aws.internal":    {},
	"169.254.169.254":          {},
}

// New returns a Matcher for the given allowlist of host suffixes/exact
// names (compared case-insensitively). The slice is copied so later
// mutation by the caller cannot widen the allowlist.
func New(allowed []string) Matcher {
	allow := append([]string(nil), allowed...)
	return func(host string) bool {
		h := strings.ToLower(strings.TrimSpace(host))
		if h == "" {
			return false
		}
		bare := bareHost(h)
		if _, deny := denyHostnames[bare]; deny {
			return false
		}
		if ip := parseHostIP(bare); ip != nil && ipIsPrivate(ip) {
			return false
		}
		for _, a := range allow {
			if h == a || strings.HasSuffix(h, "."+a) {
				return true
			}
			// Also match the port-stripped form so "huggingface.co:443"
			// matches the "huggingface.co" allowlist entry.
			if bare == a || strings.HasSuffix(bare, "."+a) {
				return true
			}
		}
		return false
	}
}

// bareHost strips any ":port" suffix and IPv6 brackets so ParseIP and
// allowlist comparisons see a bare address/name.
func bareHost(h string) string {
	if i := strings.LastIndex(h, ":"); i >= 0 {
		if strings.HasPrefix(h, "[") {
			if j := strings.Index(h, "]"); j > 0 {
				return strings.Trim(h[:j+1], "[]")
			}
		} else {
			return h[:i]
		}
	} else if strings.HasPrefix(h, "[") {
		return strings.Trim(h, "[]")
	}
	return h
}

// parseHostIP parses bare as an IPv4 or (bracket-stripped) IPv6 literal,
// returning nil for DNS names.
func parseHostIP(bare string) net.IP {
	return net.ParseIP(strings.Trim(bare, "[]"))
}

// ipIsPrivate reports whether ip is loopback, link-local, RFC 1918 /
// RFC 4193, or the unspecified address (0.0.0.0 / ::), none of which
// may ever be an upstream target.
func ipIsPrivate(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsPrivate() ||
		ip.IsUnspecified()
}
