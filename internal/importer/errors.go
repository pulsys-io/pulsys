// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package importer

import (
	"errors"

	"github.com/pulsys-io/pulsys/internal/importmsg"
)

// HumanizeImportError returns an error whose message is safe to show in the admin UI.
func HumanizeImportError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(importmsg.Humanize(err.Error()))
}
