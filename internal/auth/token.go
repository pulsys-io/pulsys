// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// SessionCookieName is the HttpOnly cookie carrying the opaque
	// session token after OIDC login.
	SessionCookieName = "pulsys_session"

	// DefaultSessionTTL is the sliding session lifetime when tenant
	// settings do not override auth.session_ttl.
	DefaultSessionTTL = 8 * 60 * 60 // seconds; wired in session service

	patPrefix = "pulsys_"
)

// TokenHash returns the sha256 digest stored in tokens.hash and
// sessions.token_hash.
func TokenHash(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// RandomToken generates a URL-safe opaque token of n bytes entropy
// (before base64 encoding expands length).
func RandomToken(n int) (string, error) {
	if n < 16 {
		n = 16
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// GeneratePAT creates a new personal access token display value and
// its sha256 hash.  Format: pulsys_<8-char-prefix>_<secret>.
func GeneratePAT() (display, prefix string, hash []byte, err error) {
	secret, err := RandomToken(32)
	if err != nil {
		return "", "", nil, err
	}
	prefixBytes := make([]byte, 4)
	if _, err := rand.Read(prefixBytes); err != nil {
		return "", "", nil, err
	}
	prefix = hex.EncodeToString(prefixBytes)
	display = fmt.Sprintf("%s%s_%s", patPrefix, prefix, secret)
	return display, prefix, TokenHash(display), nil
}

// ParseBearer extracts the bearer token from an Authorization header.
func ParseBearer(h string) (string, bool) {
	const pfx = "Bearer "
	if len(h) < len(pfx) || !strings.EqualFold(h[:len(pfx)], pfx) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(pfx):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// IsPAT reports whether a bearer value looks like a Pulsys PAT.
func IsPAT(tok string) bool {
	return strings.HasPrefix(tok, patPrefix)
}
