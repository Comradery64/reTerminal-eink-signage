# Onboarding: 0 units + no GCP connection → fully deployed multi-room fleet

Audit of what the current `/admin` GUI actually covers today (2026-08-05, after flashing
two bench units), and what still requires a terminal. Written so the next room, and
the next *person* who isn't comfortable in a shell, has a real path forward.

## What already works entirely inside the app today

- **Room lifecycle** — add/edit/remove a room, set its device token (hashed server-side,
  never stored in plaintext), set a per-room wake override, and label a room's individual panels
  when it has more than one: `/admin` → Rooms. Confirmed working live this session (two units'
  tokens both pushed via the web form, both persisted to the ConfigMap, both
  confirmed checking in). See "Multiple displays per one room" below.
- **Fleet wake defaults, alerts, access grants (RBAC for admin/manager/viewer + 2FA)** — all
  `/admin` sections, all backed by the same config-persistence path.
- **OTA firmware rollout** — `/admin` → Firmware sets a target version + image URL for devices
  *already deployed and networked*. This is the only firmware-related GUI surface that exists;
  see the gap below for why it doesn't cover first-time flashing.

## What still requires a terminal (the actual gaps)

1. **Initial firmware flash of a blank unit** — 100% CLI (`idf.py build flash`, or raw
   `esptool.py write_flash`). No WebSerial/browser flashing exists. A non-engineer cannot do this
   today; it requires ESP-IDF installed locally and a USB cable.
2. **Per-device provisioning (Wi-Fi SSID/PSK + bearer token → NVS)** — `tools/provision_device.sh`
   + `esptool.py write_flash 0x9000`, entirely CLI. The token it generates then has to be
   manually copy-pasted (or scripted, as we did this session) into `/admin`'s room-save form —
   two disconnected manual steps that are easy to get out of sync.
3. **Broker TLS root CA** (`firmware/main/certs/broker_ca.pem`) — must be manually exported
   (`kubectl exec step-ca-0 -- cat .../root_ca.crt`) and dropped in before the firmware even
   builds. No in-app guidance, no validation that it matches the live `broker-tls` secret until
   you try to connect.
4. **GCP Workload Identity Federation + Calendar sharing** (`tools/setup_wif.sh`,
   `tools/gcp_setup.py`, `tools/share_rooms.sh`, the GAM/Cloud Shell dance in
   `docs/BUILD-GUIDE.md` Part 2/Step 2) — entirely `gcloud`/GAM CLI. This is a one-time,
   security-sensitive org setup; not unreasonable that it stays CLI, but there's no in-app
   "is this working?" signal — you only find out via a `fetch schedule failed` log line.
5. **Broker deploy itself** (`kubectl apply`, image build/push, RBAC/WIF cutover) — inherently
   can't be self-service from inside an app that doesn't exist yet. Fine to stay CLI, but see
   Phase 1 below for consolidating the *steps*, not the *tool*.
6. **Credential drift has no detection.** Found twice this session, independently: a stale
   plaintext password in a 1Password item whose own `password_sha256` field was correct, and the
   fleet Wi-Fi item whose recorded SSID was simply wrong. Nothing in the app or the tooling would
   have caught either without a live login/connect attempt failing first.

## Proposed onboarding flow, phased

**Phase 1 — no new infra, biggest friction reduction for the least effort:**
- A `/admin` "Setup status" panel: GCP/Calendar auth last-success timestamp, fleet firmware
  version spread, per-room "last successful check-in", and a manual "test calendar auth now"
  button that does one live freebusy call and reports pass/fail instead of waiting for the next
  poll to fail silently in logs.
  - Note on effort: the per-room check-in half is genuinely just surfacing — `status.Build`
    already computes `LastSeenSeconds` (`status.go:57`) for `/dashboard`. The calendar
    last-success half is **not**: nothing records it today. `poller.refreshRoom` only logs on
    failure (`poller.go:81`) and calls `store.SetError`; the success path stores a schedule but
    no timestamp. This needs a new `LastFetchOK time.Time` on the cache/telemetry snapshot,
    set in `refreshRoom` after a successful `FetchSchedule`.
- One consolidated `./bootstrap.sh` that walks Steps 1–4 of `docs/BUILD-GUIDE.md`
  interactively (still CLI, but currently split across 4 docs + 6 scripts) — reduces the
  first-time setup from "read 4 docs" to "run one script, answer prompts."

**Phase 2 — in-app flashing (the highest-value gap to close):**
- Add an "Add a device" wizard to `/admin` using **ESP Web Tools** (Espressif/Google's
  open-source WebSerial flashing library — Chrome/Edge only, no install). Since the CA cert and
  broker host are deployment-wide (not per-room), the broker can host one pre-built
  `meeting_display.bin` for its own deployment; the wizard just needs to flash that + a
  browser-generated NVS image (SSID/PSK/token) over WebSerial, then call the *existing*
  `/admin/rooms/save` endpoint with the same token automatically. This collapses today's
  4-tool dance (`idf.py`, `provision_device.sh`, `esptool.py`, manual form paste) into: plug in
  device → pick room → click Flash.
- Store the fleet's Wi-Fi SSID/PSK once in the broker config (already has a secrets path via
  the `auth`/ConfigMap pattern) instead of re-typing it per device — removes the exact class of
  error hit this session (wrong SSID baked into one device's NVS).

**Phase 3 — GCP/Calendar wizard (lower priority, higher complexity):**
- Full self-service OAuth+sharing wizard is a bigger lift (needs Workspace admin + GCP IAM
  scopes) and is arguably *correctly* gated behind a human with org privileges. Realistic target:
  keep it CLI, but add the Phase 1 status panel's "test calendar auth" button so failures surface
  immediately instead of being discovered via `kubectl logs`.

## Phase 2 spec: in-app flashing wizard

Concrete design for the highest-value gap — collapsing today's 4-tool manual dance
(`idf.py build flash`, `provision_device.sh`, `esptool.py write_flash 0x9000`, paste the token
into `/admin`) into: plug in device → pick/create room → click Flash. Written to be handed to an
engineer directly; decisions below are made, not left open, except where explicitly flagged.

### Hard preconditions (read first)

- **Flash encryption / NVS encryption must stay disabled** for this wizard to work.
  `partitions.csv` already declares `nvs_keys ... encrypted`, and `provision_device.sh` documents
  the `espsecure.py encrypt_flash_data --aes_xts` step for when it is armed. Today it works only
  because `CONFIG_SECURE_FLASH_ENC_ENABLED` / `CONFIG_NVS_ENCRYPTION` are commented out
  (`firmware/sdkconfig.defaults:59-62`). The moment that hardening is turned on, a plaintext NVS
  image written at 0x9000 is unbootable garbage and ESP Web Tools cannot flash the app image
  either. **Arming flash encryption and this wizard are mutually exclusive** until someone builds
  a signing/encrypting path; whoever arms it must disable the wizard in the same change.
- **The wizard provisions PSK-only, no-mTLS devices.** `provision_device.sh` also supports
  WPA2-Enterprise (`eap_id`/`eap_user`/`eap_pass`) and per-device mTLS
  (`client_cert`/`client_key`, which are `file`-type = *blob* NVS entries). The wizard implements
  neither; `net_client.cpp` presents client certs only when present, so a wizard-provisioned
  device is server-auth-TLS-only. Those two modes stay on the CLI script.
- **`firmware.dir` must be set.** It defaults to `""` (`config.example.yaml:52`) and the
  `GET /firmware/` route is only registered when non-empty (`server.go:186-189`). The wizard
  needs a preflight check that fails with a clear message rather than a 404.
- **`/firmware/` is unauthenticated** (deliberately — see `server.go:130`; devices fetch OTA
  images before any session exists). The manifest makes the flash layout discoverable. That is an
  accepted trust boundary, not a regression, but it is why no secret may ever be served from
  `firmware.dir`.

### Architecture

The fleet Wi-Fi PSK is a *shared* secret: unlike a per-device token, one leak compromises every
device. So it never reaches the browser. The NVS image is built server-side in Go and handed to
the browser as opaque bytes to write over USB.

```
Browser (/admin → "Add a device")
  │
  ├─ 1. Fetch pre-built firmware + manifest from THIS broker: GET /firmware/manifest.json
  │     (ESP Web Tools manifest — points at bootloader.bin/partition-table.bin/meeting_display.bin,
  │     served by the existing `firmware.dir` static handler, server.go:189
  │     `mux.Handle("GET /firmware/", ...)`. Nothing new to build here except generating the
  │     manifest and dropping the 3 .bin files in that dir as part of the release process.)
  │
  ├─ 2. User picks target serial port (WebSerial permission prompt) → ESP Web Tools flashes the
  │     app image, ERASING FIRST (see below). No CA cert / broker host prompt needed — those are
  │     baked into the one shared .bin at build time, same as today.
  │
  ├─ 3. Wizard POSTs {device_id} to /admin/api/provision-nvs (new, admin-role-only). The server
  │     generates a 256-bit token, builds the NVS partition image (device_id/token/wifi_ssid/
  │     wifi_psk under the `prov` namespace) in Go, and responds with:
  │       - the raw 0x6000-byte partition image (base64), and
  │       - the token's SHA-256 hex.
  │     The plaintext token exists only in that one response body and in the device's flash; the
  │     PSK never leaves the pod. Neither is ever logged.
  │
  ├─ 4. Wizard writes the returned image over WebSerial at offset 0x9000 (partitions.csv —
  │     unchanged).
  │
  └─ 5. Wizard POSTs to the EXISTING /admin/rooms/save endpoint with a NEW optional
        `token_sha256` field (see below), so no plaintext token is ever sent back to the server.
```

**Erase before flash.** A re-flashed unit keeps its old `otadata`, which may point at `ota_1`,
so writing only `ota_0` leaves it booting the previous image. The wizard must use ESP Web Tools'
full-erase install path (or explicitly erase `otadata` at 0x10000) rather than a bare app write.

### New/changed server-side pieces

1. **Fleet Wi-Fi config.** A new `Fleet.WifiSSID` / `Fleet.WifiPSK` pair on
   `Config`, following the *exact* existing `${ENV_VAR}` pattern used for
   `Alerts.WebhookURL` (`config.go:138`, expanded in `Load()` via `envRef`, restored on
   `MarshalForPersist` — see `config.go:204-250`). Concretely:
   - Add `Fleet FleetConfig` to `Config`, `FleetConfig{WifiSSID, WifiPSK string}`.
   - ConfigMap gets `fleet: { wifi_ssid: "<your fleet SSID>", wifi_psk: "${MD_WIFI_PSK}" }`.
   - `broker-secrets` Secret gets a new key `wifi-psk`; Deployment env-injects it as `MD_WIFI_PSK`
     (same mechanism as the webhook URL already uses — no new plumbing pattern, just one more key).
   - This directly fixes the class of bug hit this session (wrong SSID hand-typed per device from
     a stale 1Password item) by making Wi-Fi creds a single fleet-wide value set once.

2. **`POST /admin/api/provision-nvs`** (new, admin-role-only, same auth middleware as the rest of
   `/admin`). Request `{"device_id": "..."}`; response
   `{"nvs_b64": "...", "token_sha256": "<64 hex>"}`. Generates a 256-bit token
   (`crypto/rand`, base64url, no truncation), builds the NVS image (below), and returns both.
   Never logs the token or the PSK; sets `Cache-Control: no-store`.

3. **`/admin/rooms/save` gets one new optional form field: `token_sha256`.** In
   `handleAdminSaveRoom`, the new branch goes *first* in the existing precedence chain
   (`admin.go:70-76`), which today is `token` plaintext → else the existing room's stored hash:
   ```
   token_sha256 (validate 64 lowercase hex; reject otherwise) → token (hash it) → existing.TokenSHA256
   ```
   That ordering matters: the `existing.TokenSHA256` fallback is what stops an unrelated edit from
   wiping a room's token (the bug fixed in 3c043ce), and create-with-no-token must still fail
   validation. Keeps the manual/curl/CLI path working unchanged while letting the wizard send only
   a hash.

4. **`/firmware/manifest.json`** — **generated**, not hand-written, and written into whatever
   directory `firmware.dir` points at. Standard ESP Web Tools format:
   ```json
   {
     "name": "Meeting Display",
     "version": "1.0.0",
     "builds": [{
       "chipFamily": "ESP32-S3",
       "parts": [
         {"path": "bootloader.bin", "offset": 0},
         {"path": "partition-table.bin", "offset": 32768},
         {"path": "meeting_display.bin", "offset": 131072}
       ]
     }]
   }
   ```
   Offsets are verified against `firmware/partitions.csv`: `0x8000`=32768 partition table,
   `0x20000`=131072 = `ota_0`; bootloader at 0 is correct for the ESP32-S3.
   `version` **must** come from `FIRMWARE_VERSION` in `firmware/main/config.hpp:69` (currently
   `1.0.0`), not be typed by hand — otherwise it drifts from both the firmware and
   `firmware.version` in the ConfigMap. Generate it in the same release step that copies the 3
   `.bin`s into `firmware.dir` and updates `firmware.version`/`firmware.url` for OTA.

### NVS image builder, in Go (the one genuinely hard part)

A hand-rolled reimplementation of a binary format. It lives server-side in Go
(`backend/internal/nvs`) rather than as browser JS, for three reasons: the fleet PSK never has to
be exposed to a browser; it gets a real Go golden-file test instead of an ad-hoc Node CI script;
and it avoids adding Python + the ESP-IDF generator to a container image that is otherwise a slim
Go binary. Bounding the scope deliberately:

- **Do not** implement the general ESP-IDF NVS format (blobs, encryption, multi-page GC). We only
  ever write the `prov` namespace entry plus 4 short string entries (`device_id`, `token`,
  `wifi_ssid`, `wifi_psk`) into a single fresh 0x6000-byte partition, matching exactly what
  `tools/provision_device.sh` produces today via `nvs_partition_gen.py generate ... 0x6000`.
  Note the namespace entry is not optional — it is what `nvs_store.cpp` opens by name.
- Page layout: 32-byte page header (state, seq, version, CRC32), 32-byte entry-state bitmap, then
  32-byte entries; string values spill into following entry slots as the variable-length payload.
  One 4096-byte page holds all five entries; the remaining pages are `0xFF`-filled exactly like a
  freshly-erased flash region.
- **Verification gate before this ships**: a Go golden test that byte-compares this package's
  output against images produced by the real `nvs_partition_gen.py` for the same inputs. Check
  the golden `.bin` fixtures into the repo (they contain only dummy credentials) and document the
  exact command that regenerates them. Don't trust a from-scratch binary-format reimplementation
  without an explicit byte-for-byte comparison against the tool it's replacing.

### Implementation order

1. ✅ `Fleet.WifiSSID`/`WifiPSK` config + `${ENV_VAR}` secret wiring (`config.FleetConfig`;
   `MD_WIFI_PSK` wired through all three deploy tiers — k3s Secret, compose `.env`, systemd
   `broker.env`).
2. ✅ `token_sha256` optional field on `/admin/rooms/save`.
3. ✅ `internal/nvs` Go image builder + the byte-for-byte golden test against
   `nvs_partition_gen.py` (ESP-IDF 5.5). Parity confirmed on the real four-key provisioning image
   and at every entry-packing boundary (value lengths 1/31/32/33/63/64/200); fixtures live in
   `internal/nvs/testdata/`.
4. ✅ `POST /admin/api/provision-nvs` wiring 1 + 3 together.
5. ⬜ Generated `manifest.json` + confirm ESP Web Tools' `<esp-web-install-button>` flashes the 3
   existing `.bin`s correctly against one bench unit (reuse the exact bytes already used for
   every manual flash this session — no firmware code changes needed for this phase).
6. ⬜ Wire the wizard UI in `/admin` (new "Add a device" section, reusing the existing Rooms-form
   styling) and do one real end-to-end bench test against a blank unit before calling this done.

Steps 5–6 are browser-side and need a bench unit plus a Chrome/Edge WebSerial session, so they
can't be finished or honestly verified from a terminal. Everything the wizard calls exists and is
tested; what's left is the page that calls it.

## Multiple displays per one room

A room can have several panels — a door plaque plus an interior display is the motivating case —
and the config layer always allowed it: `Room` is keyed on `device_id`, and `Validate` never
required `room` (the calendar address) to be unique. What was missing was everything around it.

- **`Room.Label`** — the panel's physical position ("Door", "Interior wall"), *not* the room's
  name. Optional and empty for a single-display room, so every existing config keeps loading
  unchanged. Once two entries share a calendar address, `Validate` requires both to carry a
  non-empty, distinct label (compared case-insensitively, as are the addresses themselves) — so
  the fleet can never contain two indistinguishable cards. Editable from `/admin` → Rooms; an
  edit that omits the field keeps the stored value, the same rule the token and wake overrides
  already follow.
- **One calendar fetch per room, not per panel.** `poller.refreshAll` now groups displays by
  address and calls `FetchSchedule` once per group. Before this, a room's third panel meant a
  third identical freebusy call every poll cycle — pure Google quota burn for an identical
  answer. The compose/PNG-encode step is memoized per displayed name within the group too, and
  a failed fetch is recorded against every display in that room rather than just the first.
- **Dashboard differentiation.** Cards show the label under the room name (rendered only when
  set, so single-display fleets look unchanged), and the grid is now sorted by room name then
  label, so a room's panels always sit next to each other instead of wherever their config
  entries happened to land. `status.Device.Title()` is the single place that formats
  "Room — Label", so `/status`, `/dashboard`, and alert text can't drift apart.

Both panels in a room render the same image: `Label` deliberately never reaches the renderer,
since it's an operator-facing distinction, not something to print on a door plaque.

## Immediate cleanup items found this session (not yet done)

- The 1Password item holding the fleet Wi-Fi credential records the wrong SSID, so anyone
  provisioning from it bakes a network name into NVS that the display can't join. Fix or delete
  the item so it can't mislead the next flash. (Deliberately not naming the item or the network
  here — this doc is version-controlled; look the credential up in 1Password.)
