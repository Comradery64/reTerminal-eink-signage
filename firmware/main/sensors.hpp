#pragma once

namespace sensors {

// Reads the on-board SHT4x (I2C 0x44 on I2C1). Returns true and fills tempC/rh on success.
// Brings the I2C bus up and tears it down per call so it adds no deep-sleep current.
bool read_sht4x(float* tempC, float* rh);

} // namespace sensors
