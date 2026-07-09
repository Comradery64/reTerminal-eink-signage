#include "power.hpp"
#include "config.hpp"
#include "esp_sleep.h"
#include "esp_log.h"
#include "esp_adc/adc_oneshot.h"
#include "esp_adc/adc_cali.h"
#include "esp_adc/adc_cali_scheme.h"
#include "driver/rtc_io.h"
#include "driver/gpio.h"
#include "esp_rom_sys.h"
#include "soc/soc_caps.h"

namespace power {
static const char* TAG = "power";

WakeReason wake_reason() {
    switch (esp_sleep_get_wakeup_cause()) {
        case ESP_SLEEP_WAKEUP_TIMER: return WakeReason::Timer;
        case ESP_SLEEP_WAKEUP_EXT1:  return WakeReason::Touch;
        case ESP_SLEEP_WAKEUP_UNDEFINED: return WakeReason::PowerOn; // cold boot / reset
        default: return WakeReason::Other;
    }
}

const char* wake_reason_str(WakeReason r) {
    switch (r) {
        case WakeReason::PowerOn: return "poweron";
        case WakeReason::Timer:   return "timer";
        case WakeReason::Touch:   return "touch";
        default:                  return "other";
    }
}

int battery_mv() {
    // The battery divider is gated by VBAT_EN (active-high) so it draws no current except while
    // measuring. Enable it, let it settle, read, then disable.
    gpio_set_direction((gpio_num_t)cfg::PIN_VBAT_EN, GPIO_MODE_OUTPUT);
    gpio_set_level((gpio_num_t)cfg::PIN_VBAT_EN, 1);
    esp_rom_delay_us(2000);

    adc_oneshot_unit_handle_t unit;
    adc_oneshot_unit_init_cfg_t ucfg = { .unit_id = ADC_UNIT_1 };
    if (adc_oneshot_new_unit(&ucfg, &unit) != ESP_OK) {
        gpio_set_level((gpio_num_t)cfg::PIN_VBAT_EN, 0);
        return 0;
    }

    adc_channel_t ch; adc_unit_t u;
    if (adc_oneshot_io_to_channel(cfg::PIN_BATT_ADC, &u, &ch) != ESP_OK) {
        adc_oneshot_del_unit(unit);
        return 0;
    }
    adc_oneshot_chan_cfg_t ccfg = { .atten = ADC_ATTEN_DB_12, .bitwidth = ADC_BITWIDTH_DEFAULT };
    adc_oneshot_config_channel(unit, ch, &ccfg);

    // Calibrated conversion (curve fitting on S3).
    adc_cali_handle_t cali = nullptr;
    adc_cali_curve_fitting_config_t cal = {
        .unit_id = u, .chan = ch, .atten = ADC_ATTEN_DB_12, .bitwidth = ADC_BITWIDTH_DEFAULT };
    bool have_cali = adc_cali_create_scheme_curve_fitting(&cal, &cali) == ESP_OK;

    int acc = 0, n = 8;
    for (int i = 0; i < n; ++i) {
        int raw = 0;
        adc_oneshot_read(unit, ch, &raw);
        int mv = raw;
        if (have_cali) adc_cali_raw_to_voltage(cali, raw, &mv);
        acc += mv;
    }
    if (have_cali) adc_cali_delete_scheme_curve_fitting(cali);
    adc_oneshot_del_unit(unit);

    gpio_set_level((gpio_num_t)cfg::PIN_VBAT_EN, 0); // stop the divider drain
    return (int)((acc / n) * cfg::BATT_DIVIDER);     // undo on-board resistor divider
}

int battery_pct(int mv) {
    if (mv <= cfg::BATT_EMPTY_MV) return 0;
    if (mv >= cfg::BATT_FULL_MV)  return 100;
    return (mv - cfg::BATT_EMPTY_MV) * 100 / (cfg::BATT_FULL_MV - cfg::BATT_EMPTY_MV);
}

[[noreturn]] void deep_sleep(uint32_t seconds) {
    // Button wake: the E1002 buttons are active-LOW with pull-ups, so we wake on a LOW level.
    // Both white (A) and green (B) buttons are armed via ext1.
    const gpio_num_t btns[] = {(gpio_num_t)cfg::PIN_WAKE_BTN_A, (gpio_num_t)cfg::PIN_WAKE_BTN_B,
                               (gpio_num_t)cfg::PIN_WAKE_BTN_C};
    uint64_t mask = 0;
    for (gpio_num_t b : btns) {
        rtc_gpio_init(b);
        rtc_gpio_set_direction(b, RTC_GPIO_MODE_INPUT_ONLY);
        rtc_gpio_pullup_en(b);          // hold the line HIGH while idle
        rtc_gpio_pulldown_dis(b);
        mask |= (1ULL << b);
    }
    esp_sleep_enable_ext1_wakeup(mask, ESP_EXT1_WAKEUP_ANY_LOW);

    // RTC timer wake.
    esp_sleep_enable_timer_wakeup((uint64_t)seconds * 1000000ULL);

    // Isolate floating pads & power down RTC peripherals we don't need to keep current low.
    ESP_LOGI(TAG, "deep sleep for %u s", seconds);
    esp_deep_sleep_start();
}

} // namespace power
