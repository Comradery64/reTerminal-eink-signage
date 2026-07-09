// Compile-time + provisioned device configuration.
//
// Secrets (Wi-Fi PSK / enterprise creds, device bearer token) are NOT hard-coded here in
// production — they are written to the encrypted NVS "prov" namespace by tools/provision_device.sh
// and read at boot (see nvs_store). The kconfig fallbacks below are for bench bring-up only.
#pragma once
#include <cstdint>
#include <cstddef>   // size_t (used by EPD_RAW_LEN); config.hpp is included before any header that defines it
#include "sdkconfig.h"

namespace cfg {

// ── Broker ──────────────────────────────────────────────────────────────────
constexpr const char* BROKER_HOST = CONFIG_MD_BROKER_HOST;      // e.g. "displays.example.internal"
constexpr uint16_t    BROKER_PORT = CONFIG_MD_BROKER_PORT;      // 443
constexpr const char* SNTP_SERVER = CONFIG_MD_SNTP_SERVER;     // your NTP server, set before first TLS fetch
constexpr const char* DISPLAY_PATH_PREFIX   = "/api/v1/display/";
constexpr const char* TELEMETRY_PATH_PREFIX = "/api/v1/telemetry/";

// ── Power / timing ────────────────────────────────────────────────────────────
constexpr uint32_t DEFAULT_WAKE_INTERVAL_S = 600;   // used until server sends X-Next-Wake
constexpr uint32_t MAX_WAKE_INTERVAL_S     = 6 * 3600;
constexpr uint32_t MIN_WAKE_INTERVAL_S     = 60;
constexpr uint32_t NET_BUDGET_MS           = 8000;  // hard cap on radio-on time per wake
constexpr uint32_t WIFI_CONNECT_TIMEOUT_MS = 6000;

// ── Display geometry (7.3" Spectra 6 — Good Display GDEP073E01, ED2208 controller) ──
constexpr int      EPD_WIDTH    = 800;
constexpr int      EPD_HEIGHT   = 480;
constexpr size_t   EPD_RAW_LEN  = (EPD_WIDTH * EPD_HEIGHT) / 2;  // 4bpp packed = 192000

// ── reTerminal E1002 pin map ────────────────────────────────────────────────
// Verified against Seeed's Arduino Seeed_GxEPD2 example (GxEPD2_730c_GDEP073E01) and the
// ESPHome cookbook. The ePaper SPI bus is SHARED with the microSD reader (MISO=GPIO8, which
// the panel does not use — it is write-only). We never init the SD card, so no contention.
constexpr int PIN_EPD_SCLK  = CONFIG_MD_PIN_EPD_SCLK;  // GPIO7
constexpr int PIN_EPD_MOSI  = CONFIG_MD_PIN_EPD_MOSI;  // GPIO9
constexpr int PIN_EPD_CS    = CONFIG_MD_PIN_EPD_CS;    // GPIO10
constexpr int PIN_EPD_DC    = CONFIG_MD_PIN_EPD_DC;    // GPIO11
constexpr int PIN_EPD_RST   = CONFIG_MD_PIN_EPD_RST;   // GPIO12
constexpr int PIN_EPD_BUSY  = CONFIG_MD_PIN_EPD_BUSY;  // GPIO13
// NOTE: the E1002 has NO software panel power-gate GPIO. Low panel sleep current is achieved
// via the controller's own hibernate command (0x07/0xA5), exactly as the reference driver does.

// Wake buttons (schematic nets KEY0/KEY1/KEY2, active-LOW with pull-up). All three are
// RTC-capable GPIOs, so any of them can wake the device from deep sleep via ext1.
// NOTE: the panel also has a capacitive touch controller (TOUCH_INT=GPIO47, TOUCH_RES=GPIO48,
// I2C) but GPIO47/48 are NOT RTC-capable — touch CANNOT wake from deep sleep. Buttons only.
constexpr int PIN_WAKE_BTN_A = CONFIG_MD_PIN_WAKE_BTN_A;  // GPIO3 (KEY0)
constexpr int PIN_WAKE_BTN_B = CONFIG_MD_PIN_WAKE_BTN_B;  // GPIO4 (KEY1)
constexpr int PIN_WAKE_BTN_C = CONFIG_MD_PIN_WAKE_BTN_C;  // GPIO5 (KEY2)

// Battery sense (schematic): VBAT_ADC on GPIO1, fed by a divider GATED by VBAT_EN on GPIO21.
// VBAT_EN must be driven HIGH before reading the ADC, then LOW again to stop the divider drain.
constexpr int PIN_BATT_ADC   = CONFIG_MD_PIN_BATT_ADC;    // GPIO1 = VBAT_ADC (ADC1_CH0)
constexpr int PIN_VBAT_EN     = CONFIG_MD_PIN_VBAT_EN;    // GPIO21 = VBAT_EN (active-high)

// Divider ratio: confirm the exact resistor values on the schematic and/or calibrate against
// Seeed's ESPHome battery multiplier. 2.0 = equal-resistor half divider (typical).
constexpr float BATT_DIVIDER  = 2.0f;
constexpr int   BATT_FULL_MV  = 4150;
constexpr int   BATT_EMPTY_MV = 3300;

// On-board I2C1 peripherals (schematic): SHT4x temp/humidity sensor + PCF8563 RTC.
constexpr int     PIN_I2C1_SDA = 39;
constexpr int     PIN_I2C1_SCL = 40;
constexpr uint8_t SHT4X_ADDR   = 0x44;

constexpr const char* FIRMWARE_VERSION = "1.0.0";

} // namespace cfg
