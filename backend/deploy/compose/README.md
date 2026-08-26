# Tier 2: container / compose

One image, `docker compose up`. No Kubernetes, no separate host bootstrap beyond Docker itself.
See [../../../docs/DEPLOY-TIERS.md](../../../docs/DEPLOY-TIERS.md) for how this compares to Tier 1
(systemd) and Tier 3 (k3s).

## What resolves where

- **Config persistence**: auto-detects to `file` (no in-cluster API inside a plain container
  either) — the `./config:/etc/meeting-displays` bind mount is `rw` specifically so this works.
- **Telemetry**: `sqlite` on the named `broker-data` volume — recommended over `memory` since a
  container restart (image update, host reboot) is more routine here than on a systemd host.
- **Google Calendar auth**: the `wiftoken` profile sidecar (see
  [../../../docs/GOOGLE-AUTH.md](../../../docs/GOOGLE-AUTH.md)) — no kubelet inside a container either.

## Quick start

```bash
cp .env.example .env && chmod 0600 .env && $EDITOR .env
mkdir -p config && cp ../../config.tier1.example.yaml config/config.yaml
$EDITOR config/config.yaml           # rooms, users, timezone, ...

# The container runs as uid 65532 (distroless "nonroot"), but the mkdir above created ./config
# owned by you — and config.tier1.example.yaml sets config_persistence.mode: "file" explicitly, so
# there's no "auto" fallback to save you: an unwritable target makes the broker exit non-zero at
# startup (see "Volume ownership" below), not fail quietly at the first admin save.
sudo chown -R 65532:65532 config

docker compose up -d                 # broker only, plain HTTP on :8080

# verify:
curl -s localhost:8080/healthz
curl -s localhost:8080/api/v1/config-persistence   # mode should be "file", durable: true
```

## Two honest caveats

1. **No in-container `HEALTHCHECK`.** The image is `gcr.io/distroless/static-debian12:nonroot` — no
   shell, no `curl`/`wget` to run a healthcheck command from inside the container. Probe from the
   host instead (`curl http://localhost:8080/healthz`), or point an external monitor
   (Uptime Kuma, a cron job, a Tier 1 host's own healthcheck) at the published port.
2. **Volume ownership.** Both `./config` and the named `broker-data` volume must be writable by uid
   `65532` (distroless `nonroot`).
   - `./config` is a plain bind mount: `mkdir -p config` in the quick start above creates it owned
     by whoever ran the command, not uid 65532, and `config.tier1.example.yaml` sets
     `config_persistence.mode: "file"` explicitly — an *explicit* mode never falls back the way
     `auto` would, so an unwritable target makes the broker exit non-zero at startup. Run
     `sudo chown -R 65532:65532 config` once, before the first `docker compose up -d` (already in
     the quick start above).
   - `broker-data` is a named volume; the Dockerfile arranges its ownership by seeding
     `/var/lib/meeting-displays` in the image with `COPY --chown=nonroot:nonroot` — Docker copies a
     brand-new named volume's initial contents (including ownership) from whatever the image
     already had at that mount point. This only fires on a volume's *first* creation. If you're
     reusing a `broker-data` volume created by an older image build, fix ownership once with the
     `fixperms` one-off service in `docker-compose.yml` (the broker image itself is distroless — no
     shell, no `chown` — so this borrows a plain coreutils image instead):
   ```bash
   docker compose run --rm fixperms
   ```

## TLS for devices

```bash
cp Caddyfile.example Caddyfile && $EDITOR Caddyfile   # set your host
docker compose --profile tls up -d
```

## Google Calendar (keyless, no kubelet)

```bash
mkdir -p config/gcp
docker compose run --rm --entrypoint /wiftoken broker -genkey /etc/meeting-displays/gcp/wiftoken.key
docker compose run --rm --entrypoint /wiftoken broker -jwks -key /etc/meeting-displays/gcp/wiftoken.key > jwks.json
#   ... upload jwks.json to GCP, write config/gcp/cred-config.json (see docs/GOOGLE-AUTH.md) ...
docker compose --profile google up -d
```

`config.yaml`'s `google.credentials_file` must point at the `cred-config.json` you write, and that
file's `credential_source.file` must equal the sidecar's `-out` path (`/var/run/gcp/token` in
`docker-compose.yml`, shared with the `broker` service via the `gcp-token` volume).
