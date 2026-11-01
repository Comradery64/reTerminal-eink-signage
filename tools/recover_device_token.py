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
  * It rewrites the whole `config.yaml` key from a read taken moments earlier. The patch carries the
    ConfigMap's resourceVersion, so if anyone saved from /admin in between it fails with a conflict
    and changes nothing: just run it again. Restart the broker straight after, or a later /admin
    save from the broker's in-memory copy puts the old hash back.
  * A timestamped 0600 backup of the ConfigMap (it holds live token hashes and 2FA secrets) is
    written to your HOME directory, never the current one, so it cannot be committed by accident
    (override with MD_BACKUP_DIR). Delete it when you are done; do not leave it in a synced or
    backed-up folder.
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


def _entries(b):
    """Yield (namespace_index, type, key, data) for every written entry of an NVS v2 image.
    Malformed or hostile input yields fewer entries; it never raises."""
    for p in range(0, len(b) - 4095, 4096):
        pg = b[p:p + 4096]
        if pg[:4] == b"\xff" * 4:
            continue
        i = 0
        while i < 126:
            state = (pg[32 + i // 4] >> ((i % 4) * 2)) & 3
            e = pg[64 + i * 32:96 + i * 32]
            if state != 2:
                i += 1
                continue
            span = max(e[2], 1)
            key = e[8:24].split(b"\0")[0].decode("ascii", "replace")
            data = None
            if e[1] == 0x21:  # string: the bytes follow in the next slots
                size = struct.unpack("<H", e[24:26])[0]
                start = 96 + i * 32
                if size <= (span - 1) * 32 and start + size <= len(pg):
                    data = pg[start:start + size].rstrip(b"\0")
            elif e[1] == 0x01:  # u8: a namespace record when it lives in namespace 0
                data = e[24:25]
            yield e[0], e[1], key, data
            i += span


def parse(b):
    """Minimal NVS v2 parser: the device_id / token strings of the `prov` namespace only."""
    ents = list(_entries(b))
    prov = {d[0] for ns, t, k, d in ents if ns == 0 and t == 0x01 and k == "prov" and d}
    found = {}
    for ns, t, key, data in ents:
        if t == 0x21 and ns in prov and key in ("device_id", "token") and data is not None:
            try:
                found[key] = data.decode()
            except UnicodeDecodeError:
                continue
    return found


# One room's token_sha256, matched without crossing into the next room, and whether or not the
# YAML quotes its scalars (the broker rewrites the file unquoted when /admin saves). The match
# starts at the beginning of a line (optionally after a list dash), so a comment or a key such as
# `last_device_id:` is never mistaken for the room.
def _room_re(dev):
    return re.compile(
        r"((?m:^)[ \t]*(?:-[ \t]+)?device_id:[ \t]*[\"']?%s[\"']?[ \t]*\n"
        r"(?:(?!\n[ \t]*-[ \t]|[ \t]*(?:-[ \t]+)?device_id:)[\s\S])*?token_sha256:[ \t]*[\"']?)([0-9a-f]+)([\"']?)"
        % re.escape(dev))


def current_hash(text, dev):
    m = _room_re(dev).search(text)
    return m.group(2) if m else None


def resub(text, dev, h):
    new, n = _room_re(dev).subn(lambda m: m.group(1) + h + m.group(3), text, count=1)
    if n != 1:
        sys.exit(f"{dev} not found in the config")
    return new


def kubectl(args, stdin=None):
    """Run kubectl; on failure exit with its exit code and stderr only. The CalledProcessError
    text would repeat the whole command line, which is why the patch is sent on stdin."""
    r = subprocess.run(["kubectl", *args], input=stdin, capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit(f"kubectl {args[2] if len(args) > 2 else ''} failed (exit {r.returncode}): {r.stderr.strip()[:400]}")
    return r.stdout


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    kv = parse(read_nvs(sys.argv[1]))
    dev, tok = kv.get("device_id"), kv.get("token", "")
    if not dev or not tok:
        sys.exit(f"NVS is missing the provisioned device_id/token (found {len(kv)} of 2); is it encrypted?")
    h = hashlib.sha256(tok.encode()).hexdigest()
    del tok

    raw = kubectl(["-n", NS, "get", "cm", CM, "-o", "json"])
    cm = json.loads(raw)
    live = cm["data"]["config.yaml"]
    if current_hash(live, dev) == h:
        print(f"{dev}: live hash already correct")
        return
    new = resub(live, dev, h)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%MZ")
    backup = os.path.join(os.environ.get("MD_BACKUP_DIR", os.path.expanduser("~")), f"{CM}.backup-{stamp}.json")
    fd = os.open(backup, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(raw)
    patch = {"metadata": {"resourceVersion": cm["metadata"]["resourceVersion"]}, "data": {"config.yaml": new}}
    kubectl(["-n", NS, "patch", "cm", CM, "--type", "merge", "--patch-file=/dev/stdin"], stdin=json.dumps(patch))
    print(f"{dev}: live ConfigMap updated (backup {backup}, mode 600, delete it when done)")
    print(f"restart the broker once all units are done: kubectl -n {NS} delete pod -l app=broker")


if __name__ == "__main__":
    main()
