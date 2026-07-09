#pragma once
#include <cstdint>

namespace power {

enum class WakeReason { PowerOn, Timer, Touch, Other };

WakeReason wake_reason();
const char* wake_reason_str(WakeReason r);

// Read battery via the sense divider. Returns millivolts and a 0..100 estimate.
int  battery_mv();
int  battery_pct(int mv);

// Configure ext1 (button) + RTC timer wake sources and enter deep sleep. Never returns.
[[noreturn]] void deep_sleep(uint32_t seconds);

} // namespace power
