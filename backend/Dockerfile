# ── build ─────────────────────────────────────────────────────────────────────
# Pin the build stage to the BUILD host's arch so the Go toolchain runs natively (fast),
# then cross-compile to the TARGET arch via GOARCH — CGO is off, so no emulation needed.
# 1.24-alpine (not 1.22): modernc.org/sqlite (the pure-Go telemetry backend, see
# internal/telemetry) declares `go 1.24.0` in its own go.mod, and go.mod's directive here follows
# suit — an older toolchain simply refuses to build a module that declares a newer requirement.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# Static, stripped binary. CGO off → scratch-friendly (also required: modernc.org/sqlite is a
# pure-Go driver specifically so this stays true even once the sqlite telemetry backend is in
# use). Cross-compiles for $TARGETOS/$TARGETARCH.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /broker ./cmd/broker
# Second binary: the on-prem WIF token minter (Tier 1/2 Google Calendar auth, see
# docs/GOOGLE-AUTH.md). It ships in the same image so Tier 2's `google`-profile sidecar can run it
# from this exact image with no separate build/pull.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /wiftoken ./tools/wiftoken
# Seed an empty, correctly-owned state directory so the named volume it's mounted over (Tier 2's
# `broker-data:/var/lib/meeting-displays`) inherits uid 65532 ownership on first `docker compose
# up` — Docker copies a new volume's initial contents (incl. ownership) from whatever was already
# at the mount point in the image, so without this the sqlite telemetry backend can't create its
# file under the distroless nonroot user (no shell, no chown, no entrypoint script to fix it up).
RUN mkdir -p /out/state

# ── runtime ───────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /broker /broker
COPY --from=build /wiftoken /wiftoken
COPY --from=build --chown=nonroot:nonroot /out/state /var/lib/meeting-displays
# tz database for business-hours logic
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /zoneinfo.zip
ENV ZONEINFO=/zoneinfo.zip
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/broker", "-config", "/etc/meeting-displays/config.yaml"]
