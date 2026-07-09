// State retained in RTC slow memory across deep sleep. Survives timer/GPIO wake but NOT a
// full power cycle (battery swap) — on cold boot everything is zero and the code falls back
// to a full scan / no-If-None-Match, which is correct.
#pragma once
#include <cstdint>
#include "esp_attr.h"

struct RtcState {
    uint32_t magic;            // RTC_STATE_MAGIC once initialized
    uint32_t boot_count;

    // Fast Wi-Fi association cache (skips active scan).
    uint8_t  bssid[6];
    uint8_t  channel;
    bool     bssid_valid;

    // Content hash of the framebuffer currently on the panel → sent as If-None-Match so an
    // unchanged room returns 304 and we never power the (expensive) panel refresh.
    uint32_t shown_crc32;
    bool     shown_valid;

    // Server-directed next wake (seconds). Updated from X-Next-Wake every successful wake.
    uint32_t next_wake_s;

    // Health counters surfaced in telemetry.
    uint32_t consecutive_net_failures;
};

constexpr uint32_t RTC_STATE_MAGIC = 0x4D44'5031; // "MDP1"

// Single definition lives in app_main.cpp.
extern RTC_DATA_ATTR RtcState g_rtc;

inline void rtc_state_ensure_init() {
    if (g_rtc.magic != RTC_STATE_MAGIC) {
        g_rtc = RtcState{};
        g_rtc.magic = RTC_STATE_MAGIC;
        g_rtc.next_wake_s = 0;
    }
}
