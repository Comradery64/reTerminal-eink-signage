# Broker CA certificate

Drop your broker's CA root certificate here as `broker_ca.pem` (PEM format) before building.

The build embeds this file via `EMBED_TXTFILES` in `firmware/main/CMakeLists.txt` and the
firmware pins it at TLS connect time (`net_client.cpp`) — the device fail-closed rejects any
broker cert not signed by this root. This is the only TLS trust path the firmware supports
today; there's no Mozilla-bundle fallback, so **the build will not succeed until you supply
this file.**

- If your broker's certificate is issued by a public CA (e.g. Let's Encrypt), place that CA's
  root certificate here.
- If you run your own internal CA (step-ca, a private OpenSSL CA, etc.), export its root
  certificate and place it here instead.

This file is intentionally **not** committed — every deployment pins a different CA.
