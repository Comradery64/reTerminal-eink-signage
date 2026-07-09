# Minimal internal NTP server (chrony) for the meeting-displays fleet.
# Built from source (alpine + distro chrony) into the in-cluster registry — no third-party NTP image.
FROM alpine:3.20
RUN apk add --no-cache chrony
COPY chrony.conf /etc/chrony/chrony.conf
# Unprivileged: serve on a high port (12300); the Service maps :123 → :12300. Runs as nobody, no caps.
# -x = never touch the clock (server-only); -d = foreground; -u = drop to this user.
USER 65534:65534
ENTRYPOINT ["chronyd", "-d", "-x", "-f", "/etc/chrony/chrony.conf"]
