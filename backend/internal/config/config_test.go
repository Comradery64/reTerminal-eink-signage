package config

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func validBase() Config {
	sum := sha256.Sum256([]byte("test-token"))
	return Config{
		Provider: "demo",
		Rooms:    []Room{{DeviceID: "rt-1", Room: "a@x", TokenSHA256: hex.EncodeToString(sum[:])}},
		Wake:     WakeConfig{Timezone: "UTC"},
	}
}

func TestValidateForcedRefreshHour(t *testing.T) {
	c := validBase()
	if err := c.validate(); err != nil {
		t.Fatalf("nil ForcedRefreshHour (disabled) must be valid: %v", err)
	}

	ok := 3
	c.Wake.ForcedRefreshHour = &ok
	if err := c.validate(); err != nil {
		t.Fatalf("hour 3 must be valid: %v", err)
	}

	tooHigh := 24
	c.Wake.ForcedRefreshHour = &tooHigh
	if err := c.validate(); err == nil {
		t.Fatal("hour 24 must be rejected")
	}

	negative := -1
	c.Wake.ForcedRefreshHour = &negative
	if err := c.validate(); err == nil {
		t.Fatal("negative hour must be rejected")
	}
}
