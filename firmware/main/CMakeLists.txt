# MD_BENCH_STATIC_FRAME (see Kconfig.projbuild) embeds a raw MDPF payload for a no-network bench
# bring-up and skips WiFi/TLS/OTA/telemetry/sensors entirely (app_main.cpp compiles a completely
# separate app_main() body via #if/#else — see there). Trim SRCS/REQUIRES to match: a much smaller
# image means far fewer flash blocks, which matters on a marginal USB-serial link. Only require the
# embed file to exist when this bench build is actually requested, so default (production) builds
# are unaffected.
if(CONFIG_MD_BENCH_STATIC_FRAME)
    idf_component_register(
        SRCS
            "app_main.cpp"
            "epd_spectra6.cpp"
        INCLUDE_DIRS "."
        EMBED_FILES "bench/frame.mdpf"
        REQUIRES
            driver
    )
else()
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
endif()
