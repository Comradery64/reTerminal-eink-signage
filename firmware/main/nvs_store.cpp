#include "nvs_store.hpp"
#include "nvs_flash.h"
#include "nvs.h"
#include "esp_log.h"

namespace store {
static const char* TAG = "nvs";

esp_err_t init() {
    esp_err_t err = nvs_flash_init();
    if (err == ESP_ERR_NVS_NO_FREE_PAGES || err == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_LOGW(TAG, "nvs partition needs erase (%s)", esp_err_to_name(err));
        ESP_ERROR_CHECK(nvs_flash_erase());
        err = nvs_flash_init();
    }
    return err;
}

static bool get_str(const char* ns, const char* key, char* out, size_t cap) {
    nvs_handle_t h;
    if (nvs_open(ns, NVS_READONLY, &h) != ESP_OK) return false;
    size_t len = cap;
    esp_err_t e = nvs_get_str(h, key, out, &len);
    nvs_close(h);
    return e == ESP_OK;
}

bool get_wifi_ssid(char* o, size_t c)    { return get_str("prov", "wifi_ssid", o, c); }
bool get_wifi_psk(char* o, size_t c)     { return get_str("prov", "wifi_psk", o, c); }
bool get_device_token(char* o, size_t c) { return get_str("prov", "token", o, c); }
bool get_device_id(char* o, size_t c)    { return get_str("prov", "device_id", o, c); }
bool get_eap_identity(char* o, size_t c) { return get_str("prov", "eap_id", o, c); }
bool get_eap_username(char* o, size_t c) { return get_str("prov", "eap_user", o, c); }
bool get_eap_password(char* o, size_t c) { return get_str("prov", "eap_pass", o, c); }
bool get_client_cert(char* o, size_t c)  { return get_str("prov", "client_cert", o, c); }
bool get_client_key(char* o, size_t c)   { return get_str("prov", "client_key", o, c); }

bool has_client_cert() {
    nvs_handle_t h;
    if (nvs_open("prov", NVS_READONLY, &h) != ESP_OK) return false;
    size_t len = 0;                                   // presence test only
    esp_err_t e = nvs_get_str(h, "client_cert", nullptr, &len);
    nvs_close(h);
    return e == ESP_OK && len > 1;
}

bool is_enterprise() {
    nvs_handle_t h;
    if (nvs_open("prov", NVS_READONLY, &h) != ESP_OK) return false;
    size_t len = 0;                                   // query required size; presence test only
    esp_err_t e = nvs_get_str(h, "eap_id", nullptr, &len);
    nvs_close(h);
    return e == ESP_OK && len > 1;
}

bool load_tls_session(uint8_t* buf, size_t cap, size_t* out_len) {
    nvs_handle_t h;
    if (nvs_open("rt", NVS_READONLY, &h) != ESP_OK) return false;
    size_t len = cap;
    esp_err_t e = nvs_get_blob(h, "tls_sess", buf, &len);
    nvs_close(h);
    if (e != ESP_OK) return false;
    *out_len = len;
    return true;
}

void save_tls_session(const uint8_t* buf, size_t len) {
    nvs_handle_t h;
    if (nvs_open("rt", NVS_READWRITE, &h) != ESP_OK) return;
    nvs_set_blob(h, "tls_sess", buf, len);
    nvs_commit(h);
    nvs_close(h);
}

void clear_tls_session() {
    nvs_handle_t h;
    if (nvs_open("rt", NVS_READWRITE, &h) != ESP_OK) return;
    nvs_erase_key(h, "tls_sess");
    nvs_commit(h);
    nvs_close(h);
}

} // namespace store
