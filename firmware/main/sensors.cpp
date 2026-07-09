#include "sensors.hpp"
#include "config.hpp"

#include "driver/i2c_master.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_log.h"
#include <cstdint>

namespace sensors {
static const char* TAG = "sht4x";

// SHT4x CRC-8: poly 0x31, init 0xFF (per datasheet).
static uint8_t crc8(const uint8_t* d, int n) {
    uint8_t crc = 0xFF;
    for (int i = 0; i < n; ++i) {
        crc ^= d[i];
        for (int b = 0; b < 8; ++b)
            crc = (crc & 0x80) ? (uint8_t)((crc << 1) ^ 0x31) : (uint8_t)(crc << 1);
    }
    return crc;
}

bool read_sht4x(float* tempC, float* rh) {
    i2c_master_bus_config_t bus_cfg = {};
    bus_cfg.i2c_port = I2C_NUM_0;
    bus_cfg.sda_io_num = (gpio_num_t)cfg::PIN_I2C1_SDA;
    bus_cfg.scl_io_num = (gpio_num_t)cfg::PIN_I2C1_SCL;
    bus_cfg.clk_source = I2C_CLK_SRC_DEFAULT;
    bus_cfg.glitch_ignore_cnt = 7;
    bus_cfg.flags.enable_internal_pullup = true;

    i2c_master_bus_handle_t bus = nullptr;
    if (i2c_new_master_bus(&bus_cfg, &bus) != ESP_OK) return false;

    i2c_device_config_t dev_cfg = {};
    dev_cfg.dev_addr_length = I2C_ADDR_BIT_LEN_7;
    dev_cfg.device_address = cfg::SHT4X_ADDR;
    dev_cfg.scl_speed_hz = 100000;

    i2c_master_dev_handle_t dev = nullptr;
    bool ok = false;
    if (i2c_master_bus_add_device(bus, &dev_cfg, &dev) == ESP_OK) {
        const uint8_t measure_hi_precision = 0xFD;
        if (i2c_master_transmit(dev, &measure_hi_precision, 1, 100) == ESP_OK) {
            vTaskDelay(pdMS_TO_TICKS(15));   // datasheet: high-precision conversion < 10 ms
            uint8_t rx[6] = {0};
            if (i2c_master_receive(dev, rx, sizeof rx, 100) == ESP_OK &&
                crc8(&rx[0], 2) == rx[2] && crc8(&rx[3], 2) == rx[5]) {
                uint16_t t_raw = (uint16_t)(rx[0] << 8 | rx[1]);
                uint16_t h_raw = (uint16_t)(rx[3] << 8 | rx[4]);
                *tempC = -45.0f + 175.0f * (float)t_raw / 65535.0f;
                float h = -6.0f + 125.0f * (float)h_raw / 65535.0f;
                if (h < 0) h = 0;
                if (h > 100) h = 100;
                *rh = h;
                ok = true;
            } else {
                ESP_LOGW(TAG, "read/CRC failed");
            }
        }
        i2c_master_bus_rm_device(dev);
    }
    i2c_del_master_bus(bus);
    return ok;
}

} // namespace sensors
