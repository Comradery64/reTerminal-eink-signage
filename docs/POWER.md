# Power budget — justifying 3-month battery life

> These are **engineering estimates** to size the design and prove the architecture closes. Bench
> the real numbers per unit with a power analyzer (Nordic PPK2 / Joulescope) before fleet rollout.
> The point of this page is to show *where the energy goes* and *which knobs matter*.

## Current draw by state (estimates, ESP32-S3 + 7.3" Spectra 6)

| State | Current | Duration | Notes |
|---|---:|---:|---|
| Deep sleep (RTC + panel rail gated off) | ~40 µA | continuous | RTC mem retained; panel FET off |
| Wake + fast Wi-Fi assoc + TLS resume | ~100 mA avg | ~1.5 s | BSSID cache + session ticket skip scan/full-handshake |
| **Panel full refresh (Spectra 6)** | ~45 mA | **~25 s** | **dominant active cost** — MCU idle + panel driving |

## Wake schedule (per the broker's `X-Next-Wake`)

- Business hours (07:00–20:00, ~13 h) @ 10 min → **78 wakes**
- Off hours (~11 h) @ 60 min → **11 wakes**
- **~89 wakes/day**

Crucially, **only wakes whose content changed cost a refresh.** The server-side content hash +
HTTP 304 means a typical room redraws ~15×/business day (meeting start/end, "starting soon"
transitions); the other ~74 wakes are radio-only.

## Daily energy

| Bucket | Calc | mAh/day |
|---|---|---:|
| Deep sleep | 0.04 mA × 24 h | 0.96 |
| No-change wakes (304) | 74 × (100 mA × 1.5 s) | 3.09 |
| Refresh wakes | 15 × (45 mA × 25 s) | 4.69 |
| **Total** | | **≈ 8.7 mAh/day** |

Over 90 days → **~785 mAh**. A single-cell LiPo in the 2000–3000 mAh class clears this with
comfortable margin for self-discharge, cold-corner current, and aging.

## The decisive knob: refresh count

If the device refreshed on **every** wake (no 304 optimization):

| | mAh/day | 90-day draw |
|---|---:|---:|
| With 304 (15 refreshes) | 8.7 | ~785 mAh |
| Without 304 (89 refreshes) | ~28 | ~2500 mAh |

The conditional-GET design roughly **3×'s battery life**. Everything else (fast Wi-Fi, TLS
resumption, modem sleep) trims the radio-only wakes, which are the *second* largest bucket — worth
doing, but secondary to not refreshing the panel.

## Levers, in priority order

1. **Don't refresh unchanged content** — server hash + 304 (implemented).
2. **Widen off-hours / weekend cadence** — `wake.off_hours_seconds` (implemented, server-driven).
3. **Cut radio-on time** — BSSID/channel cache, TLS session tickets, HW crypto (implemented).
4. **Coalesce "starting soon" transitions** — render the soon-state only once near the boundary
   rather than re-evaluating each wake (tune the 15-min threshold in `render.go`).
5. **Anti-ghosting daily forced refresh (optional, costs energy)** — `wake.forced_refresh_hour`.
   E-ink retains a faint image bias from sitting on the same frame across many wake cycles. This
   spends one full-refresh cycle (the dominant per-wake energy cost — see above) per device per
   day, during an off-hours hour you pick, purely to keep that bias from accumulating. It's the one
   lever that deliberately works against the "don't refresh unchanged content" budget above, so
   it's opt-in (nil/omitted = disabled) rather than on by default.
