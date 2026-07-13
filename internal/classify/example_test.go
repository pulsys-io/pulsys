// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package classify_test

import (
	"fmt"

	"github.com/pulsys-io/pulsys/internal/classify"
)

// ExampleArtifactGET shows how pulsys decides which GET requests carry file
// content (and must therefore be teed to disk on an upstream miss) versus which
// are hub JSON-metadata calls that are passed straight through.
func ExampleArtifactGET() {
	const host = "huggingface.co"

	// A classic resolve URL: this is file content, so it is download-class.
	fmt.Println(classify.ArtifactGET(host, host, "GET", "/Qwen/Qwen2.5-0.5B/resolve/main/model.safetensors"))

	// A hub metadata API call on the default host: not download-class.
	fmt.Println(classify.ArtifactGET(host, host, "GET", "/api/models/Qwen/Qwen2.5-0.5B"))

	// A blob served from a different CDN/LFS host: download-class.
	fmt.Println(classify.ArtifactGET(host, "cdn-lfs.huggingface.co", "GET", "/repo/abc123"))

	// Output:
	// true
	// false
	// true
}
