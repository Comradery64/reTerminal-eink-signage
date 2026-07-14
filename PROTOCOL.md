# Wire Protocol — Meeting Display Payload Format (MDPF v1)

This contract is mirrored byte-for-byte in:
- `backend/internal/render/payload.go`
- `firmware/main/protocol.hpp`

Any change to the header MUST bump `MDPF_VERSION` in **both** files.

## Endpoints

All endpoints require `Authorization: Bearer <device_token>`. Tokens are per-device and
provisioned out of band (see `tools/provision_device.sh`).

### `GET /api/v1/display/{device_id}`
Fetch the current framebuffer for a room.

Request headers:
- `If-None-Match: "<hex-crc32>"` — the CRC32 of the framebuffer the device is currently showing.

Responses:
- `200 OK` — body is an MDPF payload (header + compressed framebuffer). `ETag` header carries
  the new content CRC32. The device must render this. Sent even when content is unchanged once
  per day, if the broker's optional `wake.forced_refresh_hour` anti-ghosting lever is enabled
  (see `docs/POWER.md`) — that response also carries `X-Forced-Refresh: 1`, for observability
  only; firmware doesn't need to look at it, since it already renders on any `200`.
- `304 Not Modified` — schedule pixels are unchanged. **The device must NOT power the panel.**
  The `X-Next-Wake` header (seconds) still tells the device when to wake next.
- `401 / 404` — bad token / unknown device.

Response headers on both 200 and 304:
- `X-Next-Wake: <seconds>` — server-recommended sleep duration before the next wake. Lets the
  broker push the device to a slower cadence outside business hours (e.g. 60 min at night).

### `POST /api/v1/telemetry/{device_id}`
Body: JSON (see `firmware/main/telemetry.hpp` / `backend/internal/telemetry`). Fire-and-forget;
the device does not block on the response. Returns `204`.

## MDPF payload header — 32 bytes, little-endian

| Off | Size | Field          | Notes |
|----:|-----:|----------------|-------|
| 0   | 4    | magic          | ASCII `"MDPF"` (0x4D 0x44 0x50 0x46) |
| 4   | 2    | version        | `1` |
| 6   | 2    | flags          | bit0 `COMPRESSED` (PackBits); bit1 `FULL_REFRESH` |
| 8   | 2    | width          | `800` |
| 10  | 2    | height         | `480` |
| 12  | 1    | bpp            | `4` (one Spectra 6 color code per nibble) |
| 13  | 1    | reserved       | `0` |
| 14  | 2    | reserved2      | `0` |
| 16  | 4    | payload_len    | bytes of payload following the header |
| 20  | 4    | raw_len        | uncompressed packed length = width*height/2 = 192000 |
| 24  | 4    | next_wake_s    | echo of `X-Next-Wake` for offline robustness |
| 28  | 4    | content_crc32  | CRC32 (IEEE) of the **uncompressed packed framebuffer**; also the ETag |

Then `payload_len` bytes:
- if `COMPRESSED`: PackBits-encoded packed framebuffer.
- else: raw packed framebuffer (`raw_len` bytes).

## Packed framebuffer format

Row-major, top-left origin, 2 pixels per byte. The **high nibble is the left/even pixel**,
low nibble the right/odd pixel. Each nibble is a Spectra 6 controller color code:

| Code | Color  |
|-----:|--------|
| 0x0  | Black  |
| 0x1  | White  |
| 0x2  | Yellow |
| 0x3  | Red    |
| 0x5  | Blue   |
| 0x6  | Green  |

> ⚠️ These codes follow the common Good Display 7.3" E6 controller mapping. **Verify against the
> exact panel datasheet shipped in your E1002 batch** and adjust `Spectra6Code` in
> `backend/internal/render/spectra6.go` + `EPD_CLR_*` in `firmware/main/epd_spectra6.cpp` if needed.
> Because the codes are centralized in one constant block on each side, a panel revision is a
> one-line change.

## PackBits (compression)

Standard PackBits over the packed byte stream (e-paper images are mostly flat fills, so this is
both tiny and trivial to decode on the MCU with no heap allocation — it streams straight into the
panel as it decodes).

Encoding (per the canonical Apple/TIFF PackBits):
- Read control byte `n` (signed):
  - `0..127`   → copy the next `n+1` bytes literally.
  - `-127..-1` → repeat the next single byte `1-n` times.
  - `-128`     → no-op (skip).
