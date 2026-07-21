// Package cache holds the latest rendered payload per device so the hot GET path never
// touches the calendar API or the renderer — it just serves bytes (or a 304).
package cache

import (
	"sync"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/render"
)

type Entry struct {
	Payload    render.Payload
	PreviewPNG []byte // PNG encoding of the same render, for humans (e.g. /dashboard) — devices never see this
	ETag       string // quoted hex CRC32, ready for the ETag header
	RenderedAt time.Time
	Err        error // last poll error, if the room is currently failing

	// Cur/Next are the calendar.Schedule.Current/Next results as resolved by the poller at
	// RenderedAt — up to PollInterval stale as *which* events they point to, but the events'
	// own Start/End timestamps are exact, so the HTTP handler can recompute an accurate smart
	// wake duration against a fresh `now` without re-fetching the calendar per-request.
	Cur, Next *calendar.Event

	// LastForcedRefreshDay is the local "YYYY-MM-DD" this device last received its once-daily
	// forced full repaint (see Store.ShouldForceFullRefresh). Empty if never forced.
	LastForcedRefreshDay string
}

type Store struct {
	mu sync.RWMutex
	m  map[string]Entry // keyed by device_id
}

func New() *Store { return &Store{m: make(map[string]Entry)} }

func (s *Store) Get(deviceID string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[deviceID]
	return e, ok
}

func (s *Store) Set(deviceID string, e Entry) {
	s.mu.Lock()
	s.m[deviceID] = e
	s.mu.Unlock()
}

func (s *Store) SetError(deviceID string, err error) {
	s.mu.Lock()
	e := s.m[deviceID]
	e.Err = err
	s.m[deviceID] = e
	s.mu.Unlock()
}

// ShouldForceFullRefresh reports whether deviceID is due its once-daily forced full repaint —
// true at most once per local calendar day, only during the hour named by forcedHour (0-23), in
// loc. Idempotent: the first caller within that hour on a given day gets true and the device is
// marked done for the day; every later call that same day/hour returns false.
//
// State resets on broker restart (in-memory only), which just means a device may get an extra
// forced refresh sooner than scheduled — never a missed one, and never a correctness issue.
func (s *Store) ShouldForceFullRefresh(deviceID string, now time.Time, forcedHour int, loc *time.Location) bool {
	local := now.In(loc)
	if local.Hour() != forcedHour {
		return false
	}
	day := local.Format("2006-01-02")

	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[deviceID]
	if e.LastForcedRefreshDay == day {
		return false
	}
	e.LastForcedRefreshDay = day
	s.m[deviceID] = e
	return true
}
