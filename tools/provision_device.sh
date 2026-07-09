#!/usr/bin/env bash
# Provision one display: generate a per-device bearer token, build an (optionally encrypted) NVS
# image with the Wi-Fi + token secrets, and print the token SHA-256 to paste into the broker config.
#
# Usage:
#   ./provision_device.sh <device_id> <room_email> <wifi_ssid> [wifi_psk]
#   # WPA2-Enterprise instead of PSK:
#   EAP_ID=user@corp EAP_USER=user EAP_PASS=secret ./provision_device.sh <device_id> <room_email> <ssid>
#   # Optional mTLS (Approach A): embed a step-ca-issued per-device client cert+key into NVS.
#   # See Networking/meeting-displays-broker-access.md Part 2. Issue e.g.:
#   #   step ca certificate <device_id> dev.crt dev.key --provisioner acme|admin ...
#   CLIENT_CERT_FILE=dev.crt CLIENT_KEY_FILE=dev.key ./provision_device.sh <device_id> <room_email> <ssid> <psk>
#
# Requires: esptool/idf (nvs_partition_gen.py on PATH via the IDF export), openssl.
set -euo pipefail

DEVICE_ID="${1:?device_id required}"
ROOM_EMAIL="${2:?room_email required}"
WIFI_SSID="${3:?wifi_ssid required}"
WIFI_PSK="${4:-}"

OUT_DIR="$(mktemp -d)"
NVS_CSV="$OUT_DIR/nvs.csv"
NVS_BIN="$OUT_DIR/nvs-$DEVICE_ID.bin"

# 256-bit URL-safe token.
TOKEN="$(openssl rand -base64 32 | tr -d '=+/' | cut -c1-43)"
TOKEN_SHA="$(printf '%s' "$TOKEN" | openssl dgst -sha256 -hex | awk '{print $2}')"

{
  echo "key,type,encoding,value"
  echo "prov,namespace,,"
  echo "device_id,data,string,$DEVICE_ID"
  echo "token,data,string,$TOKEN"
  echo "wifi_ssid,data,string,$WIFI_SSID"
  if [[ -n "${EAP_ID:-}" ]]; then
    echo "eap_id,data,string,$EAP_ID"
    echo "eap_user,data,string,${EAP_USER:?EAP_USER required for enterprise}"
    echo "eap_pass,data,string,${EAP_PASS:?EAP_PASS required for enterprise}"
  else
    echo "wifi_psk,data,string,${WIFI_PSK:?wifi_psk required for PSK mode}"
  fi
  # Optional mTLS: embed a step-ca-issued per-device client cert + key (Approach A). The firmware
  # (net_client.cpp) presents these automatically when present; absent → server-auth-only TLS.
  if [[ -n "${CLIENT_CERT_FILE:-}" ]]; then
    echo "client_cert,file,string,$CLIENT_CERT_FILE"
    echo "client_key,file,string,${CLIENT_KEY_FILE:?CLIENT_KEY_FILE required when CLIENT_CERT_FILE is set}"
  fi
} > "$NVS_CSV"

# 0x6000 = nvs partition size from partitions.csv.
python "$IDF_PATH/components/nvs_flash/nvs_partition_generator/nvs_partition_gen.py" \
  generate "$NVS_CSV" "$NVS_BIN" 0x6000

cat <<EOF

──────────────────────────────────────────────────────────────────────────────
Provisioned: $DEVICE_ID   (room: $ROOM_EMAIL)
NVS image:   $NVS_BIN

Flash it (offset 0x9000 per partitions.csv):
  esptool.py -p <PORT> write_flash 0x9000 "$NVS_BIN"

If flash encryption is armed, encrypt the image first:
  espsecure.py encrypt_flash_data --aes_xts --keyfile flash_encryption_key.bin \\
    --address 0x9000 -o "$NVS_BIN.enc" "$NVS_BIN"

Add this room to the broker config (token is never stored server-side, only its hash):
  - device_id: "$DEVICE_ID"
    name: "<Display Name>"
    room: "$ROOM_EMAIL"
    token_sha256: "$TOKEN_SHA"
──────────────────────────────────────────────────────────────────────────────
EOF
