// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package authcontract

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestAdminContract_Completeness is the safety net that prevents the
// 2026-05-21 class of bug.  It parses every Go file that mounts an
// admin HTTP route, extracts the literal mux.HandleFunc("PAT", ...)
// patterns, and asserts every one of them appears in the
// adminEndpoints contract table (and vice versa).
//
// Why this matters: without this check, the contract test only
// proves "endpoints we remembered to enumerate are gated correctly".
// A new POST /admin/api/v1/foo added in a future PR could ship
// without any auth-rejection coverage, and a reviewer would have to
// spot it visually.  This test makes the omission a build failure.
//
// The check is intentionally pattern-based (string equality on the
// Go-1.22 mux pattern syntax) rather than AST-deep: it asks "does
// the symbol mux.HandleFunc get called with literal X?" and trusts
// the contract test to verify behavior for X.
func TestAdminContract_Completeness(t *testing.T) {
	// Source files that own admin-surface route registration.  When
	// a new file mounts admin routes, it must be listed here.
	sources := []string{
		"../../admin/api/handler.go",  // /admin/api/v1/*
		"../../auth/httpx/handler.go", // /auth/*
		"../../admin/server.go",       // /healthz, /metrics, /auth/, /admin/
	}

	patterns, err := collectMountedPatterns(sources)
	if err != nil {
		t.Fatalf("scan handler sources: %v", err)
	}

	// Some patterns are mount-prefixes (root.Handle("/auth/", ...)),
	// not concrete endpoint patterns; drop them.  We only want
	// METHOD-prefixed exact patterns.  The contract enumerates
	// individual endpoints under those prefixes.
	concrete := map[string]string{}
	for pat, src := range patterns {
		if !looksLikeMethodPattern(pat) {
			continue
		}
		concrete[pat] = src
	}

	// Build a stub fixtures only deeply enough to populate the table
	// (the contract entries don't actually exercise the DB at the
	// completeness step -- we only enumerate them).
	declared := map[string]bool{}
	for _, ep := range adminEndpoints(nil) {
		key := normalisePattern(ep.Method + " " + ep.Path)
		declared[key] = true
	}

	// Every mounted route must be in the contract.
	var missingInContract []string
	for pat, src := range concrete {
		mountKey := normalisePattern(pat)
		if !anyContractMatches(mountKey, declared) {
			missingInContract = append(missingInContract,
				fmt.Sprintf("  - %s (mounted in %s) is NOT in adminEndpoints()", pat, src))
		}
	}
	sort.Strings(missingInContract)

	// Every contract entry must correspond to a mounted route.
	var missingMount []string
	for _, ep := range adminEndpoints(nil) {
		key := normalisePattern(ep.Method + " " + ep.Path)
		if _, ok := matchPattern(concrete, key); !ok {
			missingMount = append(missingMount,
				fmt.Sprintf("  - %s is in adminEndpoints() but no source file mounts it", key))
		}
	}
	sort.Strings(missingMount)

	if len(missingInContract) > 0 || len(missingMount) > 0 {
		t.Errorf("auth contract is out of sync with mounted routes.\n"+
			"%d routes mounted but not in contract:\n%s\n"+
			"%d contract entries with no matching mount:\n%s\n\n"+
			"Add or remove rows in adminEndpoints() to match the "+
			"current mount surface.  Every endpoint MUST have an "+
			"explicit auth expectation -- this check exists because "+
			"the 2026-05-21 incident proved that 'we forgot to add "+
			"auth coverage for this endpoint' is a real failure mode.",
			len(missingInContract), strings.Join(missingInContract, "\n"),
			len(missingMount), strings.Join(missingMount, "\n"))
	}
}

// collectMountedPatterns parses the given Go source files and
// returns a map from literal pattern -> file:line where each
// mux.HandleFunc("PAT", ...) and root.HandleFunc("PAT", ...) call
// appears.  Patterns are kept verbatim (METHOD prefix included)
// because that's how net/http.ServeMux interprets them.
func collectMountedPatterns(files []string) (map[string]string, error) {
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, src := range files {
		abs, err := filepath.Abs(src)
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(fset, abs, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", src, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// We care about *ServeMux.HandleFunc / *ServeMux.Handle
			// invocations.  The selector name catches both; the
			// pattern is always the first argument.
			if sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pat, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			out[pat] = fmt.Sprintf("%s:%d", filepath.Base(src), fset.Position(lit.Pos()).Line)
			return true
		})
	}
	return out, nil
}

// looksLikeMethodPattern reports whether pat is a concrete endpoint
// pattern of the form "METHOD /path...".  Mount prefixes like
// "/auth/" and "/admin/" are not concrete; they're handled by their
// own Mount() functions and we cover the resulting child patterns.
func looksLikeMethodPattern(pat string) bool {
	space := strings.IndexByte(pat, ' ')
	if space <= 0 {
		return false
	}
	method := pat[:space]
	switch method {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
		return true
	}
	return false
}

// normalisePattern collapses Go 1.22 path-parameter syntax to a
// canonical form so the contract entry "DELETE /a/b/00000000-..."
// matches the mount entry "DELETE /a/b/{id}".  The mount-side {name}
// segments are rewritten to a sentinel; the contract-side path is
// split on '/' and any segment looking like a UUID or non-empty
// non-static blob is rewritten too.
func normalisePattern(pat string) string {
	space := strings.IndexByte(pat, ' ')
	if space <= 0 {
		return pat
	}
	method, path := pat[:space], pat[space+1:]
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = "{*}"
		}
	}
	return method + " " + strings.Join(segs, "/")
}

// matchPattern looks up key (a normalised contract pattern) in
// concrete by segment-aligned matching.  Mount patterns may carry
// {*} wildcards (the output of normalisePattern); contract patterns
// carry concrete values (e.g. "00000000-..." for an {id} param).  A
// wildcard segment matches any contract segment; static segments
// must match verbatim.  Returns the raw mounted pattern + true on
// success.
func matchPattern(concrete map[string]string, key string) (string, bool) {
	keyMethod, keyPath, ok := splitPattern(key)
	if !ok {
		return "", false
	}
	keySegs := strings.Split(keyPath, "/")
	for pat := range concrete {
		mMethod, mPath, ok := splitPattern(normalisePattern(pat))
		if !ok || mMethod != keyMethod {
			continue
		}
		mSegs := strings.Split(mPath, "/")
		if len(mSegs) != len(keySegs) {
			continue
		}
		matched := true
		for i := range mSegs {
			if mSegs[i] == "{*}" {
				continue
			}
			if mSegs[i] != keySegs[i] {
				matched = false
				break
			}
		}
		if matched {
			return pat, true
		}
	}
	return "", false
}

// splitPattern splits "METHOD /path" into ("METHOD", "/path", true).
// Returns ok=false for inputs missing the space delimiter.
func splitPattern(pat string) (method, path string, ok bool) {
	space := strings.IndexByte(pat, ' ')
	if space <= 0 {
		return "", "", false
	}
	return pat[:space], pat[space+1:], true
}

// anyContractMatches reports whether the mount-side key (which may
// contain {*} wildcard segments) matches any declared contract key
// (which carries concrete path-param values).  Segment-aligned: a
// {*} mount segment accepts any concrete segment; static segments
// must match verbatim.
func anyContractMatches(mountKey string, declared map[string]bool) bool {
	mMethod, mPath, ok := splitPattern(mountKey)
	if !ok {
		return false
	}
	mSegs := strings.Split(mPath, "/")
	for declKey := range declared {
		dMethod, dPath, ok := splitPattern(declKey)
		if !ok || dMethod != mMethod {
			continue
		}
		dSegs := strings.Split(dPath, "/")
		if len(dSegs) != len(mSegs) {
			continue
		}
		matched := true
		for i := range mSegs {
			if mSegs[i] == "{*}" {
				continue
			}
			if mSegs[i] != dSegs[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
