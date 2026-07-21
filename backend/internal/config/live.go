package config

import "sync/atomic"

// Live is a concurrency-safe holder for the broker's active *Config. It exists because the admin
// and manager web UIs (internal/server) can write a new config at any time — poller and server
// handlers must never see a half-updated Config mid-read. Callers should Load() once per
// operation (one HTTP request, one poll tick) and use that single snapshot throughout, rather
// than re-Loading partway through — that guarantees the whole operation sees one consistent
// config even while a Store() from a concurrent write is landing.
type Live struct {
	v atomic.Value // holds *Config
}

// NewLive wraps an already-validated initial config.
func NewLive(initial *Config) *Live {
	l := &Live{}
	l.v.Store(initial)
	return l
}

// Load returns the current config snapshot.
func (l *Live) Load() *Config {
	return l.v.Load().(*Config)
}

// Store swaps in a new config snapshot. Callers must validate before calling Store (see
// WithRoom, WithoutRoom, WithRoomWakeOverride) — Store itself does not validate.
func (l *Live) Store(c *Config) {
	l.v.Store(c)
}
