// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package integration

import "testing"

func TestIntegrationRequiresBuildTag(t *testing.T) {
	t.Skip("full HF CLI tests: go test -tags=integration ./internal/integration -run HFCLIDownload -count=1 with HF_INTEGRATION=1")
}
