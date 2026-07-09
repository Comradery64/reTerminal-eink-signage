#pragma once
#include <cstdint>
#include <cstddef>
#include "protocol.hpp"

namespace epd {

// Power the panel rail, init SPI + controller. Call only when an actual refresh is needed —
// the panel rail stays gated off on 304/no-change wakes to save energy.
bool power_up();

// Decode the PackBits/raw MDPF body into the framebuffer and drive a full Spectra 6 refresh.
// Blocks on BUSY until the refresh completes, then powers the panel down + deep-sleeps it.
// Returns false on decode/geometry mismatch.
bool show(const uint8_t* body, size_t body_len, const mdpf::Header& h);

// Controller deep sleep + cut the panel power rail. Always call before MCU deep sleep.
void power_down();

} // namespace epd
