// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/db"
)

func runUser(args []string, dsn string, ctx context.Context) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pulsys-db user grant-owner --tenant NAME (--email ADDR | --oidc-sub SUB)")
		return 1
	}
	switch args[1] {
	case "grant-owner":
		return doUserGrantOwner(ctx, dsn, args[2:])
	default:
		fmt.Fprintf(os.Stderr, "pulsys-db: unknown user subcommand %q\n", args[1])
		return 1
	}
}

// doUserGrantOwner is the break-glass recovery path: promote an existing
// tenant user to owner so an operator can regain admin access after an OIDC
// misconfiguration locks everyone out.  It matches the user by email OR
// oidc_sub and writes a system audit_log row so the action is never silent.
func doUserGrantOwner(ctx context.Context, dsn string, args []string) int {
	fs := flag.NewFlagSet("user grant-owner", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tenantName := fs.String("tenant", "default", "tenant slug")
	email := fs.String("email", "", "user email to promote")
	oidcSub := fs.String("oidc-sub", "", "user OIDC subject to promote")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if (*email == "") == (*oidcSub == "") {
		fmt.Fprintln(os.Stderr, "pulsys-db: user grant-owner: exactly one of --email or --oidc-sub is required")
		return 1
	}
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AppName: "pulsys-db"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: user grant-owner: %v\n", err)
		return 2
	}
	defer pool.Close()
	s := store.NewPG(pool.Pgx())
	tid, err := s.GetTenantIDByName(ctx, *tenantName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: user grant-owner: tenant %q: %v\n", *tenantName, err)
		return 2
	}
	uid, prev, err := s.GrantOwner(ctx, tid, *email, *oidcSub)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: user grant-owner: %v\n", err)
		return 2
	}
	fmt.Printf("granted owner tenant=%s user_id=%s previous_role=%s (audited)\n", *tenantName, uid, prev)
	return 0
}
