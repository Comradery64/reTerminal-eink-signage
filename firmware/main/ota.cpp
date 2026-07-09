#include "ota.hpp"
#include "config.hpp"

#include "esp_https_ota.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include <cstring>

// Same pinned internal CA the broker client uses (firmware/main/certs/broker_ca.pem).
extern const char broker_ca_pem_start[] asm("_binary_broker_ca_pem_start");

namespace ota {
static const char* TAG = "ota";

bool maybe_update(const char* target_version, const char* url) {
    if (!target_version || !*target_version || !url || !*url) return false;
    if (std::strcmp(target_version, cfg::FIRMWARE_VERSION) == 0) return false; // already current

    ESP_LOGI(TAG, "OTA available: %s -> %s (%s)", cfg::FIRMWARE_VERSION, target_version, url);

    esp_http_client_config_t http = {};
    http.url = url;
    http.cert_pem = broker_ca_pem_start;              // verify the image host against our internal CA
    http.timeout_ms = 30000;
    http.keep_alive_enable = true;

    esp_https_ota_config_t ota_cfg = {};
    ota_cfg.http_config = &http;

    esp_err_t err = esp_https_ota(&ota_cfg);
    if (err == ESP_OK) {
        ESP_LOGI(TAG, "OTA verified & written; rebooting into %s", target_version);
        esp_restart();   // does not return
    }
    ESP_LOGE(TAG, "OTA failed (%s); staying on %s", esp_err_to_name(err), cfg::FIRMWARE_VERSION);
    return false;
}

} // namespace ota
