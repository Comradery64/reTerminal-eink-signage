# reTerminal E1002 — verified hardware reference

The firmware HAL (`firmware/main/config.hpp`, `Kconfig.projbuild`, `epd_spectra6.cpp`,
`power.cpp`, `partitions.csv`) is reconciled against the sources below. This is **not** a fork of
Seeed's firmware — it's an independent ESP-IDF implementation. Pin assignments were verified
against the **official schematic** (V1.2, 251120) and cross-checked with the ESP32-S3 datasheet,
the Seeed_GxEPD2 Arduino example, the ESPHome cookbook, and the Zephyr board port. **Where the
schematic and the wiki disagreed, the schematic wins** (see the battery pin note below).

## SoC / memory

| | |
|---|---|
| SoC | ESP32-S3R8 |
| Flash | 32 MB |
| PSRAM | 8 MB, **Octal (OPI)** — `CONFIG_SPIRAM_MODE_OCT` |
| Panel | 7.3" E Ink Spectra 6 (6-color), **Good Display GDEP073E01**, **ED2208** controller, 800×480 |

## Pin map (verified against the schematic, V1.2 251120)

| Signal | GPIO | Schematic net |
|---|---|---|
| EPD SCLK | 7 | `ESP_IO7/SCK` |
| EPD MOSI | 9 | `ESP_IO9/MOSI` |
| EPD CS | 10 | `ESP_IO10/SCREEN_CS#` |
| EPD DC | 11 | `ESP_IO11/SCREEN_DC#` |
| EPD RST | 12 | `ESP_IO12/SCREEN_RST#` |
| EPD BUSY | 13 | `ESP_IO13/SCREEN_BUSY#` |
| microSD CS / EN (shared SPI bus, MISO=GPIO8; unused by panel) | 14 / 16 | `ESP_IO14/SD_CS`, `ESP_IO16/SD_EN` |
| Wake button KEY0 | 3 | `ESP_IO3/KEY0` (active-low, pull-up) |
| Wake button KEY1 | 4 | `ESP_IO4/KEY1` |
| Wake button KEY2 | 5 | `ESP_IO5/KEY2` |
| **Battery sense ADC** | **1 (ADC1_CH0)** | `ESP_IO1/VBAT_ADC` |
| **Battery divider enable** | **21** | `ESP_IO21/VBAT_EN` (drive HIGH to measure) |
| Buzzer enable | 45 | `ESP_IO45/BUZZER_EN` |

> ⚠️ **Battery pin correction:** the Seeed *wiki* says battery monitoring is on GPIO2; the
> *schematic* shows GPIO2 is just a general-purpose ADC on the expansion header, and the real
> battery net `VBAT_ADC` is on **GPIO1**, gated by **VBAT_EN (GPIO21)**. The firmware uses the
> schematic values. Reading GPIO2 (per the wiki) without asserting VBAT_EN would return garbage.

There is **no software panel power-gate GPIO** (confirmed: no `SCREEN_EN`/`EPD_EN` net exists);
low sleep current comes from the controller's hibernate command (`0x07 0xA5`).

### Touch & other peripherals (from the schematic)

- **Capacitive touch controller**: `TOUCH_INT=GPIO47`, `TOUCH_RES=GPIO48` (I²C). **Cannot wake
  from deep sleep** — per the ESP32-S3 datasheet, only RTC_GPIO0–21 (i.e. GPIO0–GPIO21) are
  RTC-domain pins usable for ext0/ext1 wake. GPIO47/48 are not. So "tap to wake" is served by the
  three buttons (GPIO3/4/5), all of which *are* RTC-capable. Touch can be read while awake.
- **PCF8563 RTC** (I²C 0x51) and a **temp/humidity sensor** (I²C 0x44, likely SHT4x) on I²C1
  (SCL=GPIO40, SDA=GPIO39). The temp/humidity sensor is a free add for room-environment telemetry.

## Panel driver (GDEP073E01 / ED2208) — from the reference driver

- **Reset**: RST high 50 ms → low 20 ms → high 10 ms → wait BUSY.
- **Init**: `0xAA`,`0x01`,`0x00`,`0x03`,`0x05`,`0x06`,`0x08`,`0x30`,`0x50`,`0x60`,`0x61`,`0x84`,`0xE3`
  (exact data bytes in `epd_spectra6.cpp::power_up`).
- **Power on** `0x04` (BUSY released ~1 s). **Data** `0x10`. **Refresh** `0x50 0x3F` then `0x12 0x00`
  (long BUSY wait). **Power off** `0x02 0x00`. **Hibernate** `0x07 0xA5`.
- **BUSY is active-LOW** (held low while working, high when ready) — we wait until high.
- **Pixels**: 4 bpp, 2 px/byte, high nibble = left pixel. Codes: Black `0x0`, White `0x1`,
  Yellow `0x2`, Red `0x3`, Orange `0x4`, Blue `0x5`, Green `0x6`. (Backend uses the 6 Spectra colors;
  matches `render/spectra6.go`.)

## Still to confirm on your bench unit (recoverable — no eFuses involved)

1. **Battery divider ratio** (`cfg::BATT_DIVIDER`, set to 2.0 = equal-resistor half divider) —
   I could read `VBAT_ADC`/`VBAT_EN`/`100K 1%` off the schematic but not the exact second-resistor
   value from the text layer. Confirm the two divider resistor values on schematic sheet ~7, or
   calibrate against a multimeter reading at a known battery voltage.
2. **BUSY polarity** — coded active-low → ready-high (per the reference driver). If the first
   refresh hangs in `wait_busy`, flip `level_ready` to 0.
3. **Full-refresh duration** — `wait_busy` timeout is 40 s; confirm the real Spectra 6 refresh time
   and tighten if desired.

## Sources

- **Official schematic V1.2 (251120)** — https://files.seeedstudio.com/wiki/reterminal_e10xx/res/202004321_reTerminal_E1002_V1_2_SCH_251120.pdf *(read in full — authoritative for all pins/nets)*
- **ESP32-S3 datasheet** — https://files.seeedstudio.com/wiki/SeeedStudio-XIAO-ESP32S3/res/esp32-s3_datasheet.pdf *(RTC_GPIO0–21 range → confirms touch GPIO47/48 cannot wake from deep sleep)*
- Getting Started — https://wiki.seeedstudio.com/getting_started_with_reterminal_e1002/
- Arduino / Seeed_GxEPD2 cookbook — https://wiki.seeedstudio.com/reterminal_e10xx_with_arduino/
- ESPHome buttons/battery/low-power cookbook — https://wiki.seeedstudio.com/reterminal_e10xx_with_esphome_advanced/
- Zephyr board port — https://docs.zephyrproject.org/latest/boards/seeed/reterminal_e1002/doc/index.html
- GxEPD2 GDEP073E01 driver — https://github.com/ZinggJM/GxEPD2 (`src/epd7c/GxEPD2_730c_GDEP073E01.cpp`)
- SenseCraft HMI overview — https://wiki.seeedstudio.com/sensecraft_hmi_overview/ *(reviewed: cloud no-code dashboard builder with predefined integrations; not suitable for a self-hosted, battery-optimized custom-firmware path — confirms our approach)*
- STEP 3D model (`reterminal_esp-250904.stp`) — mechanical CAD geometry only; **no electrical/pin data**, not used for firmware verification.
