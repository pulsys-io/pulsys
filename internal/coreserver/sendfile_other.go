// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build !linux && !darwin

package coreserver

import (
	"errors"
	"syscall"
)

var errNotTCP = errors.New("coreserver: sendfile unsupported on this platform")

// sendFileViaRaw always fails on non-Linux / non-Darwin platforms; the
// caller falls back to the io.CopyBuffer path.
func sendFileViaRaw(_ syscall.RawConn, _ int, _, _ int64, _ *int64) (int64, error) {
	return 0, errNotTCP
}
