#pragma once

namespace ota {

// If target_version is non-empty and differs from the running build, download + apply the image
// at url over HTTPS (cert verified against the bundle) and reboot into it. Returns false if no
// update was needed or the attempt failed (device keeps running the current image).
//
// Safe by construction: esp_https_ota writes to the *inactive* OTA slot and only flips the boot
// partition after a fully verified download, so a failed/interrupted OTA cannot brick the unit —
// it just boots the existing image. Pair with rollback (see sdkconfig) for runtime self-check.
bool maybe_update(const char* target_version, const char* url);

} // namespace ota
