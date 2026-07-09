#include "wifi_fast.hpp"
#include "config.hpp"
#include "app_state.hpp"
#include "nvs_store.hpp"

#include "esp_wifi.h"
#include "esp_event.h"
#include "esp_netif.h"
#include "esp_eap_client.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include <cstring>

namespace wifinet {
static const char* TAG = "wifi";

static EventGroupHandle_t s_events;
static constexpr int GOT_IP_BIT = BIT0;
static constexpr int FAIL_BIT   = BIT1;
static int s_rssi = 0;

static void on_event(void* arg, esp_event_base_t base, int32_t id, void* data) {
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        esp_wifi_connect();
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        // One retry without the cached BSSID before giving up — the AP may have roamed channels.
        auto* d = static_cast<wifi_event_sta_disconnected_t*>(data);
        ESP_LOGW(TAG, "disconnected reason=%d", d->reason);
        xEventGroupSetBits(s_events, FAIL_BIT);
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        wifi_ap_record_t ap;
        if (esp_wifi_sta_get_ap_info(&ap) == ESP_OK) {
            s_rssi = ap.rssi;
            // Persist BSSID/channel for next wake's fast association.
            std::memcpy(g_rtc.bssid, ap.bssid, 6);
            g_rtc.channel = ap.primary;
            g_rtc.bssid_valid = true;
        }
        xEventGroupSetBits(s_events, GOT_IP_BIT);
    }
}

static void apply_enterprise() {
    char id[128], user[128], pass[128];
    if (store::get_eap_identity(id, sizeof id))
        esp_eap_client_set_identity((uint8_t*)id, strlen(id));
    if (store::get_eap_username(user, sizeof user))
        esp_eap_client_set_username((uint8_t*)user, strlen(user));
    if (store::get_eap_password(pass, sizeof pass))
        esp_eap_client_set_password((uint8_t*)pass, strlen(pass));
    // Validate the RADIUS server against the CA bundle in production: ship the CA via
    // esp_eap_client_set_ca_cert(). Left to provisioning. PEAP/MSCHAPv2 is the default.
    esp_wifi_sta_enterprise_enable();
}

esp_err_t connect() {
    s_events = xEventGroupCreate();
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    esp_netif_create_default_wifi_sta();

    wifi_init_config_t init = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&init));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(WIFI_EVENT, ESP_EVENT_ANY_ID, on_event, nullptr, nullptr));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(IP_EVENT, IP_EVENT_STA_GOT_IP, on_event, nullptr, nullptr));
    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));

    // Conserve power: no power-save during the brief connect window would burn current; but
    // modem sleep between TX bursts helps. Use MIN_MODEM (default) — radio sleeps between DTIM.
    esp_wifi_set_ps(WIFI_PS_MIN_MODEM);
    // Don't persist wifi config to flash — saves write cycles every wake.
    esp_wifi_set_storage(WIFI_STORAGE_RAM);

    wifi_config_t wc = {};
    char ssid[33] = {0};
    store::get_wifi_ssid(ssid, sizeof ssid);
    std::strncpy((char*)wc.sta.ssid, ssid, sizeof(wc.sta.ssid));

    const bool enterprise = store::is_enterprise();
    if (!enterprise) {
        char psk[64] = {0};
        store::get_wifi_psk(psk, sizeof psk);
        std::strncpy((char*)wc.sta.password, psk, sizeof(wc.sta.password));
        wc.sta.threshold.authmode = WIFI_AUTH_WPA2_PSK; // accept WPA2/WPA3 transitional
    } else {
        wc.sta.threshold.authmode = WIFI_AUTH_WPA2_ENTERPRISE;
    }

    // Fast-connect: pin BSSID + channel from RTC cache to skip the scan.
    if (g_rtc.bssid_valid) {
        wc.sta.bssid_set = true;
        std::memcpy(wc.sta.bssid, g_rtc.bssid, 6);
        wc.sta.channel = g_rtc.channel;
        ESP_LOGI(TAG, "fast-connect ch=%u bssid=%02x:%02x:..", g_rtc.channel, g_rtc.bssid[0], g_rtc.bssid[1]);
    }
    wc.sta.scan_method = g_rtc.bssid_valid ? WIFI_FAST_SCAN : WIFI_ALL_CHANNEL_SCAN;

    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &wc));
    if (enterprise) apply_enterprise();
    ESP_ERROR_CHECK(esp_wifi_start());

    EventBits_t bits = xEventGroupWaitBits(s_events, GOT_IP_BIT | FAIL_BIT, pdFALSE, pdFALSE,
                                           pdMS_TO_TICKS(cfg::WIFI_CONNECT_TIMEOUT_MS));
    if (bits & GOT_IP_BIT) {
        g_rtc.consecutive_net_failures = 0;
        return ESP_OK;
    }

    // Failed with cached BSSID — invalidate so next wake does a full scan.
    if (g_rtc.bssid_valid) {
        ESP_LOGW(TAG, "fast-connect failed; clearing BSSID cache");
        g_rtc.bssid_valid = false;
    }
    g_rtc.consecutive_net_failures++;
    return ESP_FAIL;
}

void disconnect() {
    esp_wifi_disconnect();
    esp_wifi_stop();
    esp_wifi_deinit();
}

int last_rssi() { return s_rssi; }

} // namespace wifinet
