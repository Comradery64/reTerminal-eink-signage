# Deploy tiers

The broker is a single static Go binary (`CGO_ENABLED=0`, three direct deps) that has always been
deployment-agnostic in code — the Kubernetes coupling was never in the binary, it was in
telemetry only existing in RAM (so you needed Prometheus to see any history) and config writes
only speaking to the in-cluster API (silently discarding every `/admin` edit outside k3s). Both are
now optional. This doc lays out three ways to run the same binary, in order of increasing
infrastructure — **not** in order of increasing legitimacy. Tier 1 is not a toy path to Tier 3;
it's the first-class recommendation for the common case (see the decision guide below).

| | Tier 1 — binary + systemd | Tier 2 — container / compose | Tier 3 — k3s |
|---|---|---|---|
| Runtime | `broker` static binary, systemd unit | one image, `docker compose up` | Deployment in k3s |
| Config persistence | `file` (auto-detected) | `file` (auto-detected, bind mount) | `configmap` (auto-detected) |
| Telemetry | `sqlite` recommended (`memory` fine) | `sqlite` on a named volume | `memory` or `prometheus` + existing scrape |
| Dashboards | `/status`, `/dashboard` (built in) | same | Grafana + `alerts.yaml` |
| TLS for devices | Caddy/nginx in front, or a static cert | Caddy in the compose profile | Traefik Ingress + cert-manager |
| Google Calendar | self-issued OIDC + WIF (`wiftoken`) | same | kubelet-projected token + WIF (unchanged) |
| Prereqs | a Linux host | a Linux host + Docker | k3s, Traefik, cert-manager, kube-prometheus-stack, (MetalLB, Longhorn) |
| Artifacts | `backend/deploy/systemd/` | `backend/deploy/compose/` | `backend/deploy/k3s/` |

Nothing about Tier 3 changes here — it stops being the *only* documented path, not a demoted one.

## Which tier am I

**Default answer: Tier 1.** Most IT departments running a room-display fleet do not have k3s,
Traefik, cert-manager, and kube-prometheus-stack sitting around — that's a real cluster with real
operational cost, and standing one up just to serve a config file and a PackBits framebuffer to a
dozen e-paper panels is a lot of infrastructure for the job. If your honest answer to "do we
already run Kubernetes for other things" is no, Tier 1 is not a downgrade from some Kubernetes
default — it *is* the default. A single spare machine, a systemd unit, and a config file get you
everything the product actually needs: the poller, the renderer, the HTTP cache/304 path, the
built-in `/status` and `/dashboard` views, and (if you want history) a local sqlite file.

Reach for **Tier 2** if:
- You want the image-based deploy/rollback workflow (`docker compose pull && up -d`) without a
  cluster, or you're already running other services under Docker on this host.
- You want the sqlite telemetry file and the config directory each on their own volume/bind mount,
  isolated from the host filesystem otherwise.

Reach for **Tier 3** if:
- You already run k3s (or any Kubernetes) for other workloads, so the marginal cost of one more
  Deployment is near zero.
- You already have kube-prometheus-stack + Grafana and want the fleet dashboard to live there
  instead of (or alongside) the broker's built-in `/status` page.
- You need the ConfigMap-as-source-of-truth GitOps workflow (`kubectl apply -f broker.yaml`) for
  config changes, rather than SSH + edit + a systemd/compose restart.

None of these is permanent. `telemetry.backend` and `config_persistence.mode` are just config keys
— moving from Tier 1 to Tier 3 later means copying `config.yaml` into a ConfigMap and switching
`config_persistence.mode` from `file` to `configmap` (or just `auto`, which resolves correctly in
both places). Nothing about the binary, the wire protocol, or the firmware changes.

## Tier 1 — binary + systemd

**Prerequisites:** a Linux host (systemd-based — practically everything current). No Docker, no
cluster.

**Install runbook:** see [`backend/deploy/systemd/README.md`](../backend/deploy/systemd/README.md)
for the full step-by-step (create user, install binary + config, `systemctl enable --now`).

**Resolved config_persistence:** `auto` → `file`, writing atomically (temp file + rename + fsync,
same directory) into `/etc/meeting-displays/config.yaml`. The load-bearing systemd detail is
`ReadWritePaths=/etc/meeting-displays` alongside `ProtectSystem=strict` — without it, every
`/admin`/`/manager` save fails and the operator sees the new red banner (see `applyConfig` in
`backend/internal/server/configwrite.go` for the persist-first design behind that banner) the
first time they try to save anything.

**Resolved telemetry:** `sqlite` is recommended (`memory` also works, same as every tier — see
[`docs/DASHBOARD.md`](DASHBOARD.md)). `systemd`'s `StateDirectory=meeting-displays` gives
`/var/lib/meeting-displays` correct ownership for `telemetry.db` without a manual `mkdir`/`chown`.

**Devices reach it over TLS via:** a reverse proxy (Caddy is the path of least ceremony — see the
`README.md` above) or, if you already terminate TLS elsewhere on this host, point the proxy's
upstream at `localhost:8080`. Keep `/metrics`, `/status`, and `/api/v1/status` off any public
listener — same trust boundary Tier 3's Ingress has always used (see `docs/DASHBOARD.md`
Decisions) — reach them via `curl localhost:8080/...` on the host, or SSH tunnel/port-forward
equivalent.

**Google Calendar auth:** the host becomes its own OIDC issuer via `wiftoken` (stdlib-only, ships
alongside the binary). Full runbook: [`docs/GOOGLE-AUTH.md`](GOOGLE-AUTH.md).

**Verify:**
```bash
curl -s localhost:8080/healthz
curl -s localhost:8080/api/v1/config-persistence   # {"mode":"file","target":"/etc/meeting-displays/config.yaml","durable":true,...}
```

## Tier 2 — container / compose

**Prerequisites:** a Linux host + Docker (with the `compose` plugin).

**Install runbook:** see [`backend/deploy/compose/README.md`](../backend/deploy/compose/README.md).

**Resolved config_persistence:** `auto` → `file` — no in-cluster API inside a plain container
either, so the same auto-detection that picks Tier 1's `file` backend picks it here too. The
`./config:/etc/meeting-displays` bind mount is `rw` specifically so this works; a read-only mount
here reproduces the same "Saved" lie the config-persistence redesign exists to kill.

**Resolved telemetry:** `sqlite` on the named `broker-data` volume, recommended more strongly than
on Tier 1 since a container restart (image update, host reboot, `docker compose up -d` after a
`pull`) is a routine event here, not an exceptional one — you want the battery curve to survive it.

**Devices reach it over TLS via:** the optional `caddy` service (`--profile tls`), sharing the
`8080` port with the `broker` service inside the compose network.

**Google Calendar auth:** the optional `wiftoken` sidecar (`--profile google`), sharing a token
volume with `broker` — same self-issued-OIDC approach as Tier 1, packaged as a long-running
process instead of a systemd oneshot+timer pair (compose has no built-in timer primitive).

**Verify:**
```bash
curl -s localhost:8080/healthz
curl -s localhost:8080/api/v1/config-persistence
```
The image is distroless (no shell), so there is deliberately no in-container `HEALTHCHECK` —
probe from the host as above, or point an external monitor at the published port.

## Tier 3 — k3s (today's production path, unchanged)

**Prerequisites:** k3s, Traefik (or another Ingress controller), cert-manager, kube-prometheus-stack
(for Grafana), optionally MetalLB and Longhorn depending on your cluster's networking/storage setup.

**Install runbook:** unchanged — see `backend/deploy/k3s/broker.yaml.example`,
`secret.example.yaml`, `alerts.yaml`, and `docs/BUILD-GUIDE.md` Steps 3–4.

**Resolved config_persistence:** `auto` → `configmap` (today's behavior, byte-for-byte). The
`broker.yaml.example` ConfigMap now spells this out explicitly as
`config_persistence: {mode: configmap, ...}` rather than relying on auto-detection silently doing
the right thing — same values, just visible in `kubectl get configmap broker-config -o yaml`
instead of requiring a reader to know the auto-detection rule.

**Resolved telemetry:** `memory` or `prometheus` (identical behavior; `prometheus` just logs and
documents the intent) plus the existing `/metrics` scrape and Grafana dashboard
(`backend/deploy/k3s/grafana-dashboard.json`). `sqlite` also works here if you want durable history
independent of your Prometheus retention window, but most Tier 3 deployments already have that via
Prometheus and don't need it.

**Devices reach it over TLS via:** the Traefik `Ingress` in `broker.yaml.example`, terminating at
`broker-tls` (an internal-CA-signed leaf cert the firmware pins).

**Google Calendar auth:** unchanged — the kubelet mints and rotates a projected ServiceAccount
token; WIF exchanges it. No key material anywhere.

**Verify:**
```bash
kubectl -n meeting-displays port-forward svc/broker 8080:80
curl -s localhost:8080/healthz
curl -s localhost:8080/api/v1/config-persistence   # {"mode":"configmap","target":"meeting-displays/broker-config",...}
```

### Tier 3 403 triage checklist

This is a **live production failure mode**, not a hypothetical: the broker's ConfigMap PATCH call
403s in some deployments today, and every `/admin`/`/manager` save is silently discarded as a
result (fixed going forward by the fail-closed config-persistence redesign — the save now fails
loudly with the real error instead of pretending to succeed). If you see a 403 in the persistence
banner or in the broker's logs, check these in order:

1. **Is `serviceAccountName: broker` actually set on the pod spec?** An *omitted* one silently
   falls back to the `default` ServiceAccount, which has no RBAC grant and always 403s. This is
   the single most common cause — `kubectl get pod -n meeting-displays -o jsonpath='{.items[0].spec.serviceAccountName}'`
   should print `broker`, not `default`.
2. **Are the `Role` and `RoleBinding` in the right namespace?** Both must be `meeting-displays` —
   a Role in the wrong namespace is invisible to a RoleBinding (and vice versa) even if both exist.
3. **Does `resourceNames` match the real ConfigMap name?** `broker.yaml.example`'s Role scopes to
   `resourceNames: ["broker-config"]` specifically — if your ConfigMap is named anything else, the
   Role grants nothing.
4. **Do the `verbs` include `patch`?** The broker does a JSON merge-patch, not a full `update`;
   `verbs: ["get", "update", "patch"]` is required (the Role in `broker.yaml.example` already has
   this — check for drift if it was hand-edited).
5. **Verify directly, don't guess:**
   ```bash
   kubectl auth can-i patch configmaps/broker-config \
     --as=system:serviceaccount:meeting-displays:broker -n meeting-displays
   ```
   This must print `yes`. If it prints `no`, the RBAC grant is wrong somewhere above — this
   command tells you definitively without needing to reproduce the actual PATCH call.

After the config-persistence redesign, this same failure is visible from the `/admin`/`/manager`
persistence strip and `GET /api/v1/config-persistence` instead of requiring a log grep — but the
underlying RBAC fix is still a `kubectl` operation, not something the broker can self-heal.
