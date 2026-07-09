# reTerminal E-Ink Signage — Spectra 6 Room Availability Displays

End-to-end system for a fleet of **Seeed reTerminal E1002** units (ESP32-S3 + 7.3" 800×480
E Ink **Spectra 6** color e-paper) showing live conference-room availability pulled from
**Google Workspace** room calendars.

> **New here? Start with [docs/BUILD-GUIDE.md](docs/BUILD-GUIDE.md)** — the honest write-up of
> how this was built, the dead-ends hit along the way, and a step-by-step guide to reproduce it.

The design target is **≥ 3 months on battery** per wall-mounted unit, served by a single
Go broker running on Kubernetes (developed against k3s).

```
┌──────────────────┐         poll (2 min)         ┌──────────────────────────────┐
│ Google Calendar  │ ◀──────────────────────────▶ │   broker (Go, on k3s)         │
│ (room resources) │                               │  ┌─────────────────────────┐  │
└──────────────────┘                               │  │ poller  → calendar svc  │  │
                                                    │  │ render  → Spectra6 4bpp │  │
   ┌───────────────┐   HTTPS GET (If-None-Match)    │  │ cache   → payload+ETag  │  │
   │ reTerminal     │ ─────────────────────────────▶│  │ http    → /display /tlm │  │
   │ E1002 (N units)│ ◀──── 200 payload | 304 ───────│  └─────────────────────────┘  │
   │ (ESP32-S3)     │   POST /telemetry              └──────────────────────────────┘
   └───────────────┘
```

## Why this shape

| Decision | Rationale |
|---|---|
| **Thin client** | The MCU never parses calendar JSON. It fetches a pre-quantized 4-bit-per-pixel framebuffer in the panel's native color codes, PackBits-compressed. Zero rendering CPU on battery. |
| **Server-side content hash + HTTP 304** | A full Spectra 6 refresh is the largest single energy draw in the whole wake cycle (~12–30 s of active panel current). The device sends its last ETag as `If-None-Match`; if the room's schedule pixels are unchanged the broker returns `304` and the device sleeps **without powering the panel**. Most 10-minute wakes cost ~1–2 s of radio only. |
| **BSSID/channel cache in NVS** | Skips the 1.5–2.5 s active scan on every wake. |
| **TLS session resumption (tickets)** | Avoids a full ECDHE handshake (and its radio-on time) on every wake. |
| **Go broker** | Single static binary, trivial k8s/k3s deploy, excellent concurrency for fan-out polling of many rooms + serving many low-rate clients. |

See **[PROTOCOL.md](PROTOCOL.md)** for the wire contract and **[docs/POWER.md](docs/POWER.md)**
for the battery budget that justifies the 3-month claim.

## Layout

```
backend/    Go broker: calendar integration, Spectra6 renderer, cache, HTTP, telemetry
firmware/   ESP-IDF (C++) firmware: deep-sleep state machine, fast Wi-Fi, TLS, EPD driver
tools/      Device provisioning (token + secure-boot/flash-encryption helpers)
docs/       Build guide, hardware reference, power budget, security notes
```

## Quick start

```bash
# Backend
cd backend && go build ./... && ./broker -config ./config.example.yaml
# (production deploy to Kubernetes) follow docs/BUILD-GUIDE.md; example manifests are the
# backend/deploy/k3s/*.yaml.example files — copy, fill in your own values, drop the .example suffix.

# Preview the rendered room layout to PNG — no hardware/calendar/broker needed:
go run ./cmd/preview               # writes preview-available.png / -inuse.png / -soon.png

# Simulate a device against a running broker (test payloads + battery alerts):
TOKEN=<raw-token> ../tools/fake_device.sh battery-demo

# Firmware
cd firmware
idf.py set-target esp32s3
idf.py menuconfig          # set Wi-Fi, broker host, device token (or use NVS provisioning)
# Drop your broker's CA root cert at firmware/main/certs/broker_ca.pem (see certs/README.md)
idf.py build flash monitor
```

Extras built in: OTA self-update (`firmware/main/ota.cpp` + broker `firmware:` config + dual-slot
`partitions.csv`), on-board SHT4x room temp/humidity telemetry, and low-battery alerting
(in-broker webhook + Prometheus rule in `backend/deploy/k3s/alerts.yaml`).

## Hardware

Verified against the official Seeed reTerminal E1002 schematic + ESP32-S3 datasheet — see
[docs/HARDWARE.md](docs/HARDWARE.md) for the pin map and panel/controller details.

## License

Apache 2.0 — see [LICENSE](LICENSE).
