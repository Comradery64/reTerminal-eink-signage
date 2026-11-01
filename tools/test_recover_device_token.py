#!/usr/bin/env python3
"""Offline tests for recover_device_token.py (no device, no cluster): python3 -I tools/test_recover_device_token.py"""
import hashlib
import importlib.util
import os
import struct
import sys
import unittest

spec = importlib.util.spec_from_file_location("rdt", os.path.join(os.path.dirname(os.path.abspath(__file__)), "recover_device_token.py"))
rdt = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rdt)


def nvs_page(entries):
    """Build one NVS v2 page holding string entries [(key, value)]; each takes a header + data slot."""
    page = bytearray(b"\xff" * 4096)
    page[0:4] = b"\xfe\xff\xff\xff"  # a used page: not the erased 0xff header
    i = 0
    for key, val in entries:
        data = val.encode() + b"\0"
        slots = 1 + (len(data) + 31) // 32
        head = bytearray(32)
        head[0], head[1], head[2], head[3] = 1, 0x21, slots, 0xFF
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


if __name__ == "__main__":
    unittest.main()
