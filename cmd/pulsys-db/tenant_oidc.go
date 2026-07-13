// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/pulsys-io/pulsys/internal/auth"
	"github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/db"
)

func runTenant(args []string, dsn string, ctx context.Context) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pulsys-db tenant ensure --name NAME --display-name TEXT")
		return 1
	}
	switch args[1] {
	case "ensure":
		return doTenantEnsure(ctx, dsn, args[2:])
	default:
		fmt.Fprintf(os.Stderr, "pulsys-db: unknown tenant subcommand %q\n", args[1])
		return 1
	}
}

func doTenantEnsure(ctx context.Context, dsn string, args []string) int {
	fs := flag.NewFlagSet("tenant ensure", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	name := fs.String("name", "default", "tenant slug")
	display := fs.String("display-name", "Default Tenant", "human label")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AppName: "pulsys-db"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: tenant ensure: %v\n", err)
		return 2
	}
	defer pool.Close()
	s := store.NewPG(pool.Pgx())
	id, err := s.EnsureTenant(ctx, *name, *display)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: tenant ensure: %v\n", err)
		return 2
	}
	fmt.Printf("tenant_id=%s name=%s\n", id, *name)
	return 0
}

func runOIDC(args []string, dsn string, ctx context.Context) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pulsys-db oidc configure --tenant NAME --issuer URL --client-id ID --client-secret SECRET --redirect-uri URL")
		return 1
	}
	switch args[1] {
	case "configure":
		return doOIDCConfigure(ctx, dsn, args[2:])
	default:
		fmt.Fprintf(os.Stderr, "pulsys-db: unknown oidc subcommand %q\n", args[1])
		return 1
	}
}

func doOIDCConfigure(ctx context.Context, dsn string, args []string) int {
	fs := flag.NewFlagSet("oidc configure", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tenantName := fs.String("tenant", "default", "tenant slug")
	issuer := fs.String("issuer", "", "OIDC issuer URL")
	clientID := fs.String("client-id", "", "OAuth client id")
	clientSecret := fs.String("client-secret", "", "OAuth client secret")
	redirectURI := fs.String("redirect-uri", "", "registered redirect URI")
	scopes := fs.String("scopes", "openid profile email", "OIDC scopes")
	groupsClaim := fs.String("groups-claim", "groups", "JWT claim for group membership")
	ownerGroups := fs.String("owner-groups", "pulsys:owner", "comma-separated owner groups")
	adminGroups := fs.String("admin-groups", "pulsys:admin", "comma-separated admin groups")
	jitRole := fs.String("jit-default-role", "member", "default role for JIT users")
	requirePre := fs.Bool("require-preprovisioned", false, "deny JIT for unknown users")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *issuer == "" || *clientID == "" || *clientSecret == "" || *redirectURI == "" {
		fmt.Fprintln(os.Stderr, "pulsys-db: oidc configure: --issuer, --client-id, --client-secret, --redirect-uri are required")
		return 1
	}
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AppName: "pulsys-db"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: oidc configure: %v\n", err)
		return 2
	}
	defer pool.Close()
	s := store.NewPG(pool.Pgx())
	tid, err := s.GetTenantIDByName(ctx, *tenantName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: oidc configure: tenant %q: %v\n", *tenantName, err)
		return 2
	}
	role, err := auth.ParseRole(*jitRole)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: oidc configure: %v\n", err)
		return 1
	}
	p := auth.OIDCProvider{
		TenantID:              tid,
		Issuer:                *issuer,
		ClientID:              *clientID,
		ClientSecret:          *clientSecret,
		RedirectURI:           *redirectURI,
		Scopes:                *scopes,
		Enabled:               true,
		GroupsClaim:           *groupsClaim,
		OwnerGroups:           splitCSV(*ownerGroups),
		AdminGroups:           splitCSV(*adminGroups),
		JITDefaultRole:        role,
		RequirePreprovisioned: *requirePre,
	}
	if err := s.UpsertOIDCProvider(ctx, p); err != nil {
		fmt.Fprintf(os.Stderr, "pulsys-db: oidc configure: %v\n", err)
		return 2
	}
	fmt.Printf("oidc configured tenant=%s issuer=%s\n", *tenantName, *issuer)
	return 0
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
