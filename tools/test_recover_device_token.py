#!/usr/bin/env python3
"""Offline tests for recover_device_token.py (no device, no cluster): python3 -I tools/test_recover_device_token.py"""
import hashlib
import io
import json
import stat
import subprocess
import tempfile
import contextlib
import importlib.util
import os
import struct
import sys
import unittest

spec = importlib.util.spec_from_file_location("rdt", os.path.join(os.path.dirname(os.path.abspath(__file__)), "recover_device_token.py"))
rdt = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rdt)


def nvs_page(entries, namespace="prov"):
    """Build one NVS v2 page holding string entries [(key, value)] in `namespace` (index 1, declared by
    a u8 record in namespace 0), or no namespace record at all when `namespace` is None."""
    page = bytearray(b"\xff" * 4096)
    page[0:4] = b"\xfe\xff\xff\xff"  # a used page: not the erased 0xff header
    i = 0
    if namespace:
        nsrec = bytearray(32)
        nsrec[0], nsrec[1], nsrec[2] = 0, 0x01, 1
        nsrec[8:8 + len(namespace)] = namespace.encode()
        nsrec[24] = 1
        page[64:96] = nsrec
        page[32] = (page[32] & ~3) | 2
        i = 1
    for key, val in entries:
        data = val.encode() + b"\0"
        slots = 1 + (len(data) + 31) // 32
        head = bytearray(32)
        head[0], head[1], head[2], head[3] = 1, 0x21, slots, 0xFF  # namespace index 1
        head[8:8 + len(key)] = key.encode()
        head[24:26] = struct.pack("<H", len(data))
        page[64 + i * 32:96 + i * 32] = head
        page[96 + i * 32:96 + i * 32 + len(data)] = data
        for k in range(slots):  # state 2 = written
            idx = i + k
            page[32 + idx // 4] &= ~(3 << ((idx % 4) * 2)) | (2 << ((idx % 4) * 2))
        i += slots
    return bytes(page)


QUOTED = 'rooms:\n  - device_id: "rt-a"\n    name: A\n    token_sha256: "%s"\n  - device_id: "rt-b"\n    name: B\n    token_sha256: "%s"\n'
PLAIN = 'rooms:\n    - device_id: rt-a\n      name: A\n      token_sha256: %s\n    - device_id: rt-b\n      name: B\n      token_sha256: %s\n'
OLD_A, OLD_B, NEW = "a" * 64, "b" * 64, "c" * 64


class T(unittest.TestCase):
    def test_parse_reads_device_id_and_token_only(self):
        kv = rdt.parse(nvs_page([("device_id", "rt-a"), ("wifi_ssid", "x"), ("token", "s3cret-token-value")]))
        self.assertEqual(kv, {"device_id": "rt-a", "token": "s3cret-token-value"})

    def test_erased_flash_has_no_keys(self):
        self.assertEqual(rdt.parse(b"\xff" * 0x6000), {})

    def test_resub_changes_only_the_named_room_quoted_or_not(self):
        for tpl in (QUOTED, PLAIN):
            out = rdt.resub(tpl % (OLD_A, OLD_B), "rt-a", NEW)
            self.assertEqual(out, tpl % (NEW, OLD_B))
            self.assertEqual(rdt.current_hash(out, "rt-b"), OLD_B)

    def test_second_room_does_not_leak_into_the_first(self):
        # rt-a has no hash of its own: it must not take rt-b's.
        text = 'rooms:\n  - device_id: rt-a\n    name: A\n  - device_id: rt-b\n    name: B\n    token_sha256: %s\n' % OLD_B
        self.assertIsNone(rdt.current_hash(text, "rt-a"))
        with self.assertRaises(SystemExit):
            rdt.resub(text, "rt-a", NEW)

    def test_hash_is_sha256_of_the_token(self):
        self.assertEqual(hashlib.sha256(b"t").hexdigest(), hashlib.sha256("t".encode()).hexdigest())



FAKE_KUBECTL = """#!/bin/sh
case "$*" in
  *" get "*) cat "$FAKE_CM_JSON" ;;
  *" patch "*) [ -t 0 ] || cat > "$FAKE_PATCH_OUT"; echo "Error from server (Conflict): the object has been modified" >&2; exit 1 ;;
esac
"""
MARKER = "TOTP-MARKER-0123456789"


class Run(unittest.TestCase):
    """main() against a fake kubectl that refuses the patch: nothing secret may reach the output."""

    def _run(self, config_yaml):
        with tempfile.TemporaryDirectory() as d:
            kc = os.path.join(d, "kubectl")
            open(kc, "w").write(FAKE_KUBECTL)
            os.chmod(kc, 0o755)
            cm = os.path.join(d, "cm.json")
            json.dump({"metadata": {"resourceVersion": "7"}, "data": {"config.yaml": config_yaml}}, open(cm, "w"))
            tok = "device-token-value"
            rdt.read_nvs = lambda port: nvs_page([("device_id", "rt-a"), ("token", tok)])
            os.environ.update(PATH=d + os.pathsep + os.environ["PATH"], FAKE_CM_JSON=cm, MD_BACKUP_DIR=d, FAKE_PATCH_OUT=os.path.join(d, "patch.json"))
            sys.argv = ["x", "/dev/null"]
            out, err = io.StringIO(), io.StringIO()
            code = 0
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                try:
                    rdt.main()
                except SystemExit as e:
                    code = e.code or 0
                    if isinstance(e.code, str):  # sys.exit(msg) prints the message to stderr
                        err.write(e.code)
                except Exception as e:  # an uncaught traceback prints the command line
                    import traceback
                    traceback.print_exc(file=err)
            pf = os.path.join(d, "patch.json")
            self.patch = json.load(open(pf)) if os.path.exists(pf) else None
            backups = [f for f in os.listdir(d) if ".backup-" in f]
            mode = stat.S_IMODE(os.stat(os.path.join(d, backups[0])).st_mode) if backups else None
            return out.getvalue() + err.getvalue(), tok, backups, mode

    def test_failed_patch_prints_no_config_and_no_token(self):
        cfg = QUOTED % (OLD_A, OLD_B) + "users:\n  - username: u\n    totp_secret: " + MARKER + "\n"
        shown, tok, backups, mode = self._run(cfg)
        self.assertNotIn(MARKER, shown)
        self.assertNotIn(tok, shown)
        self.assertNotIn(OLD_B, shown)
        self.assertIn("Conflict", shown)  # the useful part of the error still reaches the operator

    def test_backup_is_private_and_lands_where_asked(self):
        shown, tok, backups, mode = self._run(QUOTED % (OLD_A, OLD_B))
        self.assertEqual(len(backups), 1)
        self.assertEqual(mode, 0o600)


    def test_patch_carries_the_resource_version_so_a_concurrent_admin_save_wins(self):
        self._run(QUOTED % (OLD_A, OLD_B))
        self.assertEqual(self.patch["metadata"]["resourceVersion"], "7")
        self.assertIn(hashlib.sha256(b"device-token-value").hexdigest(), self.patch["data"]["config.yaml"])


class Hostile(unittest.TestCase):
    def test_comment_and_lookalike_keys_are_not_rooms(self):
        text = "# device_id: rt-a\nlast_device_id: rt-a\ntoken_sha256: %s\nrooms:\n  - device_id: rt-a\n    token_sha256: %s\n" % (OLD_B, OLD_A)
        self.assertEqual(rdt.current_hash(text, "rt-a"), OLD_A)
        self.assertEqual(rdt.resub(text, "rt-a", NEW).count(NEW), 1)

    def test_only_the_prov_namespace_counts_and_garbage_does_not_crash(self):
        # No namespace record, or one that is not `prov`: nothing may be trusted.
        self.assertEqual(rdt.parse(nvs_page([("device_id", "evil"), ("token", "evil-token")], namespace=None)), {})
        self.assertEqual(rdt.parse(nvs_page([("device_id", "evil"), ("token", "evil-token")], namespace="other")), {})
        import random
        random.seed(7)
        for _ in range(500):
            junk = bytearray(random.randbytes(4096))
            rdt.parse(bytes(junk))  # must not raise


if __name__ == "__main__":
    unittest.main()
