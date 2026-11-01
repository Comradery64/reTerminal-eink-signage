#!/usr/bin/env python3
"""Re-sync a display's token hash in the broker config from the token stored in its own NVS.

Use when a unit gets err=auth (401) because the broker's live ConfigMap lost or never had its
token hash (for example after the ConfigMap was replaced). The script reads the unit's NVS
partition over USB, hashes the token in-process (the token is never printed or written, only its
SHA-256), and patches that one room's `token_sha256` in the live ConfigMap. Restart the broker
afterwards, because it does not hot-reload the ConfigMap:

    kubectl -n <namespace> delete pod -l app=broker

Usage (the port is required; use the Python that has esptool, e.g. the ESP-IDF venv):

    python tools/recover_device_token.py /dev/cu.usbserial-110

Environment: KUBECONFIG as usual; MD_NAMESPACE (default meeting-displays) and MD_CONFIGMAP
(default broker-config) if your install differs.

Limits:
  * Only works on a unit whose NVS is NOT encrypted. A release-mode unit (flash/NVS encryption
    on, see docs/SECURITY.md) cannot be read this way; re-provision it with
    tools/provision_device.sh instead.
  * It rewrites the whole `config.yaml` key from a read taken moments earlier, so do not edit the
    config from /admin at the same moment. A timestamped 0600 backup of the ConfigMap (it holds
    live token hashes and 2FA secrets) is written to the current directory first; delete it when
    you are done.
"""
import datetime
import hashlib
import json
import os
import re
import struct
import subprocess
import sys
import tempfile

NS = os.environ.get("MD_NAMESPACE", "meeting-displays")
CM = os.environ.get("MD_CONFIGMAP", "broker-config")
NVS_OFFSET, NVS_SIZE = "0x9000", "0x6000"  # firmware/partitions.csv


def read_nvs(port):
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "nvs.bin")
        subprocess.run([sys.executable, "-m", "esptool", "--port", port, "--baud", "460800",
                        "read_flash", NVS_OFFSET, NVS_SIZE, out], check=True, capture_output=True)
        with open(out, "rb") as f:
            return f.read()


def parse(b):
    """Minimal NVS v2 parser: returns the string entries named device_id / token."""
    found = {}
    for p in range(0, len(b), 4096):
        pg = b[p:p + 4096]
        if pg[:4] == b"\xff" * 4:
            continue
        i = 0
        while i < 126:
            state = (pg[32 + i // 4] >> ((i % 4) * 2)) & 3
            e = pg[64 + i * 32:96 + i * 32]
            if state == 2 and e[1] == 0x21:  # written, string
                key = e[8:24].split(b"\0")[0].decode()
                size = struct.unpack("<H", e[24:26])[0]
                if key in ("device_id", "token"):
                    found[key] = pg[96 + i * 32:96 + i * 32 + size].rstrip(b"\0").decode()
                i += max(e[2], 1)
                continue
            i += 1
    return found


# One room's token_sha256, matched without crossing into the next room, and whether or not the
# YAML quotes its scalars (the broker rewrites the file unquoted when /admin saves).
def _room_re(dev):
    return re.compile(
        r"(device_id:\s*[\"']?%s[\"']?\s*\n(?:(?!\n\s*-\s|device_id:)[\s\S])*?token_sha256:\s*[\"']?)([0-9a-f]+)([\"']?)"
        % re.escape(dev))


def current_hash(text, dev):
    m = _room_re(dev).search(text)
    return m.group(2) if m else None


def resub(text, dev, h):
    new, n = _room_re(dev).subn(lambda m: m.group(1) + h + m.group(3), text, count=1)
    if n != 1:
        sys.exit(f"{dev} not found in the config")
    return new


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    kv = parse(read_nvs(sys.argv[1]))
    dev, tok = kv.get("device_id"), kv.get("token", "")
    if not dev or not tok:
        sys.exit(f"NVS is missing keys (found {len(kv)} of device_id/token); is it encrypted?")
    h = hashlib.sha256(tok.encode()).hexdigest()
    del tok

    raw = subprocess.run(["kubectl", "-n", NS, "get", "cm", CM, "-o", "json"],
                         check=True, capture_output=True, text=True).stdout
    live = json.loads(raw)["data"]["config.yaml"]
    if current_hash(live, dev) == h:
        print(f"{dev}: live hash already correct")
        return
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%MZ")
    backup = f"{CM}.backup-{stamp}.json"
    fd = os.open(backup, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(raw)
    subprocess.run(["kubectl", "-n", NS, "patch", "cm", CM, "--type", "merge", "-p",
                    json.dumps({"data": {"config.yaml": resub(live, dev, h)}})],
                   check=True, capture_output=True)
    print(f"{dev}: live ConfigMap updated (backup {backup}, mode 600, delete it when done)")
    print(f"restart the broker once all units are done: kubectl -n {NS} delete pod -l app=broker")


if __name__ == "__main__":
    main()
