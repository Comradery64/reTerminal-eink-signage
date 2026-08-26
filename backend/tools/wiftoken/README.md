# wiftoken

A stdlib-only Go tool (`crypto/rsa`, `crypto/rand`, `crypto/sha256`, `crypto/x509`, `encoding/*`) that
lets a broker running **outside Kubernetes** (Tier 1 systemd, Tier 2 compose — see
[../../../docs/DEPLOY-TIERS.md](../../../docs/DEPLOY-TIERS.md)) keep **keyless** Google Calendar auth
via Workload Identity Federation. It ships in the same image/binary drop as the broker.

Full runbook, threat model, and rotation procedure:
[../../../docs/GOOGLE-AUTH.md](../../../docs/GOOGLE-AUTH.md). This file is the command reference.

## Why this exists

Inside k3s, the kubelet mints and rotates a short-lived, cluster-signed OIDC JWT that WIF exchanges
for GCP credentials — no key material ever sits on the broker's disk. Outside k3s there's no
kubelet to play that role. `wiftoken` is a minimal OIDC issuer that plays it instead: it holds an
RSA key locally and signs its own tokens. **This means Tiers 1/2 have a signing key at rest that
Tier 3 does not** — see docs/GOOGLE-AUTH.md for why that's an acceptable, documented trade rather
than a hidden regression.

## Usage

```
wiftoken -genkey <path>                         # one-time: generate the signing key
wiftoken -jwks -key <path>                       # print the public JWKS to upload to GCP
wiftoken -key <path> -out <path> -iss <url> -sub <sub> -aud <aud> [-ttl 1h] [-interval 30m]
```

| Flag | Meaning |
|---|---|
| `-genkey <path>` | Generate an RSA-2048 signing key (PKCS#8 PEM, mode `0600`) at `<path>` and exit. Never prints the key. |
| `-jwks` | Read `-key` and print the public JWKS (RFC 7517) to stdout, then exit. |
| `-key <path>` | Path to the signing key written by `-genkey`. Required for `-jwks` and mint mode. |
| `-out <path>` | Where to write the minted token (mint mode only). Written atomically (temp file + rename in the same directory), mode `0600`. |
| `-iss <url>` | Issuer URL. Must equal the WIF provider's `--issuer-uri`. |
| `-sub <string>` | Subject. Must equal the WIF provider's `--attribute-condition` (e.g. `broker`). |
| `-aud <string>` | Audience. The WIF provider's full resource name. |
| `-ttl <duration>` | Token lifetime. Default `1h`. |
| `-interval <duration>` | If `> 0`, re-mint every interval instead of exiting after one token — for a long-running sidecar. Leave at `0` (default) for a systemd oneshot driven by a `.timer`. |

## One-time setup

```bash
wiftoken -genkey /etc/meeting-displays/gcp/wiftoken.key
wiftoken -jwks -key /etc/meeting-displays/gcp/wiftoken.key > jwks.json

gcloud iam workload-identity-pools providers create-oidc onprem \
  --location=global --workload-identity-pool=displays-pool \
  --issuer-uri=https://displays.example.internal/oidc \
  --jwk-json-path=jwks.json \
  --attribute-mapping='google.subject=assertion.sub' \
  --attribute-condition='assertion.sub=="broker"'
```

No public HTTPS endpoint is required — this is the same uploaded-JWKS pattern the existing k3s WIF
provider already uses (`gcloud ... providers create-oidc k3s --jwk-json-path=k3s-jwks.json ...`,
see `docs/BUILD-GUIDE.md` Step 3). GCP verifies tokens against the uploaded key set; it never calls
back to the issuer URL for discovery.

## Running it continuously

Tier 1 (systemd): a oneshot `.service` + `.timer` (`../systemd/wiftoken.service`,
`../systemd/wiftoken.timer`) invoke `wiftoken` without `-interval` every 30 minutes.

Tier 2 (compose): the `wiftoken` service in `../compose/docker-compose.yml` (profile `google`) runs
`wiftoken` with `-interval 30m` as a long-running sidecar sharing a token volume with the broker.

Either way, point the broker's `google.credentials_file` at an `external_account` credential config
whose `credential_source.file` is `-out`'s path — **zero broker code changes**, same as the k3s path.

## Testing

`main_test.go` covers the full local chain: `-genkey` produces a loadable key with mode `0600`,
`-jwks` is deterministic for a given key, a minted token's three segments decode, its claims match
the flags used to mint it, its signature verifies against the public key reconstructed purely from
the JWKS output (the same reconstruction GCP performs), the output file is mode `0600`, and a
second mint (simulating the `-interval` loop) replaces the file atomically with no leftover temp
file. It does **not** test against a real GCP WIF provider — that's an integration/runbook concern,
documented in `docs/GOOGLE-AUTH.md`, not a unit test.
