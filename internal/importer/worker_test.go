// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package importer

import (
	"testing"
)

func TestRepoType(t *testing.T) {
	for _, tc := range []string{"", "model", "models", "dataset", "datasets", "space", "spaces"} {
		if _, err := repoType(tc); err != nil {
			t.Fatalf("repoType(%q): %v", tc, err)
		}
	}
	if _, err := repoType("unknown"); err == nil {
		t.Fatal("expected unknown repo type error")
	}
}
