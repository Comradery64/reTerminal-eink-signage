#include "telemetry.hpp"
#include <cstdio>

namespace tlm {

size_t to_json(const Sample& s, char* out, size_t cap) {
    char env[48] = {0};
    if (s.env_valid) {
        snprintf(env, sizeof env, ",\"temp_c\":%.1f,\"rh\":%.0f", s.temp_c, s.rh);
    }
    int n = snprintf(out, cap,
        "{\"fw\":\"%s\",\"batt_mv\":%d,\"batt_pct\":%d,\"heap_free\":%d,\"heap_min\":%d,"
        "\"rssi\":%d,\"wake\":\"%s\",\"wake_ms\":%d,\"rendered\":%s,\"boot\":%d%s%s%s%s}",
        s.fw, s.batt_mv, s.batt_pct, s.heap_free, s.heap_min, s.rssi, s.wake, s.wake_ms,
        s.rendered ? "true" : "false", s.boot,
        s.err ? ",\"err\":\"" : "", s.err ? s.err : "", s.err ? "\"" : "", env);
    return (n < 0) ? 0 : (size_t)n;
}

} // namespace tlm
