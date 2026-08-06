# Tier 1: plain binary + systemd

The zero-infrastructure deploy path — a static `broker` binary on any spare Linux machine. No
Kubernetes, no Docker, no cluster. See [../../../docs/DEPLOY-TIERS.md](../../../docs/DEPLOY-TIERS.md)
for how this compares to Tiers 2/3 and which one you actually want.

## What resolves where

- **Config persistence**: auto-detects to `file` (no in-cluster API on this host, and
  `/etc/meeting-displays` is writable once `ReadWritePaths` is set below) — atomic temp-file +
  rename into `config.yaml`. See `backend/config.tier1.example.yaml`.
- **Telemetry**: `sqlite` recommended (battery curve + refresh counts survive a restart with no
  Prometheus/Grafana required); `memory` also works if you don't care about history.
- **Google Calendar auth**: keyless WIF via the broker's own OIDC issuer (`wiftoken` — see
  [../../../docs/GOOGLE-AUTH.md](../../../docs/GOOGLE-AUTH.md)), *not* the kubelet-projected token
  Tier 3 uses (there's no kubelet here).

## Install runbook

```bash
# 1) Dedicated, unprivileged user — the binary never needs root.
sudo useradd --system --no-create-home --shell /usr/sbin/nologin meeting-displays

# 2) Binary.
sudo install -o root -g root -m 0755 broker /usr/local/bin/broker
sudo install -o root -g root -m 0755 wiftoken /usr/local/bin/wiftoken   # only if using Google Calendar

# 3) Config. Copy the tier-1 example, edit the REPLACE_* values, lock it down (it can contain
#    password/token hashes and, without ${ENV} refs, secrets):
sudo mkdir -p /etc/meeting-displays
sudo cp config.tier1.example.yaml /etc/meeting-displays/config.yaml
sudo cp broker.env.example /etc/meeting-displays/broker.env
sudo chown -R meeting-displays:meeting-displays /etc/meeting-displays
sudo chmod 0600 /etc/meeting-displays/config.yaml /etc/meeting-displays/broker.env
$EDITOR /etc/meeting-displays/config.yaml   # rooms, users, timezone, ...
$EDITOR /etc/meeting-displays/broker.env    # SESSION_SECRET, ALERT_WEBHOOK_URL

# 4) (Optional) Google Calendar — see docs/GOOGLE-AUTH.md for the full one-time gcloud setup.
sudo -u meeting-displays wiftoken -genkey /etc/meeting-displays/gcp/wiftoken.key
sudo -u meeting-displays wiftoken -jwks -key /etc/meeting-displays/gcp/wiftoken.key > jwks.json
#   ... upload jwks.json to GCP, write cred-config.json (see docs/GOOGLE-AUTH.md) ...
sudo cp cred-config.json /etc/meeting-displays/gcp/cred-config.json
sudo chown -R meeting-displays:meeting-displays /etc/meeting-displays/gcp

# 5) Units.
sudo cp meeting-displays-broker.service wiftoken.service wiftoken.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now meeting-displays-broker.service
sudo systemctl enable --now wiftoken.timer   # only if using Google Calendar

# 6) Verify.
curl -s localhost:8080/healthz
curl -s localhost:8080/api/v1/config-persistence   # mode should be "file", durable: true
```

## Reverse proxy for device TLS

The broker listens plain HTTP on `:8080`; devices need TLS. Put a reverse proxy in front — Caddy is
the least ceremony:

```caddyfile
displays.example.internal {
    tls internal   # or a real cert; the firmware pins whatever CA you configure at
                   # firmware/main/certs/broker_ca.pem — see docs/BUILD-GUIDE.md

    # Only device-facing paths, plus the now-authenticated /admin and /manager UIs (every route
    # under them is cookie-session-gated — see internal/server/login.go). This mirrors the k3s
    # Ingress rules in backend/deploy/k3s/broker.yaml.example: deliberately no bare "/" catch-all,
    # and /metrics, /status, /api/v1/status are NEVER proxied here — same trust boundary
    # docs/DASHBOARD.md defines for the k3s Ingress. Reach those via `curl localhost:8080/...` on
    # the host itself, or an SSH tunnel, not through this proxy.
    @app path /api/v1/display* /api/v1/telemetry* /firmware* /assets* /admin* /manager* /dashboard* /healthz
    handle @app {
        reverse_proxy localhost:8080
    }
    handle {
        respond 404
    }
}
```

## Uninstall / cleanup

```bash
sudo systemctl disable --now meeting-displays-broker.service wiftoken.timer wiftoken.service
sudo rm /etc/systemd/system/meeting-displays-broker.service /etc/systemd/system/wiftoken.service /etc/systemd/system/wiftoken.timer
sudo systemctl daemon-reload
# /etc/meeting-displays and /var/lib/meeting-displays are left in place deliberately — remove them
# yourself once you've confirmed you don't need the config/telemetry history.
```
