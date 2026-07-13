// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package rewrite

import (
	"net/url"
	"strings"
)

// OriginQueryParam is the reserved query-string key we use to thread the
// originating user-facing request path through a cross-host redirect to
// a content-addressed body (Xet / LFS).  The proxy strips it from any
// inbound /_p/ request BEFORE forwarding upstream and records the
// decoded value in cache.Meta.OriginPath so the admin UI can attribute
// disk usage back to the originating model.  Prefixed with double
// underscore so it cannot collide with any upstream-generated param.
const OriginQueryParam = "__pulsys_origin"

// LocationToProxy rewrites an absolute http(s) URL to the proxy /_p/ form.
func LocationToProxy(publicBase *url.URL, allow func(host string) bool, location string) (string, bool) {
	return LocationToProxyWithOrigin(publicBase, allow, location, "")
}

// LocationToProxyWithOrigin behaves like LocationToProxy and additionally
// appends an OriginQueryParam to the rewritten URL when origin is non-empty.
// origin should be the inbound request path (eg "/Org/Name/resolve/main/file")
// that triggered the redirect.
func LocationToProxyWithOrigin(publicBase *url.URL, allow func(host string) bool, location, origin string) (string, bool) {
	u, err := url.Parse(location)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if !allow(host) {
		return "", false
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	trim := strings.TrimPrefix(path, "/")
	joined, err := url.JoinPath(publicBase.String(), "_p", host, trim)
	if err != nil {
		return "", false
	}
	ju, _ := url.Parse(joined)
	// Preserve the upstream query BYTE-FOR-BYTE.  CDN redirects (Xet /
	// LFS presigns) are signed over the exact resource URL, including
	// query-param order and percent-encoding.  Round-tripping through
	// url.Values.Encode() sorts params alphabetically and re-encodes
	// values, which invalidates the signature upstream (observed as
	// 403 "invalid resource" from the Xet CDN bridge).  We only ever
	// APPEND our origin marker; ExtractAndStripOrigin removes it
	// textually on the way back for the same reason.
	rawq := u.RawQuery
	if origin != "" {
		p := OriginQueryParam + "=" + url.QueryEscape(origin)
		if rawq == "" {
			rawq = p
		} else {
			rawq += "&" + p
		}
	}
	ju.RawQuery = rawq
	return ju.String(), true
}

// ExtractAndStripOrigin removes the OriginQueryParam from rawQuery and
// returns (origin, cleanedRawQuery).  Safe to call on any query string;
// when the param is absent it returns ("", rawQuery) unchanged.
//
// The strip is textual: every byte of the remaining query is preserved
// exactly as received.  Presigned CDN queries are signature-covered,
// so parsing and re-encoding them (which sorts and re-escapes params)
// breaks the signature upstream.
func ExtractAndStripOrigin(rawQuery string) (origin, cleaned string) {
	if rawQuery == "" || !strings.Contains(rawQuery, OriginQueryParam) {
		return "", rawQuery
	}
	segs := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(segs))
	for _, s := range segs {
		if v, ok := strings.CutPrefix(s, OriginQueryParam+"="); ok {
			if origin == "" {
				if dec, err := url.QueryUnescape(v); err == nil {
					origin = dec
				}
			}
			continue
		}
		if s == OriginQueryParam {
			continue
		}
		kept = append(kept, s)
	}
	return origin, strings.Join(kept, "&")
}
