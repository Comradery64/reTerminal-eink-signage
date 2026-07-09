#include "net_client.hpp"
#include "config.hpp"
#include "nvs_store.hpp"

#include "esp_log.h"
#include "mbedtls/ssl.h"
#include "mbedtls/entropy.h"
#include "mbedtls/ctr_drbg.h"
#include "mbedtls/net_sockets.h"
#include "mbedtls/x509_crt.h"
#include "mbedtls/pk.h"
#include "mbedtls/error.h"
#include "esp_heap_caps.h"

#include <cstdio>
#include <cstdlib>
#include <cstring>

// Internal CA embedded via EMBED_TXTFILES (firmware/main/certs/broker_ca.pem). TLS path chosen:
// pin the broker against this CA only (not the public Mozilla bundle).
extern const uint8_t broker_ca_pem_start[] asm("_binary_broker_ca_pem_start");
extern const uint8_t broker_ca_pem_end[]   asm("_binary_broker_ca_pem_end");

namespace net {
static const char* TAG = "net";
static mbedtls_x509_crt s_cacert;
// Optional per-device client cert + key for mTLS (Approach A: provisioned into encrypted NVS).
// Presence-gated: if no client cert is provisioned we do server-auth-only TLS, so the SAME image
// works before and after mTLS is enabled at the broker/Traefik.
static mbedtls_x509_crt   s_clicert;
static mbedtls_pk_context s_clikey;
static bool               s_has_clicert = false;

// ── TLS context (single connection per wake) ────────────────────────────────
struct Tls {
    mbedtls_ssl_context      ssl;
    mbedtls_ssl_config       conf;
    mbedtls_ctr_drbg_context drbg;
    mbedtls_entropy_context  entropy;
    mbedtls_net_context      fd;
    bool connected = false;
};
static Tls s_tls;

static constexpr size_t TLS_SESSION_MAX = 512;  // serialized session ticket blob cap

static bool tls_connect() {
    if (s_tls.connected) return true;

    mbedtls_ssl_init(&s_tls.ssl);
    mbedtls_ssl_config_init(&s_tls.conf);
    mbedtls_ctr_drbg_init(&s_tls.drbg);
    mbedtls_entropy_init(&s_tls.entropy);
    mbedtls_net_init(&s_tls.fd);

    const char* pers = "md-display";
    if (mbedtls_ctr_drbg_seed(&s_tls.drbg, mbedtls_entropy_func, &s_tls.entropy,
                              (const uint8_t*)pers, strlen(pers)) != 0) {
        ESP_LOGE(TAG, "drbg seed failed");
        return false;
    }

    char port[8];
    snprintf(port, sizeof port, "%u", cfg::BROKER_PORT);
    int ret = mbedtls_net_connect(&s_tls.fd, cfg::BROKER_HOST, port, MBEDTLS_NET_PROTO_TCP);
    if (ret != 0) {
        ESP_LOGE(TAG, "tcp connect failed -0x%x", -ret);
        return false;
    }

    if (mbedtls_ssl_config_defaults(&s_tls.conf, MBEDTLS_SSL_IS_CLIENT,
            MBEDTLS_SSL_TRANSPORT_STREAM, MBEDTLS_SSL_PRESET_DEFAULT) != 0)
        return false;

    // Verify the broker against our pinned internal CA (not the public bundle).
    mbedtls_x509_crt_init(&s_cacert);
    int crt = mbedtls_x509_crt_parse(&s_cacert, broker_ca_pem_start,
                                     broker_ca_pem_end - broker_ca_pem_start);
    if (crt != 0) {
        ESP_LOGE(TAG, "internal CA parse failed -0x%x (is certs/broker_ca.pem set?)", -crt);
        return false;
    }
    mbedtls_ssl_conf_authmode(&s_tls.conf, MBEDTLS_SSL_VERIFY_REQUIRED);
    mbedtls_ssl_conf_ca_chain(&s_tls.conf, &s_cacert, nullptr);
    mbedtls_ssl_conf_rng(&s_tls.conf, mbedtls_ctr_drbg_random, &s_tls.drbg);

    // ── Optional mTLS: present our per-device client cert if one was provisioned ──
    // Issued by step-ca (EC P-256, matches the CA); cert+key live in encrypted NVS "prov".
    // If absent → server-auth-only TLS. If present but broker requires mTLS and load fails, the
    // handshake fails closed (correct). Buffers are static (.bss) to keep them off the stack.
    if (store::has_client_cert()) {
        static char cbuf[2048], kbuf[2048];
        if (store::get_client_cert(cbuf, sizeof cbuf) && store::get_client_key(kbuf, sizeof kbuf)) {
            mbedtls_x509_crt_init(&s_clicert);
            mbedtls_pk_init(&s_clikey);
            // PEM parse length must include the terminating NUL.
            int cc = mbedtls_x509_crt_parse(&s_clicert, (const uint8_t*)cbuf, strlen(cbuf) + 1);
            int kk = mbedtls_pk_parse_key(&s_clikey, (const uint8_t*)kbuf, strlen(kbuf) + 1,
                                          nullptr, 0, mbedtls_ctr_drbg_random, &s_tls.drbg);
            if (cc == 0 && kk == 0 &&
                mbedtls_ssl_conf_own_cert(&s_tls.conf, &s_clicert, &s_clikey) == 0) {
                s_has_clicert = true;
                ESP_LOGI(TAG, "mTLS: presenting device client cert");
            } else {
                ESP_LOGE(TAG, "mTLS: client cert/key load failed (cc=-0x%x kk=-0x%x)", -cc, -kk);
                mbedtls_x509_crt_free(&s_clicert);
                mbedtls_pk_free(&s_clikey);
            }
        }
    }

    if (mbedtls_ssl_setup(&s_tls.ssl, &s_tls.conf) != 0) return false;
    mbedtls_ssl_set_hostname(&s_tls.ssl, cfg::BROKER_HOST);   // SNI + cert CN check

    // ── Session resumption: load the saved ticket from NVS and offer it ──────
    uint8_t blob[TLS_SESSION_MAX];
    size_t blob_len = 0;
    bool offered_session = false;
    if (store::load_tls_session(blob, sizeof blob, &blob_len)) {
        mbedtls_ssl_session saved;
        mbedtls_ssl_session_init(&saved);
        if (mbedtls_ssl_session_load(&saved, blob, blob_len) == 0) {
            if (mbedtls_ssl_set_session(&s_tls.ssl, &saved) == 0) {
                offered_session = true;
                ESP_LOGI(TAG, "offering resumed TLS session (%u B ticket)", (unsigned)blob_len);
            }
        } else {
            store::clear_tls_session();  // stale/corrupt → drop it
        }
        mbedtls_ssl_session_free(&saved);
    }

    mbedtls_ssl_set_bio(&s_tls.ssl, &s_tls.fd, mbedtls_net_send, mbedtls_net_recv, nullptr);

    while ((ret = mbedtls_ssl_handshake(&s_tls.ssl)) != 0) {
        if (ret != MBEDTLS_ERR_SSL_WANT_READ && ret != MBEDTLS_ERR_SSL_WANT_WRITE) {
            ESP_LOGE(TAG, "tls handshake failed -0x%x", -ret);
            return false;
        }
    }
    ESP_LOGI(TAG, "tls up: %s, offered_resume=%d", mbedtls_ssl_get_ciphersuite(&s_tls.ssl),
             offered_session ? 1 : 0);
    s_tls.connected = true;

    // ── Persist the (possibly newly issued) session ticket for the next wake ──
    mbedtls_ssl_session cur;
    mbedtls_ssl_session_init(&cur);
    if (mbedtls_ssl_get_session(&s_tls.ssl, &cur) == 0) {
        size_t need = 0;
        // First call with NULL buf to size, then serialize.
        mbedtls_ssl_session_save(&cur, nullptr, 0, &need);
        if (need > 0 && need <= sizeof blob) {
            if (mbedtls_ssl_session_save(&cur, blob, sizeof blob, &need) == 0)
                store::save_tls_session(blob, need);
        }
    }
    mbedtls_ssl_session_free(&cur);
    return true;
}

static void tls_free() {
    if (s_tls.connected) {
        mbedtls_ssl_close_notify(&s_tls.ssl);
    }
    mbedtls_net_free(&s_tls.fd);
    mbedtls_ssl_free(&s_tls.ssl);
    mbedtls_ssl_config_free(&s_tls.conf);
    mbedtls_x509_crt_free(&s_cacert);
    if (s_has_clicert) {
        mbedtls_x509_crt_free(&s_clicert);
        mbedtls_pk_free(&s_clikey);
        s_has_clicert = false;
    }
    mbedtls_ctr_drbg_free(&s_tls.drbg);
    mbedtls_entropy_free(&s_tls.entropy);
    s_tls.connected = false;
}

static bool ssl_write_all(const uint8_t* p, size_t n) {
    size_t off = 0;
    while (off < n) {
        int w = mbedtls_ssl_write(&s_tls.ssl, p + off, n - off);
        if (w == MBEDTLS_ERR_SSL_WANT_READ || w == MBEDTLS_ERR_SSL_WANT_WRITE) continue;
        if (w <= 0) return false;
        off += w;
    }
    return true;
}

// ── Tiny HTTP/1.1 response reader ───────────────────────────────────────────
struct HttpResp {
    int      code = 0;
    long     content_length = -1;
    uint32_t next_wake = 0;
    uint32_t etag_crc = 0;
    bool     etag_present = false;
    char     fw_target[24] = {0};
    char     fw_url[192] = {0};
};

// Copy a header value (text after "Name:" optional space) up to CRLF into out.
static void copy_hdr_val(const char* after_colon, char* out, size_t cap) {
    while (*after_colon == ' ') after_colon++;
    size_t i = 0;
    while (after_colon[i] && after_colon[i] != '\r' && after_colon[i] != '\n' && i + 1 < cap) {
        out[i] = after_colon[i];
        i++;
    }
    out[i] = 0;
}

// Read until CRLFCRLF; parse status + the few headers we care about. `rolling` returns any
// body bytes already read past the header boundary.
static bool read_headers(HttpResp* r, uint8_t* spill, size_t spill_cap, size_t* spill_len) {
    char hdr[1024];
    size_t hl = 0;
    *spill_len = 0;
    while (hl < sizeof(hdr) - 1) {
        int n = mbedtls_ssl_read(&s_tls.ssl, (uint8_t*)hdr + hl, 1);
        if (n == MBEDTLS_ERR_SSL_WANT_READ) continue;
        if (n <= 0) return false;
        hl += n;
        if (hl >= 4 && memcmp(hdr + hl - 4, "\r\n\r\n", 4) == 0) break;
    }
    hdr[hl] = 0;

    if (sscanf(hdr, "HTTP/1.%*d %d", &r->code) != 1) return false;

    // Case-insensitive-ish header scan (broker emits canonical casing).
    if (const char* p = strstr(hdr, "Content-Length:")) r->content_length = atol(p + 15);
    if (const char* p = strstr(hdr, "X-Next-Wake:"))     r->next_wake = (uint32_t)atol(p + 12);
    if (const char* p = strstr(hdr, "ETag:")) {
        // ETag: "0a1b2c3d"
        const char* q = strchr(p, '"');
        if (q) { r->etag_crc = (uint32_t)strtoul(q + 1, nullptr, 16); r->etag_present = true; }
    }
    if (const char* p = strstr(hdr, "X-Fw-Target:")) copy_hdr_val(p + 12, r->fw_target, sizeof r->fw_target);
    if (const char* p = strstr(hdr, "X-Fw-Url:"))    copy_hdr_val(p + 9,  r->fw_url,    sizeof r->fw_url);
    return true;
}

DisplayResult fetch_display(const char* device_id, const char* bearer, uint32_t inm_crc) {
    DisplayResult out;
    if (!tls_connect()) { out.status = Status::NetError; return out; }

    char req[512];
    int rl;
    if (inm_crc != 0) {
        rl = snprintf(req, sizeof req,
            "GET %s%s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n"
            "If-None-Match: \"%08x\"\r\nConnection: keep-alive\r\n\r\n",
            cfg::DISPLAY_PATH_PREFIX, device_id, cfg::BROKER_HOST, bearer, (unsigned)inm_crc);
    } else {
        rl = snprintf(req, sizeof req,
            "GET %s%s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n"
            "Connection: keep-alive\r\n\r\n",
            cfg::DISPLAY_PATH_PREFIX, device_id, cfg::BROKER_HOST, bearer);
    }
    if (rl <= 0 || !ssl_write_all((uint8_t*)req, rl)) { out.status = Status::NetError; return out; }

    HttpResp resp;
    uint8_t spill[64]; size_t spill_len = 0;
    if (!read_headers(&resp, spill, sizeof spill, &spill_len)) { out.status = Status::NetError; return out; }

    out.next_wake_s = resp.next_wake;
    out.etag_crc    = resp.etag_crc;
    // OTA hints are carried on every response (incl. 304) so updates aren't blocked by 304s.
    std::strncpy(out.fw_target, resp.fw_target, sizeof out.fw_target - 1);
    std::strncpy(out.fw_url, resp.fw_url, sizeof out.fw_url - 1);

    if (resp.code == 304) { out.status = Status::NotModified; return out; }
    if (resp.code == 401) { out.status = Status::Unauthorized; return out; }
    if (resp.code == 503) { out.status = Status::NoPayload; return out; }
    if (resp.code != 200 || resp.content_length <= 0) { out.status = Status::NetError; return out; }

    // Read the full MDPF payload (header + compressed body). Small (~a few KB); buffer it.
    size_t total = (size_t)resp.content_length;
    auto* buf = (uint8_t*)malloc(total);
    if (!buf) { out.status = Status::NetError; return out; }
    size_t got = 0;
    while (got < total) {
        int n = mbedtls_ssl_read(&s_tls.ssl, buf + got, total - got);
        if (n == MBEDTLS_ERR_SSL_WANT_READ) continue;
        if (n <= 0) { free(buf); out.status = Status::NetError; return out; }
        got += n;
    }

    if (!mdpf::parse_header(buf, total, &out.header)) { free(buf); out.status = Status::BadPayload; return out; }
    if (out.header.next_wake_s) out.next_wake_s = out.header.next_wake_s;

    // Hand back only the body (after the 32-byte header). Move it to the front.
    size_t body_len = out.header.payload_len;
    if (mdpf::HEADER_SIZE + body_len > total) { free(buf); out.status = Status::BadPayload; return out; }
    memmove(buf, buf + mdpf::HEADER_SIZE, body_len);
    out.body = buf;
    out.body_len = body_len;
    out.status = Status::Ok;
    return out;
}

void free_result(DisplayResult& r) {
    if (r.body) { free(r.body); r.body = nullptr; }
}

void post_telemetry(const char* device_id, const char* bearer, const char* json, size_t json_len) {
    if (!tls_connect()) return;
    char hdr[512];
    int hl = snprintf(hdr, sizeof hdr,
        "POST %s%s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n"
        "Content-Type: application/json\r\nContent-Length: %u\r\nConnection: close\r\n\r\n",
        cfg::TELEMETRY_PATH_PREFIX, device_id, cfg::BROKER_HOST, bearer, (unsigned)json_len);
    if (hl <= 0) return;
    ssl_write_all((uint8_t*)hdr, hl);
    ssl_write_all((const uint8_t*)json, json_len);
    // Best-effort: drain the 204 so the socket closes cleanly; ignore content.
    HttpResp resp; uint8_t spill[8]; size_t sl;
    read_headers(&resp, spill, sizeof spill, &sl);
}

void shutdown() {
    tls_free();
}

} // namespace net
