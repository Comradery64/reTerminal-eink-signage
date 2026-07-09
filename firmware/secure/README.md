# Secure Boot V2 + Flash Encryption — fleet bring-up

These protect the corporate Wi-Fi credentials and per-device bearer token stored in NVS. Both
are **one-way eFuse operations** — practice on a sacrificial unit first.

## 1. Generate the Secure Boot V2 signing key (once, kept offline)

```bash
espsecure.py generate_signing_key --version 2 --scheme rsa3072 secure_boot_signing_key.pem
```

Store this in your HSM / secrets vault. Anyone with it can sign firmware the fleet will trust.
The public-key-hash digest is burned into eFuse on first secure boot; the private key never
touches the device.

## 2. Enable in `sdkconfig.defaults`

Uncomment the `CONFIG_SECURE_BOOT*` and `CONFIG_SECURE_FLASH_ENC*` blocks (already templated).
Use **`CONFIG_SECURE_FLASH_ENCRYPTION_MODE_RELEASE`** for production — development mode leaves a
plaintext re-flash path open.

## 3. NVS encryption key

`partitions.csv` declares an `nvs_keys` partition (`encrypted` flag). With flash encryption on,
the XTS key for the NVS partition is itself stored encrypted. Generate it:

```bash
python "$IDF_PATH/components/nvs_flash/nvs_partition_generator/nvs_partition_gen.py" \
  generate-key --keyfile nvs_keys-<device>.bin
```

## 4. First-boot flow (per device, on the assembly bench)

```bash
idf.py build
idf.py -p <PORT> bootloader-flash       # signed bootloader
idf.py -p <PORT> flash                   # signed app + partitions
./tools/provision_device.sh <id> <room@corp> <ssid> <psk>   # builds NVS image
esptool.py -p <PORT> write_flash 0x9000 nvs-<id>.bin
```

On first boot the device burns the secure-boot digest + flash-encryption key eFuses and
re-encrypts flash in place. **After this, UART download mode can no longer read plaintext flash.**

## 5. Verify before shipping the unit

```bash
espefuse.py -p <PORT> summary | grep -E "SECURE_BOOT_EN|SPI_BOOT_CRYPT_CNT|DIS_DOWNLOAD"
```

`SECURE_BOOT_EN = 1` and `SPI_BOOT_CRYPT_CNT` set (odd parity) confirm both are active. For a
fleet, also burn `DIS_DOWNLOAD_MODE` (release mode does this) so a stolen unit can't be coerced
into download mode.

## Rotation / decommission

- **Token rotation**: re-run `provision_device.sh`, re-flash NVS at 0x9000 (encrypted), update the
  `token_sha256` in the broker config, restart the broker. No firmware rebuild needed.
- **Decommission**: revoke by removing the room from the broker config — the token hash no longer
  matches anything and `/display` returns 401.
