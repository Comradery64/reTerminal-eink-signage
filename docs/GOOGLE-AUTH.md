# Google Calendar auth, per deploy tier

The broker reads room availability via Calendar's `freeBusy` API, authenticated **keyless**
(Workload Identity Federation, no service-account key) everywhere — see `docs/SECURITY.md` for the
threat model. What differs across tiers (`docs/DEPLOY-TIERS.md`) is *who mints the short-lived OIDC
token WIF exchanges*.

## Tier 3 (k3s) — unchanged

The kubelet mints and rotates a projected ServiceAccount token (`expirationSeconds: 3600`,
refreshed automatically well before expiry) and mounts it as a file the broker reads. WIF's
`external_account` credential exchanges that token for a short-lived GCP access token. **Nothing
sits at rest on the broker host or pod** — the signing key lives in the cluster's own OIDC issuer,
which the broker never sees. See `backend/deploy/k3s/broker.yaml.example`'s `gcp-token` volume and
`docs/BUILD-GUIDE.md` Step 3 for the one-time provider setup.

## Tiers 1/2 (no Kubernetes) — the host becomes its own OIDC issuer

**The constraint stack:** WIF's `external_account` credential needs a periodically refreshed OIDC
JWT in a file whose `aud` matches the provider. Today the kubelet mints and rotates that file.
Tiers 1/2 have no kubelet. And the org sets `iam.disableServiceAccountKeyCreation` *and*
`iam.disableServiceAccountKeyUpload` (see `docs/BUILD-GUIDE.md` §2.2) — service-account keys are
**foreclosed**, not merely discouraged, so "just use a key" isn't on the table.

**The recommended answer: make the broker host its own OIDC issuer, and keep WIF.**
[`backend/tools/wiftoken`](../backend/tools/wiftoken/README.md) — a stdlib-only Go tool, built into
the same image/binary drop as the broker — has three modes:

- `-genkey <path>` — write an RSA-2048 signing key (PKCS#8 PEM, mode `0600`) and exit. Never prints it.
- `-jwks -key <path>` — print the public JWKS to upload to GCP.
- (default) mint mode — sign and atomically write a token on a loop.

### Exact runbook

```bash
# 1) Generate the signing key once. Never leaves this host.
wiftoken -genkey /etc/meeting-displays/gcp/wiftoken.key

# 2) Print and upload the public JWKS. This is the SAME uploaded-JWKS pattern the existing k3s WIF
#    provider already uses (see docs/BUILD-GUIDE.md Step 3) — no public HTTPS endpoint required;
#    GCP verifies tokens against this uploaded key set, it never calls back to -iss for discovery.
wiftoken -jwks -key /etc/meeting-displays/gcp/wiftoken.key > jwks.json

gcloud iam workload-identity-pools providers create-oidc onprem \
  --location=global --workload-identity-pool=displays-pool \
  --issuer-uri=https://displays.example.internal/oidc \
  --jwk-json-path=jwks.json \
  --attribute-mapping='google.subject=assertion.sub' \
  --attribute-condition='assertion.sub=="broker"'

gcloud iam service-accounts add-iam-policy-binding rooms-broker@<project>.iam.gserviceaccount.com \
  --role=roles/iam.workloadIdentityUser \
  --member="principal://iam.googleapis.com/projects/<project-number>/locations/global/workloadIdentityPools/displays-pool/subject/broker"

# 3) external_account cred config — credential_source.file points at wiftoken's -out path.
cat > /etc/meeting-displays/gcp/cred-config.json <<'JSON'
{
  "type": "external_account",
  "audience": "//iam.googleapis.com/projects/<project-number>/locations/global/workloadIdentityPools/displays-pool/providers/onprem",
  "subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
  "token_url": "https://sts.googleapis.com/v1/token",
  "service_account_impersonation_url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/rooms-broker@<project>.iam.gserviceaccount.com:generateAccessToken",
  "credential_source": { "file": "/var/lib/meeting-displays/gcp/token", "format": { "type": "text" } }
}
JSON

# 4) config.yaml: point google.credentials_file at the cred config. ZERO broker code changes —
#    this is the exact same code path Tier 3 already uses.
#    google: { credentials_file: "/etc/meeting-displays/gcp/cred-config.json" }

# 5) Run the minter continuously — Tier 1: wiftoken.service + wiftoken.timer (OnUnitActiveSec=30min).
#    Tier 2: the `wiftoken` compose service under the `google` profile.
```

`credentials_file` never changes based on tier — the broker's Google auth code has no idea whether
the token file behind it is kubelet-projected or `wiftoken`-minted. That symmetry is the whole
point: this is a deployment-time choice, not a code branch.

### The honest limitation — stated plainly, not softened

**Tier 3 has no key material at rest.** The kubelet mints tokens from cluster-held keys the broker
never sees. **Tiers 1/2 do**: a signing key sits on the broker host. It is not a Google credential
and it never leaves the host, but **anyone who can read that file can mint pool tokens for as long
as the WIF provider exists** — deleting the provider (or narrowing its `attribute-condition`) is
the actual kill switch, not rotating anything on the broker side alone.

Mitigations, all of which you should actually do, not just read about:

- **`0600`, root-owned key file.** `wiftoken -genkey` already writes mode `0600`; keep the parent
  directory root-owned too (the systemd/compose artifacts here do this by construction).
- **Full-disk encryption** on the host. The signing key is one more reason this machine — which
  already holds device token hashes, user password hashes, and the session secret (see
  `docs/SECURITY.md`) — deserves it if it doesn't have it already.
- **The `attribute-condition` pinning `assertion.sub=="broker"`.** A stolen key can only mint
  tokens that satisfy this condition; it can't be used to impersonate an arbitrary WIF subject.
- **`freeBusyReader` on specific room calendars only** — unchanged from Tier 3's scope. A
  compromised token still can't read titles, attendees, or any calendar not explicitly shared.
- **Short TTL (1h default).** A leaked, unrevoked token is only useful for the remainder of its
  hour — routine key-file exfiltration doesn't buy standing access, it buys at most one hour per
  successful re-read of the file.
- **Quarterly two-key rotation** (below).

This raises the host's blast radius but does not create a new trust tier: the host was already the
most sensitive machine in the system.

### Rotation runbook (quarterly, or on any suspected exposure — never "wait and see")

```bash
# 1) Generate a second key at a different path — do NOT overwrite the current one yet.
wiftoken -genkey /etc/meeting-displays/gcp/wiftoken-2.key

# 2) Publish a two-key JWKS (both keys' entries in one `keys` array) to GCP.
wiftoken -jwks -key /etc/meeting-displays/gcp/wiftoken.key   > old.json
wiftoken -jwks -key /etc/meeting-displays/gcp/wiftoken-2.key > new.json
jq -s '{keys: (.[0].keys + .[1].keys)}' old.json new.json > jwks.json
gcloud iam workload-identity-pools providers update-oidc onprem \
  --location=global --workload-identity-pool=displays-pool --jwk-json-path=jwks.json

# 3) Cut the minter over to the new key (edit the .service/compose command's -key path), restart.

# 4) Once you've confirmed Calendar fetches still succeed on the new key, drop the old one from
#    the JWKS (re-run step 2 with only the new key) and delete the old key file.
```

## Ruled out, with reasons — so nobody re-litigates this

- **Service-account keys.** Org policy blocks both create *and* upload
  (`iam.disableServiceAccountKeyCreation` / `-KeyUpload`) — this isn't a preference to override,
  it's a hard org control. Even without the policy, it's a long-lived Google credential at rest,
  which is exactly the risk keyless WIF exists to avoid.
- **OAuth user refresh tokens.** Also a long-lived Google credential at rest, and additionally tied
  to a specific human's consent and offboarding lifecycle — if that person leaves the org, the
  broker's Calendar access breaks (or worse, keeps working on a revoked-in-spirit grant). That's a
  strict regression on the "nothing long-lived, nothing tied to a human" property `docs/SECURITY.md`
  already claims for this system.
- **Published ICS feed URLs.** Genuinely zero-infrastructure and tempting — no auth at all, just a
  URL. Rejected on **freshness, not security**: Google's published-ICS cache lag is commonly hours,
  and a room display that's hours stale fails the product's entire core promise (real-time
  availability). The whole system exists to answer "is this room free *right now*"; an
  hours-stale answer is worse than no display.
- **GCE/Cloud Run with ADC via the metadata server.** This genuinely works today with
  `credentials_file: ""` and zero new code — the Google auth library already falls back to
  Application Default Credentials, and a GCE/Cloud Run instance's attached service account is
  exchanged automatically via the metadata server, no WIF config at all. Recommend this as
  **"Tier 2-on-GCP"** if your org already has GCP compute — it's arguably the simplest option of
  all *for that specific org*. The limitation is exactly that qualifier: it requires GCP compute,
  which defeats Tier 1/2's whole premise of "any spare office machine, no cloud account required."

## The honest floor

If an organization will not run the minter (`wiftoken`) and has no GCP compute to fall back to,
Tiers 1/2 still support `provider: "demo"` — a built-in fake schedule, useful for evaluation,
local development, and hardware bring-up (see `backend/config.demo.yaml`). Real Google Calendar
integration on Tiers 1/2 requires either the minter or GCP compute; there is no third, keyless,
zero-infrastructure option, and this doc will not pretend otherwise.
