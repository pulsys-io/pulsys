// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package realhf contains tests that exercise huggingface.co directly.
//
// These are deliberately quarantined behind two gates:
//
//  1. The `realhf` build tag (so `go test ./...` skips them entirely).
//  2. The HF_INTEGRATION_REAL=1 env var (so even a build with the tag
//     skips when the env says don't go to the network).
//
// Read tests run with just the gates. Write tests additionally
// require:
//
//   - HF_TOKEN, with `repo.write` scope
//   - HF_INTEGRATION_REAL_WRITE=1 (explicit opt-in for mutations on
//     the user's HF account)
//
// Both gates exist because these tests will create + delete real
// private repos on the configured HF account; nothing about that
// should happen by accident.
//
// Run locally:
//
//	HF_INTEGRATION_REAL=1 \
//	HF_INTEGRATION_REAL_WRITE=1 \
//	HF_TOKEN=hf_... \
//	go test -tags realhf -timeout 5m ./internal/realhf/...
//
// CI runs the read suite nightly; the write suite is opt-in per run.
package realhf
