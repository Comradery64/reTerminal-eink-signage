// MDPF v1 wire protocol — MUST stay byte-for-byte in sync with
// backend/internal/render/payload.go and PROTOCOL.md.
#pragma once
#include <cstdint>
#include <cstring>

namespace mdpf {

constexpr uint8_t  MAGIC[4]   = {'M', 'D', 'P', 'F'};
constexpr uint16_t VERSION    = 1;
constexpr size_t   HEADER_SIZE = 32;

enum Flags : uint16_t {
    FLAG_COMPRESSED  = 1 << 0,  // body is PackBits-encoded
    FLAG_FULL_REFR   = 1 << 1,  // panel requires a full refresh
};

#pragma pack(push, 1)
struct Header {
    uint8_t  magic[4];
    uint16_t version;
    uint16_t flags;
    uint16_t width;
    uint16_t height;
    uint8_t  bpp;
    uint8_t  reserved;
    uint16_t reserved2;
    uint32_t payload_len;    // bytes of body following the header
    uint32_t raw_len;        // uncompressed packed length (width*height/2)
    uint32_t next_wake_s;    // server-suggested sleep seconds
    uint32_t content_crc32;  // hash of the uncompressed framebuffer == ETag
};
#pragma pack(pop)
static_assert(sizeof(Header) == HEADER_SIZE, "MDPF header must be 32 bytes");

// Parse + validate a header from a buffer of at least HEADER_SIZE bytes.
inline bool parse_header(const uint8_t* buf, size_t len, Header* out) {
    if (len < HEADER_SIZE) return false;
    std::memcpy(out, buf, HEADER_SIZE);
    if (std::memcmp(out->magic, MAGIC, 4) != 0) return false;
    if (out->version != VERSION) return false;
    if (out->bpp != 4) return false;
    return true;
}

// Streaming PackBits decoder. Mirrors backend PackBits() (verified by packbits_check.py).
// Calls sink(byte) for each decoded byte, stops at raw_len. Returns bytes produced.
// `sink` is a callable void(uint8_t); designed so the EPD driver can clock each decoded
// byte straight onto the SPI bus with zero intermediate framebuffer allocation.
template <typename Sink>
size_t packbits_decode(const uint8_t* comp, size_t comp_len, size_t raw_len, Sink&& sink) {
    size_t i = 0, produced = 0;
    while (i < comp_len && produced < raw_len) {
        int8_t ctrl = static_cast<int8_t>(comp[i++]);
        if (ctrl >= 0) {                       // literal run of (ctrl+1) bytes
            int count = ctrl + 1;
            for (int k = 0; k < count && i < comp_len && produced < raw_len; ++k) {
                sink(comp[i++]);
                produced++;
            }
        } else if (ctrl != -128) {             // replicate next byte (1-ctrl) times
            int count = 1 - ctrl;
            if (i >= comp_len) break;
            uint8_t v = comp[i++];
            for (int k = 0; k < count && produced < raw_len; ++k) {
                sink(v);
                produced++;
            }
        }
        // ctrl == -128 is a no-op
    }
    return produced;
}

} // namespace mdpf
