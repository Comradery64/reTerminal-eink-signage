# Fleet Dashboard

Status: **implemented.** This document scopes two complementary views into fleet health — battery
level, availability (online/offline), last check-in, last screen refresh, RSSI, firmware version —
and the backend work each depends on.

## Where things live

- `backend/internal/telemetry/telemetry.go` — `md_last_render_seconds` gauge (the prerequisite
  below), plus `Store.Snapshot` for structured (non-Prometheus-text) reads.
- `backend/internal/status` — derives each device's `ok` / `stale` / `low_battery` / `unreported`
  classification from `config.AlertConfig` (the same thresholds `notify.Manager` alerts on).
- `backend/internal/server/status.go` — `GET /api/v1/status` (JSON) and `GET /status` (HTML table,
  `?format=json` for the same JSON body). Cluster-internal-only, unauthenticated, same trust
  boundary as `/metrics` — see Decisions below; not reachable through the public Ingress
  (`backend/deploy/k3s/broker.yaml.example`).
- `backend/deploy/k3s/grafana-dashboard.json` + `grafana-dashboard-configmap.yaml.example` — the
  Grafana dashboard, provisioned via the kube-prometheus-stack sidecar convention (see the comment
  at the top of the `.yaml.example` file for how to regenerate the ConfigMap from the JSON).
- `backend/deploy/k3s/alerts.yaml` — unchanged in behavior; a comment now points at
  `alerts.stale_after` as the value to keep in sync with `DisplayStale`'s `3600s`.

## Why two, not one

| | Grafana dashboard | Broker status page |
|---|---|---|
| Audience | Engineering / on-call | Facilities / building manager |
| Depends on | Your Prometheus + Grafana stack existing and being reachable | Nothing beyond the broker itself |
| Data source | `/metrics` (already scraped) | New endpoint, reads the broker's in-memory state directly |
| Auth | Whatever your Grafana instance already uses | Unauthenticated, cluster-internal-only (decided — see Decisions) |
| Effort | Low (dashboard JSON only, if the one metric gap below is filled) | Medium (new handler + view) |

They're not redundant: Grafana is the deep-dive/history tool (trends, alert correlation, already
wired to Slack via `alerts.yaml`); the status page is a zero-dependency "is everything OK right
now" view for someone without cluster/Grafana access.

## Prerequisite gap: no "last render" signal today

`backend/internal/telemetry/telemetry.go` stores only the **latest** report per device. Firmware
already sends `rendered: bool` per wake (`firmware/main/telemetry.hpp`), but the broker never
turns "did it render on the most recent report" into "how long since it last actually rendered."
Right now `/metrics` can answer "is it checking in" (`md_last_seen_seconds`) but not "is it still
actually refreshing the panel when content changes" — the more useful ghosting/hardware-health
signal. Both plans below need this fixed first:

- Add `lastRendered time.Time` to the telemetry `state` struct, updated only when `Report.RenderedBool`
  is true.
- Emit a new gauge: `md_last_render_seconds{device="..."}` (age since last actual repaint),
  parallel to the existing `md_last_seen_seconds`.
- This one change is small, additive, and unblocks both plans — do it regardless of which
  dashboard ships first.

## Plan A — Grafana dashboard

Scope: a dashboard JSON checked into the repo (e.g. `backend/deploy/k3s/grafana-dashboard.json`
or a `dashboards/` ConfigMap, matching however `kube-prometheus-stack` auto-discovers dashboards
in your cluster — likely a `ConfigMap` labeled `grafana_dashboard: "1"`).

Proposed panels, one row per device (templated on the `device` label so it scales to N rooms
without per-room edits):
- **Battery** — `md_battery_percent` gauge + `md_battery_millivolts` as a hover detail (mirrors
  the nonlinear LiPo caveat already documented in `alerts.yaml`)
- **Availability** — `md_last_seen_seconds`, colored (green < 15m, amber < 1h, red > 1h — matches
  the existing `DisplayStale` alert threshold of 3600s so the dashboard and the alert agree)
- **Last screen refresh** — `md_last_render_seconds` (new, see above)
- **Signal** — `md_wifi_rssi_dbm`
- **Firmware** — `md_boot_count` as a reboot-loop smell test (a device rebooting far more often
  than its wake cadence implies is crashing, not sleeping)
- **Room environment** — `md_room_temp_celsius` / `md_room_humidity_percent` where reported
- Fleet summary row at the top: count of devices below the battery/staleness thresholds, so
  on-call doesn't have to scan every row

Work items (planning-level, no code yet):
1. Backend: add `md_last_render_seconds` (prerequisite above).
2. Author the dashboard JSON (can be done in the Grafana UI, then exported — doesn't require
   touching backend code at all beyond #1).
3. Decide provisioning mechanism: commit the JSON + a `ConfigMap` manifest (`*.yaml.example`,
   consistent with every other manifest in `backend/deploy/k3s/`) vs. leaving it as a manual
   import step documented in `docs/DEPLOY.md` (private repo). Recommend committing it — same
   reasoning as everything else in `deploy/k3s/`: reproducible, not a manual click-through.

## Plan B — Broker status page

Scope: a new read-only endpoint on the existing broker binary (public repo — this is generic,
not company-specific, so it belongs upstream like everything else in `backend/`).

- `GET /api/v1/status` — JSON array, one object per configured room: `device_id`, `name`,
  `battery_pct`, `last_seen_seconds`, `last_render_seconds` (once the prerequisite lands),
  `rssi`, `firmware_version`, `boot_count`, and a derived `status: "ok" | "stale" | "low_battery"`
  enum so a simple UI doesn't need to reimplement the threshold logic that already lives in
  `alerts.yaml`/`notify.Manager` (worth factoring those thresholds into one shared place both the
  alerter and this endpoint call, rather than duplicating the 45%/3600s constants).
- `GET /status` — a minimal server-rendered HTML table over the same data (no JS framework, no
  build step — consistent with this project's "thin client, simple server" philosophy). A
  `?format=json` or separate path covers anyone who wants to poll it programmatically.
- Data source: purely in-memory (`cache.Store` + `telemetry.Store`), same as `/metrics` — no new
  storage, no database.

Work items (planning-level, no code yet):
1. Backend: add `md_last_render_seconds` (shared prerequisite with Plan A).
2. Factor alert thresholds (battery %, staleness) out of `notify.Manager` into something both it
   and the new status handler can reference, to avoid two sources of truth for "what counts as
   low battery."
3. New `internal/status` package (or a couple of handlers in `internal/server`) exposing the JSON
   endpoint; a small HTML template for the human-readable view.
4. Auth model: none — cluster-internal-only (decided, see Decisions).

## Decisions

1. **Status-page auth: cluster-internal-only, unauthenticated.** Same trust boundary as `/metrics`
   today — no Basic Auth, no token. The endpoint must not be exposed on the public ingress
   (`backend/deploy/k3s/broker.yaml.example`'s `Ingress` resource only fronts `/api/v1/display`
   and `/api/v1/telemetry` today for device traffic — `/status` and `/api/v1/status` should
   likewise never be added to that `Ingress`; reachable only via `kubectl port-forward` / a
   cluster-internal `Service`, same as how `/metrics` is scraped in-cluster by Prometheus and
   never exposed externally).
2. **Grafana/Prometheus: assumed present (or in progress) on the same k3s cluster the displays
   already run on.** No separate stand-up work scoped here — this plan takes kube-prometheus-stack
   (or equivalent) as a given, matching what `backend/deploy/k3s/alerts.yaml` already assumes
   (`PrometheusRule`/`AlertmanagerConfig` CRDs). If it later turns out the stack isn't actually
   there yet when implementation starts, that's a prerequisite to flag at that time, not now.
3. **Public/private split: dashboard JSON, panel layout, and the broker's `/status` + `/api/v1/status`
   handler code all belong in the public repo** — none of it is company-specific, same reasoning as
   every other file in `backend/`. Only real datasource UIDs, Grafana instance URLs, and the actual
   `Ingress`/`Service` manifests wired to your real cluster stay in the private repo's real
   `broker.yaml`, following the same `.yaml`/`.yaml.example` split already used for everything else.
   Nothing secret is needed for either plan: no new passwords, keys, or PII are introduced by
   either the dashboard JSON or the status endpoint.

## Suggested order

1. Ship the `md_last_render_seconds` prerequisite (small, unblocks both, useful on its own even
   before either dashboard exists).
2. Grafana dashboard next, since the stack is assumed present on the same cluster — lowest
   incremental effort (panels only, no new backend surface beyond the prerequisite).
3. Broker status page after, scoped strictly cluster-internal per the auth decision above (no
   ingress exposure, no new auth code needed).
4. Both coexist long-term; they serve different audiences (engineering/on-call vs. facilities, if
   facilities are ever granted cluster access — otherwise the status page's real audience today is
   still "someone with `kubectl` access," same as `/metrics`).
