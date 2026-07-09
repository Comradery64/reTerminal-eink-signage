#pragma once
#include "esp_err.h"

namespace wifinet {

// Connect using RTC-cached BSSID/channel when available (skips the active scan, ~1.5-2.5 s
// saved per wake). On success the new BSSID/channel are written back into RTC state.
// Blocks up to WIFI_CONNECT_TIMEOUT_MS. Returns ESP_OK on GOT_IP.
esp_err_t connect();

void disconnect();   // graceful: esp_wifi_disconnect + stop, lets radio power down before sleep

int  last_rssi();

} // namespace wifinet
