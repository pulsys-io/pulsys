// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package classify

import "testing"

func TestArtifactGET(t *testing.T) {
	const def = "huggingface.co"
	cases := []struct {
		name         string
		upstreamHost string
		method       string
		path         string
		want         bool
	}{
		{"resolve", def, "GET", "/gpt2/resolve/main/config.json", true},
		{"api models", def, "GET", "/api/models", false},
		{"cdn blob", "cdn.example.com", "GET", "/blob/xyz", true},
		{"cdn api", "cdn.example.com", "GET", "/api/foo", false},
		{"_p prefix", def, "GET", "/_p/cdn-lfs.huggingface.co/x/y", true},
		{"info lfs", def, "GET", "/repo/info/lfs/something", true},
		{"encoded resolve", def, "GET", "/repo/Resolve%2fmain/x", true},
		{"non-GET", def, "POST", "/gpt2/resolve/main/x", false},
		{"upper resolve", def, "GET", "/foo/RESOLVE/main/x", true},
		{"resolve-cache models", def, "GET", "/api/resolve-cache/models/foo/bar/abc/config.json", true},
		{"resolve-cache datasets", def, "GET", "/api/resolve-cache/datasets/foo/bar/abc/data.parquet", true},
		{"resolve-cache spaces", def, "GET", "/api/resolve-cache/spaces/foo/bar/abc/file.bin", true},
		{"resolve-cache uppercase", def, "GET", "/API/Resolve-Cache/models/x/y/z/file", true},
		{"api/whoami still metadata", def, "GET", "/api/whoami", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArtifactGET(def, tc.upstreamHost, tc.method, tc.path); got != tc.want {
				t.Fatalf("ArtifactGET(%q,%q,%q,%q) = %v, want %v", def, tc.upstreamHost, tc.method, tc.path, got, tc.want)
			}
		})
	}
}
