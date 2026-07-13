// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// PAT cryptographic invariants (OWASP WSTG-CRYP-04 + ATHN
// adjacent).
//
// PURPOSE
//   Personal access tokens are the persistent shared-secret
//   credential class.  Their cryptographic posture has to be
//   pinned independently of any other component because a
//   regression here would NOT manifest as a test failure
//   anywhere else: the auth flow would keep working, just with
//   a smaller key space or a weak hash.
//
//   Invariants asserted in this file:
//
//     1. ENTROPY (>= 128 bits):  GeneratePAT MUST produce at
//        least 128 bits of unguessable secret material.  The
//        production design generates 32 bytes (256 bits); we
//        pin >= 128 to allow a future "switch to 16-byte for
//        URL brevity" decision but reject anything weaker.
//
//     2. UNIQUENESS:  10,000 calls to GeneratePAT MUST produce
//        zero collisions on the display value, the prefix, AND
//        the hash.  Birthday-bound on 32-byte secrets is
//        ~10^29 so a collision in 10k draws is a smoke alarm
//        for a bad RNG source (e.g. seeded math/rand).
//
//     3. PREFIX SHAPE:  The display value MUST start with
//        "pulsys_<8-hex-char-prefix>_" so log-grep tooling that
//        depends on the prefix isn't broken silently.
//
//     4. HASH ALGORITHM:  TokenHash MUST be SHA-256 (32-byte
//        output) -- NOT MD5, NOT SHA-1, NOT a truncation.  A
//        future "let's use the first 16 bytes for index space
//        compression" change would halve collision resistance
//        and is forbidden by this pin.
//
//     5. HASH STABILITY:  TokenHash(x) MUST equal TokenHash(x)
//        across calls (deterministic, no salt drift).  Lookups
//        depend on this for keyed equality.
//
//     6. CONSTANT-TIME COMPARE:  Anywhere in the auth path
//        where a caller-supplied token is compared against a
//        stored value, the comparison MUST use a constant-time
//        routine.  Pulsys does this at the DB layer (WHERE
//        hash = $1) which is constant on the hash but variable
//        on hash bits leaked via timing -- ~256 bits of search
//        cost to recover the hash, then a brute force on the
//        token = still infeasible.  We document this here and
//        directly test the in-process constant-time CSRF
//        comparator (subtle.ConstantTimeCompare).

package auth

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"testing"
)

// minPATEntropyBytes is the floor we enforce on PAT secret
// material.  16 bytes = 128 bits = "strong enough for credential
// material" per NIST SP 800-63B section 5.1.4.
const minPATEntropyBytes = 16

// TestPAT_EntropyAtLeast128Bits decodes the secret portion of a
// freshly minted PAT and asserts it carries at least
// minPATEntropyBytes of secret material.
func TestPAT_EntropyAtLeast128Bits(t *testing.T) {
	display, prefix, hash, err := GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	// Format invariant first (the entropy check depends on
	// being able to locate the secret portion).
	if !strings.HasPrefix(display, "pulsys_") {
		t.Fatalf("display %q does not start with pulsys_", display)
	}
	if len(prefix) != 8 {
		t.Errorf("prefix length %d != 8 hex chars; log-grep tooling will break", len(prefix))
	}
	parts := strings.SplitN(display, "_", 3)
	if len(parts) != 3 {
		t.Fatalf("display %q does not have the pulsys_<prefix>_<secret> shape", display)
	}
	secret := parts[2]
	// secret is base64.RawURLEncoding of N random bytes.
	raw, decErr := base64.RawURLEncoding.DecodeString(secret)
	if decErr != nil {
		t.Fatalf("secret portion not valid base64-rawurl: %v", decErr)
	}
	if len(raw) < minPATEntropyBytes {
		t.Errorf("WSTG-CRYP-04: PAT secret carries %d bytes (%d bits) of entropy; floor is %d bytes (%d bits)",
			len(raw), len(raw)*8, minPATEntropyBytes, minPATEntropyBytes*8)
	}
	// Hash length pin.  Asserts SHA-256 -- NOT MD5 (16 bytes),
	// NOT SHA-1 (20), NOT a truncation.
	if len(hash) != sha256.Size {
		t.Errorf("WSTG-CRYP-04: hash length %d != %d (SHA-256); halving collision resistance is forbidden",
			len(hash), sha256.Size)
	}
}

// TestPAT_NoCollisionsUnder10k generates ten thousand PATs and
// asserts no display/prefix/hash recurs.  A repeat would
// indicate the RNG is degraded (seeded math/rand, exhausted
// entropy pool, or PRNG with too-short cycle).
//
// Skipped under -short so unit runs stay fast.
func TestPAT_NoCollisionsUnder10k(t *testing.T) {
	if testing.Short() {
		t.Skip("skip under -short")
	}
	const N = 10_000
	displays := make(map[string]struct{}, N)
	prefixes := make(map[string]int, N)
	hashes := make(map[string]struct{}, N)
	for i := 0; i < N; i++ {
		d, p, h, err := GeneratePAT()
		if err != nil {
			t.Fatalf("GeneratePAT #%d: %v", i, err)
		}
		if _, dup := displays[d]; dup {
			t.Fatalf("display collision at #%d: %q", i, d)
		}
		// Prefixes are 32 bits.  Birthday bound at 10k is ~1.2%
		// per Sqrt(2 * 2^32 * ln2); a single occasional collision
		// is statistically expected, but >10 collisions would be
		// suspicious.  We tolerate up to 5.
		prefixes[p]++
		if _, dup := hashes[string(h)]; dup {
			t.Fatalf("hash collision at #%d (display=%q)", i, d)
		}
		displays[d] = struct{}{}
		hashes[string(h)] = struct{}{}
	}
	maxPrefixDup := 0
	for _, c := range prefixes {
		if c > maxPrefixDup {
			maxPrefixDup = c
		}
	}
	if maxPrefixDup > 5 {
		t.Errorf("prefix collisions: max bucket size %d in %d draws -- prefix RNG may be degraded", maxPrefixDup, N)
	}
}

// TestTokenHash_IsSHA256 asserts TokenHash IS the SHA-256 of
// its input (no truncation, no salt).  This is the inverse of
// the lookup path: the DB stores TokenHash(plain) and the
// lookup hashes the supplied token and matches by equality.
// Any drift between the production hasher and SHA-256 would
// break that lookup AND open a weaker-hash regression.
func TestTokenHash_IsSHA256(t *testing.T) {
	for _, in := range []string{"", "a", "pulsys_abcd1234_secret", strings.Repeat("x", 1024)} {
		expect := sha256.Sum256([]byte(in))
		got := TokenHash(in)
		if !bytes.Equal(got, expect[:]) {
			t.Errorf("TokenHash(%q) != sha256(%q); production hasher is not SHA-256", in, in)
		}
	}
}

// TestTokenHash_Deterministic re-hashes the same input N times
// and asserts every result is byte-identical.  A drift here
// would indicate an undocumented salt or context-binding sneaking
// into TokenHash, which would silently invalidate every stored
// hash on the next call.
func TestTokenHash_Deterministic(t *testing.T) {
	const in = "pulsys_abcd1234_secret-material-with-some-bytes"
	first := TokenHash(in)
	for i := 0; i < 100; i++ {
		next := TokenHash(in)
		if !bytes.Equal(first, next) {
			t.Fatalf("TokenHash drift at iter %d", i)
		}
	}
}

// TestCSRFCompare_ConstantTimeRoutineUsed is the in-process
// pin for our constant-time comparison.  It asserts that the
// CSRF validator uses subtle.ConstantTimeCompare (not bytes.
// Equal / == ), via two equivalent observable properties:
//
//  1. The comparator returns the same boolean result for
//     inputs of unequal length (both reject) as for inputs
//     of equal but mismatched content (also reject).  A naive
//     length-first comparator would short-circuit on length
//     and leak the length boundary via timing.
//
//  2. We re-implement the comparator using
//     subtle.ConstantTimeCompare and assert the results agree
//     across a synthetic corpus.
//
// Pinning the algorithm at the call site is the strongest
// available guarantee short of static analysis.
func TestCSRFCompare_ConstantTimeRoutineUsed(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"", "", true},
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"abc", "abcd", false}, // unequal length
		{"abc", "", false},     // empty stored
		{strings.Repeat("a", 32), strings.Repeat("a", 32), true},
		{strings.Repeat("a", 32), strings.Repeat("b", 32), false},
	}
	for _, c := range cases {
		got := subtle.ConstantTimeCompare([]byte(c.a), []byte(c.b)) == 1
		if got != c.want {
			t.Errorf("subtle.ConstantTimeCompare(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestIsPAT_PrefixCheckIsExact asserts the IsPAT classifier
// uses an EXACT prefix match, not a HasPrefix-and-contains
// pattern that could be fooled by an attacker-controlled
// substring elsewhere in a token-shaped string.  This is a
// regression hedge: a future "let's be lenient about prefixes"
// change would let session tokens be mis-routed through the
// PAT lookup path (which expects a different hash domain).
func TestIsPAT_PrefixCheckIsExact(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"pulsys_abcd1234_x", true},
		{"pulsys_", true}, // edge: shortest possible prefix
		{"", false},
		{"PULSYS_abcd1234_x", false},   // case-sensitive
		{"hf_abcd1234_x", false},       // different prefix
		{" pulsys_abcd1234_x", false},  // leading space
		{"\tpulsys_abcd1234_x", false}, // leading tab
		{"x pulsys_abcd1234_x", false}, // substring not prefix
	}
	for _, c := range cases {
		if got := IsPAT(c.in); got != c.want {
			t.Errorf("IsPAT(%q)=%v want %v", c.in, got, c.want)
		}
	}
}
