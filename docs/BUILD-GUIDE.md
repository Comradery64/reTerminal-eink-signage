# Building a Battery E-Paper Meeting-Room Display Fleet — the real story + a no-AI guide

This is the honest write-up of how we built a 12-unit conference-room signage system
(Seeed reTerminal E1002 + a Go broker on k3s, reading Google Workspace room calendars), the
dead-ends we hit, what we learned, and a clean step-by-step guide so anyone can reproduce it
without a model holding their hand.

---

## Part 1 — What we ended up with

- **Devices:** 12× Seeed reTerminal E1002 (ESP32-S3 + 7.3" Spectra-6 color e-paper). One per room
  (11 rooms + 1 spare). ESP-IDF C++ firmware: deep-sleep 99.9% of the time, wake every 10 min, fetch,
  repaint **only if the picture changed**, sleep. Target: 3-month battery.
- **Broker:** a single Go binary on the existing k3s cluster. It polls each room's calendar, renders
  a Spectra-6 framebuffer, PackBits-compresses it, and serves it over HTTPS. It computes a content
  hash so unchanged rooms get an HTTP `304` and the device never powers the (energy-expensive) panel.
- **Calendar access:** **free/busy only**, **keyless**. The broker authenticates via **Workload
  Identity Federation** (no service-account key) and reads each room's busy/free blocks (never titles).
- **TLS:** devices pin an **internal CA** baked into the firmware.

The single most important design decision: **a panel refresh costs ~25 s of active current and
dwarfs everything else, so the whole system is built to avoid refreshing unless pixels changed.**
That one idea (server content-hash + HTTP 304) roughly triples battery life.

---

## Part 2 — The journey (and the walls we hit)

### 2.1 Hardware: trust the schematic, not the wiki
We first wrote the firmware against *plausible* pin numbers and a *similar* panel. Then we read the
**official schematic** and the ESP32-S3 datasheet, and three "facts" flipped:
- Battery sense is **GPIO1** behind an enable gate **VBAT_EN (GPIO21)** — the wiki said GPIO2 (which
  is just a header ADC). Reading GPIO2 would have returned garbage.
- The panel is **GDEP073E01 / ED2208** — we ported the *exact* init sequence from `Seeed_GxEPD2` and
  caught a missing `0x83` full-window command before refresh.
- "Tap to wake" via the touch layer is **impossible from deep sleep** — touch is on GPIO47/48, which
  aren't RTC-domain pins. The physical buttons (GPIO3/4/5) are the only wake source.

**Lesson:** for hardware, the schematic is ground truth. Wikis drift.

### 2.2 The calendar-auth odyssey (the big one)
This took four pivots, each forced by an org security control:

1. **Plan A — service-account key + domain-wide delegation (the "normal" way).** Blocked: the org
   enforces `iam.disableServiceAccountKeyCreation`. No SA keys, period.
2. **Plan B — keyless via Workload Identity Federation.** This is actually *better* and matches a
   zero-trust posture (no static secret at rest). k3s mints a short-lived token; GCP exchanges it for
   short-lived SA credentials. Because the k3s OIDC issuer is on-prem (not publicly reachable), the
   WIF provider uses the **uploaded-JWKS** variant, not issuer discovery.
3. **Drop domain-wide delegation entirely.** For 11 rooms, DWD is overkill and keyless-DWD needs an
   `iam.signJwt` impersonation dance. Instead we **share each room calendar directly** with the SA as
   `freeBusyReader`. The SA reads as itself, scope `calendar.freebusy` only — it can never see titles,
   attendees, or any calendar it wasn't explicitly shared.
4. **GAM to enumerate + share the rooms.** Which led to its own saga (below).

**Lesson:** in a locked-down Workspace/GCP org, *let the security policy push you toward the more
secure design.* Keyless + least-scope + per-resource sharing is both what the policy allows and what
you actually want.

### 2.3 The GAM-on-a-VM detour that died on billing
GAM (Google Apps Manager) is the right tool for admin tasks like listing room resources and sharing
calendars. The "secure" pattern is to run it on a locked-down GCE VM. We tried — and hit a wall:
**the project had no billing account, and the account couldn't see one**, so Compute Engine (even the
free e2-micro) wouldn't start. Pivot: **Google Cloud Shell** — a free, Google-managed Linux box, no
billing, no VM. GAM installs there in one command and `$HOME` persists. For occasional admin work
it's strictly better than a billed VM.

**Lesson:** don't stand up a billed VM for a one-off admin task. Cloud Shell is free and sufficient.

### 2.4 GAM's setup is an unavoidable human ceremony
Even on Cloud Shell, authorizing GAM required: trusting a client ID in the Admin console, an OAuth
consent, and — because the org *also* blocks `iam.disableServiceAccountKeyUpload` — GAM's
service-account step failed. That last failure turned out to be **irrelevant**: listing/sharing rooms
uses GAM's *user* OAuth (acting as the admin), not the service account. We finished with user OAuth and
ignored the SA path.

**Lesson:** know which credential a tool actually uses for the operation you need. We chased an
SA-key error that didn't matter.

### 2.5 Browser automation is a last resort, not a first one
We tried driving the GCP/Admin consoles with a headless browser. The consoles are heavy dynamic SPAs;
every click was a fight (custom elements, hidden duplicates, step-up auth challenges). Where a CLI
existed (`gcloud`, `gam` via `gcloud cloud-shell ssh`), it was an order of magnitude more reliable.

**Lesson:** prefer CLIs/APIs. Reserve browser automation for the few genuinely click-only steps
(OAuth consent, "mark app trusted"), and let a human do the credential entry.

---

## Part 3 — Distilled learnings

1. **Optimize the dominant cost.** On color e-paper that's the refresh; design to skip it (content
   hash + 304). Everything else is secondary.
2. **Schematic > wiki** for pins/peripherals.
3. **Let org security policy steer the architecture** — it pushed us to keyless WIF + free/busy +
   per-room sharing, which is the design you'd want anyway.
4. **Free/busy is enough** for availability signage, and it's the minimum-blast-radius scope.
5. **Cloud Shell** beats a VM for one-off admin tooling (free, no billing).
6. **Match the credential to the task** (GAM user-OAuth vs. SA).
7. **CLI over browser automation.**
8. **Never put a static secret at rest** if the platform offers keyless (WIF). The org that blocks SA
   keys is doing you a favor.

---

## Part 4 — The full guide (no AI required)

Reproduces the whole system. Assumes: a Google Workspace org (super-admin access), a k3s cluster,
`kubectl`, `docker`, Go ≥ 1.22, and ESP-IDF ≥ 5.2. Repo layout and deep detail live in
`README.md`, `docs/RUNBOOK.md`, `docs/DEPLOY.md`, `docs/HARDWARE.md`, `docs/POWER.md`,
`docs/SECURITY.md`. This section is the linear path.

### Step 0 — Backend, locally (no cloud)
```bash
cd backend
go build ./... && go test ./...
go run ./cmd/preview                                   # writes preview-*.png — see the layouts
./broker -demo -config config.demo.yaml                # runs with a fake schedule
# other shell: smoke-test the device API + battery alert
TOKEN=demo-token ../tools/fake_device.sh display
TOKEN=demo-token ../tools/fake_device.sh battery-demo
```

### Step 1 — GCP project + the broker's service account (keyless; NO key)
Install gcloud, `gcloud auth login`. Then create a project + SA (the key creation is *expected to
fail* under `iam.disableServiceAccountKeyCreation` — that's fine, WIF needs no key):
```bash
gcloud projects create <broker-project> --name="Meeting Displays"
gcloud config set project <broker-project>
gcloud services enable calendar-json.googleapis.com iamcredentials.googleapis.com sts.googleapis.com
gcloud iam service-accounts create rooms-broker --display-name="Meeting Displays broker"
# SA email: rooms-broker@<broker-project>.iam.gserviceaccount.com
```

### Step 2 — Set up GAM in Cloud Shell (free) to discover + share rooms
In Cloud Shell (shell.cloud.google.com):
```bash
bash <(curl -s -S -L https://gam-shortn.appspot.com/gam-install) -l
gam create project           # creates a GAM project; if your org blocks third-party apps, it walks
                             # you through trusting a client ID + creating a Desktop OAuth client
gam oauth create             # admin consent; creates ~/.gam/oauth2.txt
                             # (ignore any service-account key-UPLOAD failure — not needed)
```
Discover the real rooms and share each with the broker SA as free/busy reader:
```bash
gam print resources fields resourceId,resourceEmail,resourceName,resourceCategory,capacity
# for each CONFERENCE_ROOM resourceEmail (c_…@resource.calendar.google.com):
gam calendar <resourceEmail> add freebusy user rooms-broker@<broker-project>.iam.gserviceaccount.com
```
(Repo helper: put the rooms in `tools/rooms.csv`; `tools/share_rooms.sh` does the discovery+share loop.)

### Step 3 — Keyless auth (Workload Identity Federation), uploaded-JWKS variant
Because k3s's OIDC issuer is on-prem:
```bash
ISSUER=$(kubectl get --raw /.well-known/openid-configuration | jq -r .issuer)
kubectl get --raw /openid/v1/jwks > k3s-jwks.json
PNUM=$(gcloud projects describe <broker-project> --format='value(projectNumber)')
SA=rooms-broker@<broker-project>.iam.gserviceaccount.com

gcloud iam workload-identity-pools create displays-pool --location=global --display-name="Displays"
gcloud iam workload-identity-pools providers create-oidc k3s \
  --location=global --workload-identity-pool=displays-pool \
  --issuer-uri="$ISSUER" --jwk-json-path=k3s-jwks.json \
  --attribute-mapping="google.subject=assertion.sub" \
  --allowed-audiences="//iam.googleapis.com/projects/$PNUM/locations/global/workloadIdentityPools/displays-pool/providers/k3s"
gcloud iam service-accounts add-iam-policy-binding "$SA" \
  --role=roles/iam.workloadIdentityUser \
  --member="principal://iam.googleapis.com/projects/$PNUM/locations/global/workloadIdentityPools/displays-pool/subject/system:serviceaccount:meeting-displays:default"
```
Put `POOL_ID=displays-pool`, `PROVIDER_ID=k3s`, the project number, and the SA email into
`backend/deploy/k3s/broker.yaml` (the `gcp-token` projected-token audience **and** the
`broker-gcp-credconfig` external-account ConfigMap). No key file anywhere.

### Step 4 — Device tokens, image, TLS, deploy
```bash
# per-device bearer tokens (broker stores only the SHA-256):
./tools/provision_all.sh tools/rooms.csv      # -> provisioning/{rooms.snippet.yaml, device-tokens.txt}
# paste rooms.snippet.yaml into the broker ConfigMap; build + push the image:
docker build -t <registry>/meeting-displays/broker:1.0.0 backend && docker push <registry>/...
# internal-CA TLS: export your root CA to firmware/main/certs/broker_ca.pem; put the broker leaf
# cert+key in the broker-tls secret; the Ingress host must match the cert CN/SAN.
kubectl create namespace meeting-displays
kubectl -n meeting-displays create secret generic broker-secrets \
  --from-literal=alert-webhook-url='https://hooks.slack.com/...'   # no SA key — keyless
kubectl apply -f backend/deploy/k3s/broker.yaml -f backend/deploy/k3s/alerts.yaml
# verify: broker logs show renders (no freebusy errors); GET /api/v1/display/<device> returns 200.
```

### Step 5 — Firmware: flash ONE unit (eFuses OFF), validate
```bash
cd firmware
idf.py set-target esp32s3
idf.py menuconfig           # set Wi-Fi + CONFIG_MD_BROKER_HOST = your internal hostname (matches cert)
idf.py build flash monitor
./tools/provision_device.sh <device_id> <room> <wifi_ssid> <wifi_psk>   # writes token to NVS
```
Validate on hardware (all recoverable): image renders; if the first refresh hangs, flip
`EPD_BUSY_READY_LEVEL` in `epd_spectra6.cpp`; sanity-check battery mV vs a multimeter; confirm a
button wakes it from deep sleep.

### Step 6 — Fleet + hardening
Provision the remaining units. Only after a unit is fully validated, arm **Secure Boot V2 + Flash
Encryption** — on one sacrificial unit first (see `firmware/secure/README.md`); these burn eFuses
irreversibly. Enable OTA (dual-slot `partitions.csv` + the broker's `firmware:` config) for fleet updates.

### Gotchas that will save you a day
- **No SA keys** in this org — don't fight it; use WIF.
- **No GCE VM** without billing — use Cloud Shell for GAM.
- GAM's **SA-key-upload failure is harmless** for listing/sharing rooms (that uses user OAuth).
- A panel refresh is the battery cost — the 304 path is load-bearing, don't "simplify" it away.
- Room names may contain emojis; the e-paper font is ASCII — strip them (or ship a TTF).
- The CA file ships as a placeholder so devices **fail closed** until you install the real CA.
```
