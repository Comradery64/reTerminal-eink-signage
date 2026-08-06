// Command wiftoken lets the broker keep KEYLESS Google Calendar auth (Workload Identity
// Federation) when it is NOT running in Kubernetes (Tier 1 systemd, Tier 2 compose — see
// docs/DEPLOY-TIERS.md). Inside k3s, the kubelet mints and rotates a short-lived, cluster-signed
// OIDC JWT that WIF exchanges for GCP credentials, and no key material sits on the broker host.
// Outside k3s there is no kubelet playing that role, so this tool plays it instead: a minimal,
// stdlib-only OIDC issuer that signs its own tokens with an RSA key held on the local disk.
//
// This is a real, named trade-off, not a shortcut — see docs/GOOGLE-AUTH.md for the full runbook
// and the honest limitation it introduces: unlike the k3s path, there IS a signing key at rest
// here, and anyone who can read it can mint pool tokens until the WIF provider is deleted.
// Mitigations (0600 root-owned key, disk encryption, a `sub` attribute condition on the WIF
// provider, least-privilege `freeBusyReader` scope, short TTLs, quarterly key rotation) are
// documented there, not enforced here — this tool's job is only to mint and rotate the token file.
//
// Three modes, selected by which flags are set (checked in this order):
//
//	-genkey <path>   Generate an RSA-2048 signing key (PKCS#8 PEM, mode 0600) and exit. The key
//	                 is written straight to disk and never printed, logged, or returned as a
//	                 string, so it can't leak into a shell history or a process listing.
//
//	-jwks            Read -key and print the public JWKS to stdout, then exit. Upload this once
//	                 to GCP via:
//	                   gcloud iam workload-identity-pools providers create-oidc onprem \
//	                     --issuer-uri=<iss> --jwk-json-path=jwks.json \
//	                     --attribute-mapping='google.subject=assertion.sub' \
//	                     --attribute-condition='assertion.sub=="broker"'
//	                 This is the exact uploaded-JWKS pattern the existing k3s WIF provider already
//	                 uses (see docs/BUILD-GUIDE.md Step 3) — no public HTTPS endpoint is required.
//
//	(default) mint   Read -key, sign one RS256 JWT for -iss/-sub/-aud with lifetime -ttl, and
//	                 write it atomically to -out (temp file + rename, same directory, so a reader
//	                 — the Google auth library's external_account credential_source.file — never
//	                 observes a half-written token). If -interval > 0, sleep and repeat forever, so
//	                 the same binary works both as a systemd oneshot (paired with a .timer) and as
//	                 a long-running compose sidecar sharing a token volume.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

func main() {
	genkeyPath := flag.String("genkey", "", "generate an RSA-2048 signing key at this path (PKCS#8 PEM, mode 0600) and exit")
	jwks := flag.Bool("jwks", false, "print the public JWKS for -key to stdout and exit")
	keyPath := flag.String("key", "", "path to the RSA signing key (PKCS#8 PEM) previously written by -genkey")
	out := flag.String("out", "", "path to write the minted token to (atomic temp file + rename)")
	iss := flag.String("iss", "", "issuer URL — must equal the WIF provider's --issuer-uri")
	sub := flag.String("sub", "", "subject — must equal the WIF provider's --attribute-condition (e.g. \"broker\")")
	aud := flag.String("aud", "", "audience — the WIF provider's full resource name")
	ttl := flag.Duration("ttl", time.Hour, "token lifetime")
	interval := flag.Duration("interval", 0,
		"if > 0, re-mint every interval instead of exiting after one token (for a long-running "+
			"sidecar); leave at 0 for a systemd oneshot driven by a .timer")
	flag.Parse()

	switch {
	case *genkeyPath != "":
		if err := genKey(*genkeyPath); err != nil {
			fmt.Fprintln(os.Stderr, "wiftoken: genkey:", err)
			os.Exit(1)
		}

	case *jwks:
		if *keyPath == "" {
			fmt.Fprintln(os.Stderr, "wiftoken: -jwks requires -key")
			os.Exit(2)
		}
		priv, err := loadPrivateKey(*keyPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wiftoken: load key:", err)
			os.Exit(1)
		}
		set, err := jwksFor(&priv.PublicKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wiftoken: jwks:", err)
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(set); err != nil {
			fmt.Fprintln(os.Stderr, "wiftoken: encode jwks:", err)
			os.Exit(1)
		}

	default:
		if *keyPath == "" || *out == "" || *iss == "" || *sub == "" || *aud == "" {
			fmt.Fprintln(os.Stderr, "wiftoken: mint mode requires -key -out -iss -sub -aud")
			os.Exit(2)
		}
		priv, err := loadPrivateKey(*keyPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wiftoken: load key:", err)
			os.Exit(1)
		}
		for {
			if err := mintOnce(priv, *out, *iss, *sub, *aud, *ttl); err != nil {
				fmt.Fprintln(os.Stderr, "wiftoken: mint:", err)
				os.Exit(1)
			}
			if *interval <= 0 {
				return
			}
			time.Sleep(*interval)
		}
	}
}

// genKey writes a fresh RSA-2048 signing key to path as PKCS#8 PEM, mode 0600. It never returns
// the key material to the caller — only os.Chmod/os.OpenFile touch disk — so a bug elsewhere in
// this file (a stray fmt.Println, a debug flag) can't accidentally print a private key: there is
// no code path in this function that ever holds the PEM bytes anywhere but the destination file.
//
// Rotation intentionally does not live here: quarterly rotation (see docs/GOOGLE-AUTH.md) writes
// a *second* key at a different path, publishes a two-key JWKS, cuts traffic over, then removes
// the old key — overwriting this file in place would drop the old key before the new JWKS
// propagates to GCP, breaking any in-flight token exchange signed with it.
func genKey(path string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return f.Sync()
}

// loadPrivateKey reads the PKCS#8 PEM written by genKey. It is the only function that ever
// materializes the key in memory as a Go value; callers pass the *rsa.PrivateKey straight into
// jwksFor/mintOnce and never serialize it back out.
func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS8 key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("key is not RSA")
	}
	return rsaKey, nil
}

// jwk is a single RSA signature-verification key in JWK form (RFC 7517).
type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// kidFor derives a stable key ID from the DER encoding of the public key: GCP's WIF provider
// matches a token's `kid` header against the uploaded JWKS by this value, so the same key must
// always produce the same kid across separate `-jwks` invocations (e.g. after a broker restart),
// and different keys must not collide. SHA-256 of the DER-encoded SubjectPublicKeyInfo gives both.
func kidFor(pub *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// jwksFor builds the single-key JWKS document for pub. Only one key is ever emitted per
// invocation; the quarterly two-key rotation runbook in docs/GOOGLE-AUTH.md concatenates two
// separate `-jwks` runs' `keys` arrays by hand (or with jq) rather than this tool managing
// multiple keys itself — keeping this tool a stateless, single-key mint/verify pair is what makes
// it small enough to be stdlib-only with no config file of its own.
func jwksFor(pub *rsa.PublicKey) (jwkSet, error) {
	kid, err := kidFor(pub)
	if err != nil {
		return jwkSet{}, err
	}
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	return jwkSet{Keys: []jwk{{
		Kty: "RSA",
		Use: "sig",
		Alg: "RS256",
		Kid: kid,
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(eBytes),
	}}}, nil
}

// mintOnce signs one RS256 JWT for iss/sub/aud and writes it atomically to out. RS256 (not ES256)
// is deliberate: it's what every WIF OIDC provider accepts without question and matches what the
// k3s projected-token issuer already emits, so there is nothing provider-side to reconfigure.
func mintOnce(key *rsa.PrivateKey, out, iss, sub, aud string, ttl time.Duration) error {
	kid, err := kidFor(&key.PublicKey)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid}

	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return fmt.Errorf("jti: %w", err)
	}
	claims := map[string]any{
		"iss": iss,
		"sub": sub,
		"aud": aud,
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(ttl).Unix(),
		"jti": base64.RawURLEncoding.EncodeToString(jtiBytes),
	}

	token, err := signJWT(key, header, claims)
	if err != nil {
		return err
	}
	// 0600: this file sits in credential_source.file for an external_account config — same
	// sensitivity as the signing key itself for the token's ~1h lifetime.
	return atomicWrite(out, []byte(token), 0600)
}

func signJWT(key *rsa.PrivateKey, header map[string]string, claims map[string]any) (string, error) {
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// atomicWrite creates <dir>/<base>.tmp-<random> next to path (rename across filesystems is not
// atomic, so the temp file must share path's directory), syncs and closes it, then renames it
// over path. This is the same temp-file+rename+fsync shape internal/config/persist.go uses
// for config writes, for the identical reason: a process reading the token mid-write (the Google
// auth library, on its own schedule, independent of this tool's -interval) must see either the
// whole previous token or the whole new one, never a truncated file.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds; cleans up on any error return

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return os.Rename(tmpPath, path)
}
