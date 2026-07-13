// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package auth

import "testing"

func TestRoleFromGroups(t *testing.T) {
	owner := []string{"pulsys:owner"}
	admin := []string{"pulsys:admin"}
	if got := RoleFromGroups([]string{"pulsys:owner"}, owner, admin, RoleMember); got != RoleOwner {
		t.Fatalf("owner: got %q", got)
	}
	if got := RoleFromGroups([]string{"pulsys:admin"}, owner, admin, RoleMember); got != RoleAdmin {
		t.Fatalf("admin: got %q", got)
	}
	if got := RoleFromGroups([]string{"other"}, owner, admin, RoleReader); got != RoleReader {
		t.Fatalf("default: got %q", got)
	}
}

func TestMatchRepoGlob(t *testing.T) {
	patterns := []string{"acme/*", "public/**"}
	if !MatchRepoGlob("acme/llama", patterns) {
		t.Fatal("expected acme/llama match")
	}
	if !MatchRepoGlob("public/models/x", patterns) {
		t.Fatal("expected public/** match")
	}
	if MatchRepoGlob("private/x", patterns) {
		t.Fatal("expected no match")
	}
}

func TestGeneratePAT(t *testing.T) {
	display, prefix, hash, err := GeneratePAT()
	if err != nil {
		t.Fatal(err)
	}
	if !IsPAT(display) {
		t.Fatalf("not a PAT: %q", display)
	}
	if len(hash) != 32 {
		t.Fatalf("hash len %d", len(hash))
	}
	if prefix == "" {
		t.Fatal("empty prefix")
	}
}

func TestParseBearer(t *testing.T) {
	tok, ok := ParseBearer("Bearer abc123")
	if !ok || tok != "abc123" {
		t.Fatalf("got %q ok=%v", tok, ok)
	}
	if _, ok := ParseBearer("Basic x"); ok {
		t.Fatal("expected false")
	}
}
