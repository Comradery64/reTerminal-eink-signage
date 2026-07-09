#pragma once
#include <cstdint>
#include <cstddef>
#include "protocol.hpp"

namespace net {

enum class Status { Ok, NotModified, Unauthorized, NoPayload, NetError, BadPayload };

struct DisplayResult {
    Status   status   = Status::NetError;
    mdpf::Header header{};
    uint8_t* body     = nullptr;   // heap; compressed MDPF body (caller frees via free_result)
    size_t   body_len = 0;
    uint32_t next_wake_s = 0;      // from X-Next-Wake (falls back to header/default)
    uint32_t etag_crc   = 0;       // parsed from ETag (0 if absent)
    char     fw_target[24] = {0};  // X-Fw-Target: server-advertised firmware version (OTA)
    char     fw_url[192]   = {0};  // X-Fw-Url: signed image URL for OTA
};

// Establishes (or resumes) one TLS session and issues GET /api/v1/display/{id}.
// `if_none_match_crc` is the CRC of the frame currently on the panel (0 = unconditional).
DisplayResult fetch_display(const char* device_id, const char* bearer, uint32_t if_none_match_crc);
void free_result(DisplayResult& r);

// POST /api/v1/telemetry/{id}. Reuses the live TLS session if still connected; otherwise opens
// one. Fire-and-forget (returns quickly, ignores body).
void post_telemetry(const char* device_id, const char* bearer, const char* json, size_t json_len);

// Close any live TLS connection and persist the session ticket for the next wake.
void shutdown();

} // namespace net
