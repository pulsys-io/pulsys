// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package auth

import (
	"path"
	"strings"
)

// MatchRepoGlob reports whether repo (e.g. "org/model") matches any
// of the glob patterns in ACLs.  Patterns use path.Match semantics
// with '/' as separator; a trailing "/**" is treated as prefix match.
func MatchRepoGlob(repo string, patterns []string) bool {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		return false
	}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == "*" || p == "**" {
			return true
		}
		if strings.HasSuffix(p, "/**") {
			prefix := strings.TrimSuffix(p, "/**")
			if repo == prefix || strings.HasPrefix(repo, prefix+"/") {
				return true
			}
			continue
		}
		if ok, _ := path.Match(p, repo); ok {
			return true
		}
	}
	return false
}

// RoleFromGroups maps IdP group membership to a Pulsys role using the
// configured owner/admin group lists.  Falls back to defaultRole when
// no elevated group matches.
func RoleFromGroups(groups []string, ownerGroups, adminGroups []string, defaultRole Role) Role {
	set := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		set[g] = struct{}{}
	}
	for _, g := range ownerGroups {
		if _, ok := set[g]; ok {
			return RoleOwner
		}
	}
	for _, g := range adminGroups {
		if _, ok := set[g]; ok {
			return RoleAdmin
		}
	}
	return defaultRole
}
