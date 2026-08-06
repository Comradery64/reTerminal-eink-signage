package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGenKeyMintJWKSRoundTrip pins the whole chain an operator relies on: -genkey produces a key
// wiftoken can load, -jwks describes that key faithfully enough for GCP's WIF provider to verify
// tokens minted from it, and mint mode actually produces a JWT whose claims and signature match.
func TestGenKeyMintJWKSRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "signing.key")

	if err := genKey(keyPath); err != nil {
		t.Fatalf("genKey: %v", err)
	}

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 0600", perm)
	}

	priv, err := loadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	if priv.N.BitLen() < 2048 {
		t.Fatalf("key size = %d bits, want >= 2048", priv.N.BitLen())
	}

	set, err := jwksFor(&priv.PublicKey)
	if err != nil {
		t.Fatalf("jwksFor: %v", err)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("jwks has %d keys, want 1", len(set.Keys))
	}

	// kid must be stable across repeated -jwks invocations against the same key — GCP matches a
	// token's kid header to an entry in the uploaded JWKS, so a non-deterministic kid would break
	// verification the moment the tool is re-run.
	set2, err := jwksFor(&priv.PublicKey)
	if err != nil {
		t.Fatalf("jwksFor (2nd call): %v", err)
	}
	if set.Keys[0].Kid != set2.Keys[0].Kid {
		t.Fatalf("kid is not stable across calls: %q != %q", set.Keys[0].Kid, set2.Keys[0].Kid)
	}

	outPath := filepath.Join(dir, "token")
	const (
		iss = "https://displays.example.internal/oidc"
		sub = "broker"
		aud = "//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/displays-pool/providers/onprem"
	)
	ttl := time.Hour

	if err := mintOnce(priv, outPath, iss, sub, aud, ttl); err != nil {
		t.Fatalf("mintOnce: %v", err)
	}

	tokInfo, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if perm := tokInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file mode = %o, want 0600", perm)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	parts := strings.Split(string(raw), ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3 (header.claims.signature)", len(parts))
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header segment: %v", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Alg != "RS256" {
		t.Errorf("header.alg = %q, want RS256", header.Alg)
	}
	if header.Typ != "JWT" {
		t.Errorf("header.typ = %q, want JWT", header.Typ)
	}
	if header.Kid != set.Keys[0].Kid {
		t.Errorf("header.kid = %q, jwks kid = %q, want match", header.Kid, set.Keys[0].Kid)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims segment: %v", err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Aud string `json:"aud"`
		Iat int64  `json:"iat"`
		Nbf int64  `json:"nbf"`
		Exp int64  `json:"exp"`
		Jti string `json:"jti"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != iss {
		t.Errorf("claims.iss = %q, want %q", claims.Iss, iss)
	}
	if claims.Sub != sub {
		t.Errorf("claims.sub = %q, want %q", claims.Sub, sub)
	}
	if claims.Aud != aud {
		t.Errorf("claims.aud = %q, want %q", claims.Aud, aud)
	}
	if claims.Jti == "" {
		t.Errorf("claims.jti is empty, want a random per-mint value")
	}
	if claims.Exp-claims.Iat != int64(ttl.Seconds()) {
		t.Errorf("exp-iat = %ds, want %ds (the requested ttl)", claims.Exp-claims.Iat, int64(ttl.Seconds()))
	}
	if claims.Nbf > claims.Iat {
		t.Errorf("nbf (%d) is after iat (%d)", claims.Nbf, claims.Iat)
	}

	// The check that actually matters: reconstruct the public key purely from the JWKS output
	// (n/e, base64url per RFC 7518) and verify the signature against it — this mirrors exactly
	// what GCP's WIF provider does with the uploaded jwks.json, so if this fails, the JWKS this
	// tool prints is useless in production even if the token's claims look right.
	nBytes, err := base64.RawURLEncoding.DecodeString(set.Keys[0].N)
	if err != nil {
		t.Fatalf("decode jwks n: %v", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(set.Keys[0].E)
	if err != nil {
		t.Fatalf("decode jwks e: %v", err)
	}
	eInt := 0
	for _, b := range eBytes {
		eInt = eInt<<8 | int(b)
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: eInt}

	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature segment: %v", err)
	}
	sum := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify against the JWKS-derived public key: %v", err)
	}
}

// TestGenKeyNeverPrintsKeyMaterial guards the one property the doc comments promise: -genkey must
// not put the private key anywhere but the destination file, including stdout, since anything
// printed can end up in a terminal scrollback or a captured log.
func TestGenKeyNeverPrintsKeyMaterial(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "signing.key")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	genErr := genKey(keyPath)
	w.Close()
	os.Stdout = origStdout
	if genErr != nil {
		t.Fatalf("genKey: %v", genErr)
	}

	captured, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if len(captured) != 0 {
		t.Fatalf("genKey wrote %d bytes to stdout, want none", len(captured))
	}
}

// TestMintOnceOverwritesAtomically pins that a second mint (simulating the -interval re-mint
// loop) replaces the token file wholesale via rename, never truncates it in place — a reader
// racing the writer must always see a complete, parseable token.
func TestMintOnceOverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "signing.key")
	if err := genKey(keyPath); err != nil {
		t.Fatalf("genKey: %v", err)
	}
	priv, err := loadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}

	outPath := filepath.Join(dir, "token")
	if err := mintOnce(priv, outPath, "iss", "broker", "aud", time.Hour); err != nil {
		t.Fatalf("mintOnce (1st): %v", err)
	}
	first, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read 1st token: %v", err)
	}

	if err := mintOnce(priv, outPath, "iss", "broker", "aud", 2*time.Hour); err != nil {
		t.Fatalf("mintOnce (2nd): %v", err)
	}
	second, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read 2nd token: %v", err)
	}

	if string(first) == string(second) {
		t.Fatalf("2nd mint (different ttl) produced an identical token — jti/exp should differ")
	}
	if len(strings.Split(string(second), ".")) != 3 {
		t.Fatalf("2nd token is not well-formed after overwrite")
	}

	// No leftover temp files: atomicWrite must clean up after itself on the success path.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file after successful mint: %s", e.Name())
		}
	}
}
