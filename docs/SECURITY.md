# Security model

## Threats & mitigations

| Threat | Mitigation |
|---|---|
| Stolen unit → extract corporate Wi-Fi creds / token from flash | **Flash Encryption (RELEASE)** + NVS encryption; download mode disabled in release mode |
| Malicious firmware flashed onto a unit | **Secure Boot V2 (RSA-3072)** — bootloader only runs app signed by the offline key |
| Token from one room replayed against another | Broker binds token → `device_id`; constant-time SHA-256 compare in `auth.go` |
| Token leak server-side | Broker stores only `sha256(token)`, never the raw token |
| Passive network capture of schedules | TLS 1.2/1.3 to the broker; firmware pins an **internal CA** (`broker_ca.pem`); SNI + CN/SAN checked. Also: free/busy carries no meeting titles, so there's little to capture. |
| Static Google key theft | **No key exists.** Auth is keyless via **Workload Identity Federation** — k3s mints a short-lived (1h) projected token exchanged for a short-lived SA token. Nothing long-lived at rest. |
| Compromised broker → calendar exposure | Scope is **`calendar.freebusy` only**, and the SA is shared on **exactly the 3 room calendars** (freeBusyReader). Worst case leaks busy/free times — never titles, attendees, or any other calendar. No impersonation, no domain-wide delegation. |

## Credential locations

- **Device**: Wi-Fi PSK/EAP creds + bearer token → encrypted NVS `prov` namespace only.
- **Broker → Google**: **no key** — keyless WIF. The external-account *cred config* (a ConfigMap, no
  secret material) tells the auth lib how to exchange the projected k8s token; the token itself is
  minted on demand by k8s and expires hourly.
- **Broker → fleet**: device token *hashes* → ConfigMap. Slack webhook → k8s Secret. TLS leaf →
  `broker-tls` Secret.
- **Signing key** (Secure Boot): offline / HSM, never deployed.

## Operational notes

- Rotate a device token by re-provisioning NVS and updating its `token_sha256` in the broker config
  (no firmware rebuild). See `firmware/secure/README.md`.
- Serve the broker behind Traefik with an internal CA cert the firmware's bundle trusts; consider
  mTLS (client cert per device) as a future hardening step if the threat model warrants it.
- Telemetry endpoint is authenticated with the same per-device token, so a unit can't spoof
  another's battery/health metrics.
