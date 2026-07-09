// Encrypted-NVS-backed storage for provisioned secrets and cross-deep-sleep blobs.
//
// Two namespaces:
//   "prov" — write-once at provisioning: wifi ssid/psk (or EAP creds), broker bearer token.
//            Protected by Flash Encryption (see firmware/secure/README.md).
//   "rt"   — runtime caches: serialized TLS session ticket. (Fast-connect BSSID/channel and
//            last-shown CRC live in RTC slow memory, not NVS — see app_state.hpp.)
#pragma once
#include <cstdint>
#include <cstddef>
#include "esp_err.h"

namespace store {

esp_err_t init();   // nvs_flash_init (+ erase/retry on version mismatch)

// ── Provisioned secrets (read-only at runtime) ──────────────────────────────
// Returns false if the key is missing (device not provisioned).
bool get_wifi_ssid(char* out, size_t cap);
bool get_wifi_psk(char* out, size_t cap);
bool get_device_token(char* out, size_t cap);
bool get_device_id(char* out, size_t cap);

// EAP / WPA2-Enterprise (optional; present only on enterprise-provisioned units)
bool get_eap_identity(char* out, size_t cap);
bool get_eap_username(char* out, size_t cap);
bool get_eap_password(char* out, size_t cap);
bool is_enterprise();

// Optional per-device client cert + key for mTLS (present only on mTLS-provisioned units).
bool get_client_cert(char* out, size_t cap);   // PEM (public)
bool get_client_key(char* out, size_t cap);     // PEM (private; Flash-Encryption protected)
bool has_client_cert();                          // presence test (no client cert → server-auth-only TLS)

// ── TLS session ticket persistence (resumption across deep sleep) ─────────────
// Stores/loads the serialized mbedtls_ssl_session produced by mbedtls_ssl_session_save().
bool  load_tls_session(uint8_t* buf, size_t cap, size_t* out_len);
void  save_tls_session(const uint8_t* buf, size_t len);
void  clear_tls_session();

} // namespace store
