// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux

package coreserver

// InitIoUringForTest exposes ring setup for integration tests.
func InitIoUringForTest(s *Server) error {
	return s.ioUringInit()
}
