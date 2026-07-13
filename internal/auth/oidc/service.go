// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package oidc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/pulsys-io/pulsys/internal/auth"
	"golang.org/x/oauth2"
)

// Service verifies id_tokens from the external IdP and issues Pulsys
// server-side sessions.
//
// OIDC login (PKCE, redirect, code exchange) runs entirely in the
// admin SPA (P6). The backend is a confidential relying party for
// token verification and session issuance only — it never generates
// code_challenge or stores code_verifier.
type Service struct {
	Store      auth.Store
	SessionTTL time.Duration
	Now        func() time.Time
}

func (s *Service) sessionTTL() time.Duration {
	if s.SessionTTL > 0 {
		return s.SessionTTL
	}
	return 8 * time.Hour
}

// PublicConfig is the OIDC metadata the admin SPA needs to run PKCE
// in the browser.  client_secret is intentionally omitted.
type PublicConfig struct {
	Issuer      string   `json:"issuer"`
	ClientID    string   `json:"client_id"`
	RedirectURI string   `json:"redirect_uri"`
	Scopes      []string `json:"scopes"`
}

// PublicConfig returns IdP settings for the tenant's enabled provider.
func (s *Service) PublicConfig(ctx context.Context, tenantID string) (*PublicConfig, error) {
	prov, err := s.Store.GetOIDCProviderByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	scopes := strings.Fields(prov.Scopes)
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	return &PublicConfig{
		Issuer:      prov.Issuer,
		ClientID:    prov.ClientID,
		RedirectURI: prov.RedirectURI,
		Scopes:      scopes,
	}, nil
}

// EstablishSession verifies rawIDToken from the frontend (after the SPA
// completed PKCE + code exchange with the IdP), JIT-provisions the user,
// and creates a server-side Pulsys session.
func (s *Service) EstablishSession(ctx context.Context, tenantID, rawIDToken string) (*auth.Session, *auth.User, error) {
	if rawIDToken == "" {
		return nil, nil, fmt.Errorf("oidc: empty id_token")
	}
	prov, err := s.Store.GetOIDCProviderByTenant(ctx, tenantID)
	if err != nil {
		return nil, nil, err
	}
	_, idVerifier, err := s.oauthConfig(ctx, prov)
	if err != nil {
		return nil, nil, err
	}
	idToken, err := idVerifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc: verify id_token: %w", err)
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, nil, fmt.Errorf("oidc: parse claims: %w", err)
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, nil, fmt.Errorf("oidc: missing sub claim")
	}
	email, _ := claims["email"].(string)
	name, _ := claims["name"].(string)
	if email == "" {
		email = sub + "@oidc.local"
	}
	if name == "" {
		name = email
	}
	groups := extractGroups(claims, prov.GroupsClaim)
	role := auth.RoleFromGroups(groups, prov.OwnerGroups, prov.AdminGroups, prov.JITDefaultRole)

	user, err := s.provisionUser(ctx, prov, sub, email, name, role)
	if err != nil {
		return nil, nil, err
	}
	sess, err := s.Store.CreateSession(ctx, user.ID, user.TenantID, s.sessionTTL())
	if err != nil {
		return nil, nil, err
	}
	return sess, user, nil
}

func (s *Service) provisionUser(ctx context.Context, prov *auth.OIDCProvider, sub, email, name string, role auth.Role) (*auth.User, error) {
	existing, err := s.Store.FindUserByOIDCSub(ctx, prov.TenantID, sub)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if err := s.Store.UpdateUserProfile(ctx, existing.ID, email, name, role); err != nil {
			return nil, err
		}
		existing.Email = email
		existing.DisplayName = name
		existing.Role = role
		return existing, nil
	}
	if prov.RequirePreprovisioned {
		return nil, auth.ErrLoginDenied
	}
	id, err := s.Store.CreateUserOIDC(ctx, auth.User{
		TenantID:    prov.TenantID,
		Email:       email,
		DisplayName: name,
		Role:        role,
		OIDCSub:     sub,
		IsActive:    true,
	})
	if err != nil {
		return nil, err
	}
	return &auth.User{
		ID:          id,
		TenantID:    prov.TenantID,
		Email:       email,
		DisplayName: name,
		Role:        role,
		OIDCSub:     sub,
		IsActive:    true,
	}, nil
}

func (s *Service) oauthConfig(ctx context.Context, prov *auth.OIDCProvider) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	// Dev / split-network deployments (e.g. docker compose) expose the IdP
	// to the browser at one URL and to the backend at another. When
	// PULSYS_OIDC_DISCOVERY_BASE is set the backend fetches the discovery
	// document from that URL while continuing to validate the id_token
	// `iss` claim against prov.Issuer (the browser-facing URL).
	discoveryURL := prov.Issuer
	if base := strings.TrimRight(os.Getenv("PULSYS_OIDC_DISCOVERY_BASE"), "/"); base != "" {
		ctx = oidc.InsecureIssuerURLContext(ctx, prov.Issuer)
		discoveryURL = base
	}
	provider, err := oidc.NewProvider(ctx, discoveryURL)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	scopes := strings.Fields(prov.Scopes)
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	cfg := &oauth2.Config{
		ClientID:     prov.ClientID,
		ClientSecret: prov.ClientSecret,
		RedirectURL:  prov.RedirectURI,
		Endpoint:     provider.Endpoint(),
		Scopes:       scopes,
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: prov.ClientID})
	return cfg, verifier, nil
}

func extractGroups(claims map[string]any, claimName string) []string {
	if claimName == "" {
		claimName = "groups"
	}
	raw, ok := claims[claimName]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		if v != "" {
			return []string{v}
		}
	}
	return nil
}
