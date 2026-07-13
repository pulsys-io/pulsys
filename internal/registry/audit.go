// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package registry

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditExecer is the minimal Exec surface the proxy upload handler
// uses for emitting audit_log rows. The concrete type is *pgxpool.Pool
// in production; tests may substitute a wrapper or nil to disable.
type AuditExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Compile-time assertion: pgxpool.Pool already satisfies AuditExecer.
var _ AuditExecer = (*pgxpool.Pool)(nil)
