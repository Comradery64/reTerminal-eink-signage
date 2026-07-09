#include "epd_spectra6.hpp"
#include "config.hpp"

#include "driver/spi_master.h"
#include "driver/gpio.h"
#include "esp_rom_sys.h"
#include "esp_log.h"
#include "esp_heap_caps.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include <cstring>

namespace epd {
static const char* TAG = "epd";

// ── Spectra 6 controller color codes ────────────────────────────────────────
// MUST match backend/internal/render/spectra6.go Spectra6Code. Verify against the panel
// datasheet for your E1002 batch.
enum : uint8_t {
    EPD_CLR_BLACK  = 0x0, EPD_CLR_WHITE = 0x1, EPD_CLR_YELLOW = 0x2,
    EPD_CLR_RED    = 0x3, EPD_CLR_BLUE  = 0x5, EPD_CLR_GREEN  = 0x6,
};

static spi_device_handle_t s_spi;
static bool s_inited = false;

// GDEP073E01/ED2208 holds BUSY LOW while working and releases it HIGH when ready.
// If the first on-hardware refresh hangs in wait_busy(), flip this to 0 and re-test.
static constexpr int EPD_BUSY_READY_LEVEL = 1;

static inline void dc(int level)  { gpio_set_level((gpio_num_t)cfg::PIN_EPD_DC, level); }
static inline void rst(int level) { gpio_set_level((gpio_num_t)cfg::PIN_EPD_RST, level); }

// Returns true when BUSY reaches the ready level, false on timeout (so callers can abort the
// wake cleanly and report an error instead of blocking the radio/battery indefinitely).
[[nodiscard]] static bool wait_busy(int timeout_ms = 40000) {
    int waited = 0;
    while (gpio_get_level((gpio_num_t)cfg::PIN_EPD_BUSY) != EPD_BUSY_READY_LEVEL) {
        vTaskDelay(pdMS_TO_TICKS(10));
        if ((waited += 10) >= timeout_ms) {
            ESP_LOGE(TAG, "BUSY timeout after %d ms", timeout_ms);
            return false;
        }
    }
    return true;
}

static void cmd(uint8_t c) {
    dc(0);
    spi_transaction_t t = {}; t.length = 8; t.tx_buffer = &c;
    spi_device_polling_transmit(s_spi, &t);
}

static void data(const uint8_t* d, size_t n) {
    if (!n) return;
    dc(1);
    // SPI max transfer is bounded; chunk it.
    const size_t CHUNK = 4000;
    for (size_t off = 0; off < n; off += CHUNK) {
        size_t len = (n - off < CHUNK) ? (n - off) : CHUNK;
        spi_transaction_t t = {}; t.length = len * 8; t.tx_buffer = d + off;
        spi_device_polling_transmit(s_spi, &t);
    }
}

static void data1(uint8_t b) { data(&b, 1); }

bool power_up() {
    gpio_config_t io = {};
    io.mode = GPIO_MODE_OUTPUT;
    io.pin_bit_mask = (1ULL << cfg::PIN_EPD_DC) | (1ULL << cfg::PIN_EPD_RST);
    gpio_config(&io);

    io.mode = GPIO_MODE_INPUT;
    io.pin_bit_mask = (1ULL << cfg::PIN_EPD_BUSY);
    gpio_config(&io);

    if (!s_inited) {
        spi_bus_config_t bus = {};
        bus.mosi_io_num = cfg::PIN_EPD_MOSI;
        bus.miso_io_num = -1;            // panel is write-only (MISO=GPIO8 belongs to the SD slot)
        bus.sclk_io_num = cfg::PIN_EPD_SCLK;
        bus.quadwp_io_num = -1; bus.quadhd_io_num = -1;
        bus.max_transfer_sz = 4096 + 8;
        if (spi_bus_initialize(SPI2_HOST, &bus, SPI_DMA_CH_AUTO) != ESP_OK) return false;

        spi_device_interface_config_t dev = {};
        dev.clock_speed_hz = 10 * 1000 * 1000;   // 10 MHz
        dev.mode = 0;
        dev.spics_io_num = cfg::PIN_EPD_CS;
        dev.queue_size = 1;
        if (spi_bus_add_device(SPI2_HOST, &dev, &s_spi) != ESP_OK) return false;
        s_inited = true;
    }

    // Hardware reset (timing per Seeed_GxEPD2 GxEPD2_730c_GDEP073E01).
    rst(1); vTaskDelay(pdMS_TO_TICKS(50));
    rst(0); vTaskDelay(pdMS_TO_TICKS(20));
    rst(1); vTaskDelay(pdMS_TO_TICKS(10));
    if (!wait_busy()) return false;

    // ── Init sequence (GDEP073E01 / ED2208, verbatim from the reference driver) ──
    cmd(0xAA); { const uint8_t d[]={0x49,0x55,0x20,0x08,0x09,0x18}; data(d,sizeof d); } // CMDH
    cmd(0x01); { const uint8_t d[]={0x3F};                          data(d,sizeof d); } // PWRR
    cmd(0x00); { const uint8_t d[]={0x5F,0x69};                     data(d,sizeof d); } // PSR
    cmd(0x03); { const uint8_t d[]={0x00,0x54,0x00,0x44};           data(d,sizeof d); } // POFS
    cmd(0x05); { const uint8_t d[]={0x40,0x1F,0x1F,0x2C};           data(d,sizeof d); } // BTST1
    cmd(0x06); { const uint8_t d[]={0x6F,0x1F,0x17,0x49};           data(d,sizeof d); } // BTST2
    cmd(0x08); { const uint8_t d[]={0x6F,0x1F,0x1F,0x22};           data(d,sizeof d); } // BTST3
    cmd(0x30); { const uint8_t d[]={0x08};                          data(d,sizeof d); } // PLL
    cmd(0x50); { const uint8_t d[]={0x3F};                          data(d,sizeof d); } // CDI
    cmd(0x60); { const uint8_t d[]={0x02,0x00};                     data(d,sizeof d); } // TCON
    cmd(0x61); { const uint8_t d[]={0x03,0x20,0x01,0xE0};           data(d,sizeof d); } // TRES 800x480
    cmd(0x84); { const uint8_t d[]={0x01};                          data(d,sizeof d); } // T_VDCS
    cmd(0xE3); { const uint8_t d[]={0x2F};                          data(d,sizeof d); } // PWS

    cmd(0x04); if (!wait_busy(5000)) return false;   // power on (BUSY released ~1 s)
    return true;
}

bool show(const uint8_t* body, size_t body_len, const mdpf::Header& h) {
    if (h.width != cfg::EPD_WIDTH || h.height != cfg::EPD_HEIGHT) {
        ESP_LOGE(TAG, "geometry mismatch %ux%u", h.width, h.height);
        return false;
    }
    // Framebuffer in PSRAM (192 KB). Decode PackBits (or copy raw) into it.
    auto* fb = (uint8_t*)heap_caps_malloc(cfg::EPD_RAW_LEN, MALLOC_CAP_SPIRAM);
    if (!fb) { ESP_LOGE(TAG, "no PSRAM for framebuffer"); return false; }

    size_t produced;
    if (h.flags & mdpf::FLAG_COMPRESSED) {
        size_t idx = 0;
        produced = mdpf::packbits_decode(body, body_len, cfg::EPD_RAW_LEN,
                                         [&](uint8_t b){ fb[idx++] = b; });
    } else {
        produced = (body_len < cfg::EPD_RAW_LEN) ? body_len : cfg::EPD_RAW_LEN;
        memcpy(fb, body, produced);
    }
    if (produced != cfg::EPD_RAW_LEN) {
        ESP_LOGE(TAG, "decode short: %u/%u", (unsigned)produced, (unsigned)cfg::EPD_RAW_LEN);
        heap_caps_free(fb);
        return false;
    }

    // Stream the framebuffer, then trigger + wait for the full refresh (panel was powered on
    // at the end of power_up()).
    cmd(0x10);                          // DTM (data start)
    data(fb, cfg::EPD_RAW_LEN);
    heap_caps_free(fb);

    // Full-area refresh — exact sequence from Seeed_GxEPD2 GDEP073E01::refresh():
    // set the partial-RAM window to the whole panel (0x83), then CDI border (0x50), then refresh.
    // Note the controller quirk: y_end = y + h (not h-1). For 800x480: xe=799 (0x031F), ye=480 (0x01E0).
    cmd(0x83); { const uint8_t win[]={0x00,0x00, 0x03,0x1F, 0x00,0x00, 0x01,0xE0, 0x01}; data(win,sizeof win); }
    cmd(0x50); data1(0x3F);             // CDI border setting for full refresh
    cmd(0x12); data1(0x00);             // display refresh
    vTaskDelay(pdMS_TO_TICKS(2));       // GxEPD2 delay(1) before polling BUSY
    if (!wait_busy()) {                 // <-- the long, energy-dominant wait (Spectra 6 full refresh)
        ESP_LOGE(TAG, "refresh BUSY timeout — panel may be misconfigured");
        return false;
    }
    cmd(0x02); data1(0x00);             // power off
    (void)wait_busy(3000);              // best-effort; don't fail the render if power-off lags
    ESP_LOGI(TAG, "refresh complete");
    return true;
}

void power_down() {
    if (s_inited) {
        cmd(0x07); data1(0xA5);         // controller hibernate → µA-level panel draw (no rail GPIO)
    }
}

} // namespace epd
