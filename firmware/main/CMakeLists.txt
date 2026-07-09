idf_component_register(
    SRCS
        "app_main.cpp"
        "power.cpp"
        "wifi_fast.cpp"
        "net_client.cpp"
        "nvs_store.cpp"
        "epd_spectra6.cpp"
        "telemetry.cpp"
        "sensors.cpp"
        "ota.cpp"
    INCLUDE_DIRS "."
    EMBED_TXTFILES "certs/broker_ca.pem"
    REQUIRES
        nvs_flash
        esp_wifi
        esp_netif
        esp_event
        wpa_supplicant   # provides esp_eap_client.h + esp_eap_client_* (was a standalone
                         # component pre-5.5; folded into wpa_supplicant in ESP-IDF 5.5.x)
        esp-tls
        mbedtls
        esp_adc
        driver
        esp_driver_i2c
        esp_https_ota
        app_update
        esp_timer
        esp_hw_support
)
