// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package authcontract

import (
	"fmt"
	"net/http"
)

// Credential identifies one of the credential classes the matrix
// drives.  Anonymous is the "no Authorization header, no session
// cookie" baseline; everything else is named after the fixture row it
// references.  Adding a new credential class requires only:
//
//  1. extending the const block,
//  2. extending fixtures.Apply to populate the request,
//  3. setting the expected Outcome in every endpoint's row.
//
// Step (3) is enforced by the matrix runner: missing map entries
// cause a t.Fatal naming the offending (endpoint, credential) pair.
type Credential int

const (
	CredAnonymous Credential = iota

	// PATs.  Bogus is the well-formed but never-issued case the user
	// reported; revoked / expired are the lifecycle states we
	// previously failed to enforce; the three scoped PATs validate
	// requireAccess's PAT branch.
	CredBogusPAT
	CredRevokedPAT
	CredExpiredPAT
	CredPATScopeRead     // scope = admin:read
	CredPATScopeWrite    // scope = admin:write
	CredPATScopeAdminAll // scope = admin:*

	// Sessions.  One per role plus a revoked cookie to exercise the
	// "cookie was valid, user logged out, attacker replays it" case.
	CredSessionReader
	CredSessionMember
	CredSessionAdmin
	CredSessionOwner
	CredSessionRevoked
)

// allCredentials returns every credential class in stable order.
// Used by the matrix runner so a missing expectation is reported
// once per (endpoint, credential) pair rather than randomly.
func allCredentials() []Credential {
	return []Credential{
		CredAnonymous,
		CredBogusPAT,
		CredRevokedPAT,
		CredExpiredPAT,
		CredPATScopeRead,
		CredPATScopeWrite,
		CredPATScopeAdminAll,
		CredSessionReader,
		CredSessionMember,
		CredSessionAdmin,
		CredSessionOwner,
		CredSessionRevoked,
	}
}

// dataPlaneCredentials is the subset relevant to the proxy data
// plane.  The data plane has no cookie or role concept -- only PATs
// matter -- so sessions are excluded.  Keeping the lists separate
// also keeps the data-plane matrix small.
func dataPlaneCredentials() []Credential {
	return []Credential{
		CredAnonymous,
		CredBogusPAT,
		CredRevokedPAT,
		CredExpiredPAT,
		CredPATScopeRead,
		CredPATScopeWrite,
		CredPATScopeAdminAll,
	}
}

func (c Credential) String() string {
	switch c {
	case CredAnonymous:
		return "anonymous"
	case CredBogusPAT:
		return "bogus-PAT"
	case CredRevokedPAT:
		return "revoked-PAT"
	case CredExpiredPAT:
		return "expired-PAT"
	case CredPATScopeRead:
		return "PAT(admin:read)"
	case CredPATScopeWrite:
		return "PAT(admin:write)"
	case CredPATScopeAdminAll:
		return "PAT(admin:*)"
	case CredSessionReader:
		return "session(reader)"
	case CredSessionMember:
		return "session(member)"
	case CredSessionAdmin:
		return "session(admin)"
	case CredSessionOwner:
		return "session(owner)"
	case CredSessionRevoked:
		return "session(revoked)"
	default:
		return fmt.Sprintf("cred(%d)", int(c))
	}
}

// Outcome is the auth-layer expectation for one (endpoint, credential)
// cell.  It deliberately does NOT enumerate body / content
// expectations: those are the concern of the owning package's tests.
type Outcome int

const (
	// Unauth401 means the response must be exactly 401.  Used for
	// every credential the endpoint refuses to authenticate.
	Unauth401 Outcome = iota

	// Forbidden403 means the response must be exactly 403.  Used
	// when a request authenticates successfully but lacks the
	// required role / scope.
	Forbidden403

	// Admitted means the response must NOT be 401 or 403.  The
	// handler ran (or returned a body-validation error); the auth
	// layer admitted the request.  2xx is the common case; for some
	// endpoints we deliberately send incomplete bodies and accept
	// 4xx as long as it's not the auth-rejection codes.
	Admitted
)

func (o Outcome) String() string {
	switch o {
	case Unauth401:
		return "401-unauthenticated"
	case Forbidden403:
		return "403-forbidden"
	case Admitted:
		return "admitted (not 401/403)"
	default:
		return fmt.Sprintf("outcome(%d)", int(o))
	}
}

// Check reports whether the observed HTTP status matches o.  Returns
// a non-empty failure message when the contract is violated.
func (o Outcome) Check(status int) string {
	switch o {
	case Unauth401:
		if status == http.StatusUnauthorized {
			return ""
		}
		return fmt.Sprintf("got status %d, want 401", status)
	case Forbidden403:
		if status == http.StatusForbidden {
			return ""
		}
		return fmt.Sprintf("got status %d, want 403", status)
	case Admitted:
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			return ""
		}
		return fmt.Sprintf("got auth-rejection status %d, want anything except 401/403", status)
	default:
		return fmt.Sprintf("unknown outcome %d", int(o))
	}
}

// Endpoint declares the auth contract for one HTTP endpoint.  Method,
// Path, and Body are sent verbatim to the test server; Outcomes maps
// every Credential class to its expected resolution.
type Endpoint struct {
	// Method is the HTTP method ("GET", "POST", "DELETE", "PUT", "HEAD").
	Method string
	// Path is the URL path including any path parameter substitutions
	// (Fixtures resolves IDs before the matrix runs).
	Path string
	// Body is the optional request body.  Nil = no body.
	Body []byte
	// ContentType, when non-empty, is set on the request.
	ContentType string
	// Outcomes is the per-credential expectation.  Missing keys are
	// a test failure -- the matrix runner enumerates every credential
	// class and asserts each is covered.
	Outcomes map[Credential]Outcome
	// Note is a short human-readable description for failure logs.
	Note string
}

// String returns "METHOD PATH" suitable for sub-test names.
func (e Endpoint) String() string {
	return e.Method + " " + e.Path
}

// requireOutcome looks up cred in e.Outcomes; missing entries panic
// at test-time so an unhandled cell never silently passes.
func (e Endpoint) requireOutcome(cred Credential) Outcome {
	out, ok := e.Outcomes[cred]
	if !ok {
		panic(fmt.Sprintf("authcontract: %s has no expectation for credential %s",
			e, cred))
	}
	return out
}
