#pragma once
#include <cstddef>

namespace tlm {

struct Sample {
    const char* fw;
    int   batt_mv;
    int   batt_pct;
    int   heap_free;
    int   heap_min;
    int   rssi;
    const char* wake;
    int   wake_ms;
    bool  rendered;
    const char* err;   // nullptr if none
    int   boot;
    bool  env_valid;   // true if temp_c/rh below are populated (SHT4x read OK)
    float temp_c;
    float rh;
};

// Serialize to compact JSON. Returns bytes written (excluding NUL). Mirrors
// backend/internal/telemetry.Report field names.
size_t to_json(const Sample& s, char* out, size_t cap);

} // namespace tlm
