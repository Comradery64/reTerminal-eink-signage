// Conference-room display firmware — strict deep-sleep state machine.
//
//   Wake (timer | touch | cold boot)
//     → fast Wi-Fi associate (cached BSSID)
//     → TLS connect (resumed session ticket)
//     → GET /display with If-None-Match
//         304  → DO NOT touch the panel (saves the dominant refresh energy)
//         200  → decode + full Spectra 6 refresh
//     → POST /telemetry
//     → deep sleep until server-directed next wake
//
// The device is awake for ~1-2 s on a no-change wake, ~15-30 s only when the panel actually
// needs to redraw. See docs/POWER.md for the battery budget.

#include "config.hpp"
#include "app_state.hpp"
#include "nvs_store.hpp"
#include "wifi_fast.hpp"
#include "net_client.hpp"
#include "epd_spectra6.hpp"
#include "power.hpp"
#include "telemetry.hpp"
#include "sensors.hpp"
#include "ota.hpp"

#include "esp_log.h"
#include "esp_timer.h"
#include "esp_heap_caps.h"
#include "esp_netif_sntp.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include <cstring>
#include <ctime>
#include <sys/time.h>

RTC_DATA_ATTR RtcState g_rtc;   // single definition (declared extern in app_state.hpp)

static const char* TAG = "app";

static uint32_t clamp_wake(uint32_t s) {
    if (s == 0) return cfg::DEFAULT_WAKE_INTERVAL_S;
    if (s < cfg::MIN_WAKE_INTERVAL_S) return cfg::MIN_WAKE_INTERVAL_S;
    if (s > cfg::MAX_WAKE_INTERVAL_S) return cfg::MAX_WAKE_INTERVAL_S;
    return s;
}

// TLS leaf certs carry a real notBefore, so mbedTLS verify fails (BADCERT_FUTURE) until the clock
// is set. The RTC keeps time across deep sleep, so we sync only when the clock is clearly unset (cold
// boot). NTP source is cfg::SNTP_SERVER (menuconfig) — point it at whatever NTP server your device's
// network can reach. Must run AFTER wifi is up.
static bool time_is_valid() {
    time_t now = 0; time(&now);
    return now > 1700000000;   // ~2023-11; anything earlier means the clock was never synced
}
static bool sync_time_if_needed() {
    if (time_is_valid()) return true;
    esp_sntp_config_t sc = ESP_NETIF_SNTP_DEFAULT_CONFIG(cfg::SNTP_SERVER);
    if (esp_netif_sntp_init(&sc) != ESP_OK) return false;
    bool ok = (esp_netif_sntp_sync_wait(pdMS_TO_TICKS(6000)) == ESP_OK) && time_is_valid();
    esp_netif_sntp_deinit();
    if (ok) {
        time_t n = 0; time(&n);
        ESP_LOGI(TAG, "time synced via %s (epoch=%ld)", cfg::SNTP_SERVER, (long)n);
    } else {
        ESP_LOGE(TAG, "SNTP sync failed — is %s reachable/serving NTP?", cfg::SNTP_SERVER);
    }
    return ok;
}

extern "C" void app_main(void) {
    const int64_t t0 = esp_timer_get_time();

    rtc_state_ensure_init();
    g_rtc.boot_count++;
    const auto wake = power::wake_reason();
    ESP_LOGI(TAG, "wake #%u reason=%s", g_rtc.boot_count, power::wake_reason_str(wake));

    ESP_ERROR_CHECK(store::init());

    char device_id[48] = {0}, token[96] = {0};
    if (!store::get_device_id(device_id, sizeof device_id) ||
        !store::get_device_token(token, sizeof token)) {
        ESP_LOGE(TAG, "device not provisioned — sleeping long");
        power::deep_sleep(cfg::MAX_WAKE_INTERVAL_S);
    }

    bool rendered = false;
    const char* err = nullptr;
    uint32_t next_wake = clamp_wake(g_rtc.next_wake_s);

    if (wifinet::connect() != ESP_OK) {
        err = "wifi";
        // Exponential-ish backoff capped, so a dead AP doesn't drain the pack with rapid retries.
        next_wake = clamp_wake(cfg::DEFAULT_WAKE_INTERVAL_S *
                               (g_rtc.consecutive_net_failures > 4 ? 4 : 1));
    } else if (!sync_time_if_needed()) {
        // Clock unset & gateway NTP unreachable → TLS cert-time check would fail; skip the fetch.
        err = "time";
    } else {
        // A touch wake forces an unconditional fetch (user wants the freshest view even if the
        // server's cached frame matches what's shown).
        uint32_t inm = (wake == power::WakeReason::Touch) ? 0
                       : (g_rtc.shown_valid ? g_rtc.shown_crc32 : 0);

        net::DisplayResult r = net::fetch_display(device_id, token, inm);
        if (r.next_wake_s) next_wake = clamp_wake(r.next_wake_s);

        switch (r.status) {
        case net::Status::NotModified:
            ESP_LOGI(TAG, "304 — panel unchanged, skipping refresh");
            break;
        case net::Status::Ok:
            if (epd::power_up() && epd::show(r.body, r.body_len, r.header)) {
                g_rtc.shown_crc32 = r.header.content_crc32;
                g_rtc.shown_valid = true;
                rendered = true;
            } else {
                err = "render";
            }
            epd::power_down();
            break;
        case net::Status::Unauthorized: err = "auth";    break;
        case net::Status::NoPayload:    err = "nopayload"; next_wake = 60; break;
        case net::Status::BadPayload:   err = "badpayload"; break;
        default:                        err = "fetch";   break;
        }
        net::free_result(r);

        // ── Telemetry (fire-and-forget) ──────────────────────────────────────
        float temp_c = 0, rh = 0;
        bool env_ok = sensors::read_sht4x(&temp_c, &rh);

        tlm::Sample s {
            .fw        = cfg::FIRMWARE_VERSION,
            .batt_mv   = power::battery_mv(),
            .batt_pct  = 0,
            .heap_free = (int)heap_caps_get_free_size(MALLOC_CAP_INTERNAL),
            .heap_min  = (int)heap_caps_get_minimum_free_size(MALLOC_CAP_INTERNAL),
            .rssi      = wifinet::last_rssi(),
            .wake      = power::wake_reason_str(wake),
            .wake_ms   = (int)((esp_timer_get_time() - t0) / 1000),
            .rendered  = rendered,
            .err       = err,
            .boot      = (int)g_rtc.boot_count,
            .env_valid = env_ok,
            .temp_c    = temp_c,
            .rh        = rh,
        };
        s.batt_pct = power::battery_pct(s.batt_mv);
        char json[320];
        size_t jl = tlm::to_json(s, json, sizeof json);
        net::post_telemetry(device_id, token, json, jl);

        // Opportunistic OTA: only on a healthy battery and never mid-interaction (a multi-MB
        // download is costly). maybe_update() no-ops unless the server advertises a newer build,
        // and reboots into it on success (so this call may not return).
        if (r.fw_target[0] && s.batt_pct >= 30 && wake != power::WakeReason::Touch) {
            ota::maybe_update(r.fw_target, r.fw_url);
        }
    }

    g_rtc.next_wake_s = next_wake;

    // ── Tear down cleanly so the radio is fully off before deep sleep ─────────
    net::shutdown();
    wifinet::disconnect();

    ESP_LOGI(TAG, "cycle done in %lld ms; sleeping %u s (err=%s)",
             (esp_timer_get_time() - t0) / 1000, next_wake, err ? err : "none");
    power::deep_sleep(next_wake);
}
