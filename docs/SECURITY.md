# Security model

## Threats & mitigations

| Threat | Mitigation |
|---|---|
| Stolen unit → extract corporate Wi-Fi creds / token from flash | **Flash Encryption (RELEASE)** + NVS encryption; download mode disabled in release mode |
| Malicious firmware flashed onto a unit | **Secure Boot V2 (RSA-3072)** — bootloader only runs app signed by the offline key |
| Token from one room replayed against another | Broker binds token → `device_id`; constant-time SHA-256 compare in `auth.go` |
| Token leak server-side | Broker stores only `sha256(token)`, never the raw token |
| Passive network capture of schedules | TLS 1.2/1.3 to the broker; firmware pins an **internal CA** (`broker_ca.pem`); SNI + CN/SAN checked. Also: free/busy carries no meeting titles, so there's little to capture. |
| Static Google key theft | **No key exists.** Auth is keyless via **Workload Identity Federation** — on Tier 3 (k3s), k3s mints a short-lived (1h) projected token exchanged for a short-lived SA token; nothing long-lived at rest. On Tiers 1/2 (no Kubernetes) see the signing-key trade-off below. |
| Compromised broker → calendar exposure (detail_level: "titles") | **Scope widens deliberately.** `calendar.events.readonly` + rooms shared as *reader* means a compromise leaks titles, organizers, and times for every shared room. Opt-in only; see "Meeting detail level" below. |
| Compromised broker → calendar exposure | Scope is **`calendar.freebusy` only**, and the SA is shared on **exactly the 3 room calendars** (freeBusyReader). Worst case leaks busy/free times — never titles, attendees, or any other calendar. No impersonation, no domain-wide delegation. |
| Stolen wiftoken signing key (Tiers 1/2 only) | See "Tier 1/2 signing-key trade-off" below — this is a real, named exception to the "nothing long-lived at rest" property above, not an oversight. |

## Meeting detail level — the one knob that changes this threat model

`google.detail_level` decides what the broker is allowed to read, and therefore what a compromise
can cost. It defaults to `free_busy` and is never widened by an upgrade.

| | `free_busy` (default) | `titles` |
|---|---|---|
| Scope | `calendar.freebusy` | `calendar.events.readonly` |
| Room shared as | `freeBusyReader` | `reader` |
| Panel shows | "Busy" | the meeting title |
| Worst case on full compromise | busy/free times only | titles, organizers, times, for every shared room |

The default is not merely a redaction — at `free_busy` a title is never fetched, so it cannot be
leaked by a broker that is fully owned. That is a stronger property than hiding it at render time,
and it is why the narrow option stays the default.

Two things to weigh before enabling `titles`:

1. **A wall-mounted panel is a public surface.** A corridor display showing real titles is readable
   by everyone who walks past, including visitors and contractors. Events marked private or
   confidential render as "Private meeting", but that only protects meetings someone remembered to
   mark.
2. **Scope is bound at startup**, so changing this needs a broker restart. It is deliberately not
   settable from `/admin`: silently widening an OAuth scope from a web form is not something a
   click should do.

If `titles` is set but a room is still shared as `freeBusyReader`, the events call is refused and
the broker logs an ERROR and falls back to free/busy for the rest of the process — panels keep
working and show "Busy". Fix the sharing, then restart to re-arm.

## Credential locations

- **Device**: Wi-Fi PSK/EAP creds + bearer token → encrypted NVS `prov` namespace only.
- **Broker → Google**: **no key** — keyless WIF. The external-account *cred config* (a ConfigMap on
  Tier 3, a plain file on Tiers 1/2 — no secret material either way) tells the auth lib how to
  exchange a short-lived token for GCP credentials; the token itself is minted on demand (by the
  kubelet on Tier 3, by `wiftoken` on Tiers 1/2 — see below) and expires hourly.
- **Broker → fleet**: device token *hashes* → ConfigMap (Tier 3) or config file (Tiers 1/2). Slack
  webhook → k8s Secret (Tier 3) or an env file (Tiers 1/2, `broker.env`/`.env`, mode `0600`). TLS
  leaf → `broker-tls` Secret (Tier 3) or a file the reverse proxy reads (Tiers 1/2).
- **Signing key** (Secure Boot): offline / HSM, never deployed.

## Tier 1/2 signing-key trade-off

Tiers 1/2 (no Kubernetes — see `docs/DEPLOY-TIERS.md`) have no kubelet to mint the short-lived OIDC
token WIF exchanges for GCP credentials, so the broker host mints its own via
`backend/tools/wiftoken`. Full runbook, threat model, and rotation procedure: **`docs/GOOGLE-AUTH.md`**.
Stated plainly here too, because it's a genuine exception to the "no key exists" row above, not an
oversight: **Tiers 1/2 have an RSA signing key at rest that Tier 3 does not.** It is never a Google
credential and never leaves the host, but anyone who can read it can mint WIF pool tokens until the
provider is deleted. Mitigations: `0600` root-owned key file, full-disk encryption on the host, the
WIF provider's `attribute-condition` pinning the token subject, `freeBusyReader` scoped to specific
rooms only (unchanged from Tier 3), a 1h token TTL, and a documented quarterly two-key rotation
runbook. This raises the host's blast radius but doesn't create a new trust tier — the host already
holds device token hashes, user password hashes, and the session secret, all covered below.

## Persisted config never bakes in expanded secrets

`session_secret` and `alert_webhook_url` (and any other `${VAR}`-referenced value) are expanded in
memory when the broker loads its config, but a `/admin` or `/manager` save writes back the
**`${VAR}` reference**, not the expanded plaintext — whether the destination is a Tier 3 ConfigMap
or a Tier 1/2 file. Marshalling the expanded value on write-back would otherwise copy a live secret
out of its Secret/env-file and into a ConfigMap or (worse, since it's more likely to be
git-tracked) a plain config file on the very first admin save after this feature shipped. The
env-file/Secret is still the only place the real secret value lives at rest.

## Operational notes

- Rotate a device token by re-provisioning NVS and updating its `token_sha256` in the broker config
  (no firmware rebuild). See `firmware/secure/README.md`.
- Serve the broker behind Traefik (Tier 3) or Caddy (Tiers 1/2) with an internal CA cert the
  firmware's bundle trusts; consider mTLS (client cert per device) as a future hardening step if
  the threat model warrants it.
- Telemetry endpoint is authenticated with the same per-device token, so a unit can't spoof
  another's battery/health metrics.
