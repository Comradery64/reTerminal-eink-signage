# ── build ─────────────────────────────────────────────────────────────────────
# Pin the build stage to the BUILD host's arch so the Go toolchain runs natively (fast),
# then cross-compile to the TARGET arch via GOARCH — CGO is off, so no emulation needed.
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# Static, stripped binary. CGO off → scratch-friendly. Cross-compiles for $TARGETOS/$TARGETARCH.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /broker ./cmd/broker

# ── runtime ───────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /broker /broker
# tz database for business-hours logic
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /zoneinfo.zip
ENV ZONEINFO=/zoneinfo.zip
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/broker", "-config", "/etc/meeting-displays/config.yaml"]
