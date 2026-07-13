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
	q := u.Query()
	if origin != "" {
		q.Set(OriginQueryParam, origin)
	}
	if encoded := q.Encode(); encoded != "" {
		ju.RawQuery = encoded
	}
	return ju.String(), true
}

// ExtractAndStripOrigin removes the OriginQueryParam from rawQuery and
// returns (origin, cleanedRawQuery).  Safe to call on any query string;
// when the param is absent it returns ("", rawQuery) unchanged.
func ExtractAndStripOrigin(rawQuery string) (origin, cleaned string) {
	if rawQuery == "" || !strings.Contains(rawQuery, OriginQueryParam) {
		return "", rawQuery
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", rawQuery
	}
	origin = q.Get(OriginQueryParam)
	q.Del(OriginQueryParam)
	return origin, q.Encode()
}
