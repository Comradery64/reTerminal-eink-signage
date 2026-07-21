// Package telemetry ingests device health reports and exposes them in Prometheus exposition
// format so the existing k3s monitoring stack can scrape /metrics.
package telemetry

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// Report is the JSON body posted by firmware (mirror of firmware/main/telemetry.hpp).
type Report struct {
	FirmwareVer  string   `json:"fw"`
	BatteryMV    int      `json:"batt_mv"`
	BatteryPct   int      `json:"batt_pct"`
	HeapFree     int      `json:"heap_free"`
	HeapMinFree  int      `json:"heap_min"`
	RSSI         int      `json:"rssi"`
	WakeReason   string   `json:"wake"`     // "timer" | "touch" | "poweron"
	WakeMS       int      `json:"wake_ms"`  // active time this cycle
	RenderedBool bool     `json:"rendered"` // did we refresh the panel this wake?
	ErrCode      string   `json:"err,omitempty"`
	BootCount    int      `json:"boot"`
	TempC        *float64 `json:"temp_c,omitempty"` // room temperature (SHT4x); nil if not reported
	RH           *float64 `json:"rh,omitempty"`     // room humidity %; nil if not reported
}

type state struct {
	last         Report
	lastSeen     time.Time
	lastRendered time.Time // zero until the device reports rendered=true at least once
	roomStatus   string    // "available" | "starting_soon" | "in_meeting"; "" until first set
	reported     bool      // true once Ingest has been called at least once (firmware telemetry
	// received) — distinct from having a roomStatus, which SetRoomStatus sets independently and
	// much earlier (as soon as the poller can compute a schedule, before any device has ever
	// phoned home). Without this, a device that's only ever gotten a room-status update — the
	// entire not-yet-flashed rest of the fleet, in practice — would emit battery_percent=0 and a
	// last_seen_seconds computed from a zero time.Time (a multi-billion-second garbage value),
	// which read in Grafana as "every unflashed device is a dead, empty battery" instead of
	// "no telemetry yet".
}

type Store struct {
	mu sync.RWMutex
	m  map[string]*state
}

func New() *Store { return &Store{m: make(map[string]*state)} }

func (s *Store) Ingest(deviceID string, r Report, now time.Time) {
	s.mu.Lock()
	st, ok := s.m[deviceID]
	if !ok {
		st = &state{}
		s.m[deviceID] = st
	}
	lastRendered := st.lastRendered
	if r.RenderedBool {
		lastRendered = now
	}
	*st = state{last: r, lastSeen: now, lastRendered: lastRendered, roomStatus: st.roomStatus, reported: true}
	s.mu.Unlock()
}

// SetRoomStatus records the room's current calendar-derived state (calendar.RoomStatus) —
// what the panel is actually displaying right now, as opposed to device health. Called by the
// poller on every render cycle, independent of firmware telemetry POSTs, so it's set even for a
// device that has never phoned home yet.
func (s *Store) SetRoomStatus(deviceID, status string) {
	s.mu.Lock()
	st, ok := s.m[deviceID]
	if !ok {
		st = &state{}
		s.m[deviceID] = st
	}
	st.roomStatus = status
	s.mu.Unlock()
}

// Snapshot is a point-in-time read of one device's telemetry, for consumers that need
// structured access (e.g. the status endpoint) rather than /metrics' exposition text.
type Snapshot struct {
	Report       Report
	LastSeen     time.Time
	LastRendered time.Time // zero if the device has never reported rendered=true
}

func (s *Store) Snapshot(deviceID string) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.m[deviceID]
	if !ok || !st.reported {
		return Snapshot{}, false
	}
	return Snapshot{Report: st.last, LastSeen: st.lastSeen, LastRendered: st.lastRendered}, true
}

// WriteMetrics emits Prometheus exposition text.
func (s *Store) WriteMetrics(w io.Writer, now time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Telemetry-derived metrics (battery, signal, heap, ...) are scoped to devices that have
	// actually POSTed at least once — a device known only via SetRoomStatus (i.e. configured but
	// never flashed/never phoned home) must not show up as "0% battery" / "stale", which is what
	// happens if it's included with its zero-value Report and zero time.Time lastSeen.
	ids := make([]string, 0, len(s.m))
	for id, st := range s.m {
		if st.reported {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	fmt.Fprintln(w, "# HELP md_battery_millivolts Battery voltage in mV.")
	fmt.Fprintln(w, "# TYPE md_battery_millivolts gauge")
	for _, id := range ids {
		fmt.Fprintf(w, "md_battery_millivolts{device=%q}%d\n", id, s.m[id].last.BatteryMV)
	}
	emitGauge(w, ids, s.m, "md_battery_percent", "Estimated battery percent.", func(r Report) float64 { return float64(r.BatteryPct) })
	emitGauge(w, ids, s.m, "md_heap_free_bytes", "Free heap at report time.", func(r Report) float64 { return float64(r.HeapFree) })
	emitGauge(w, ids, s.m, "md_heap_min_free_bytes", "Min free heap since boot.", func(r Report) float64 { return float64(r.HeapMinFree) })
	emitGauge(w, ids, s.m, "md_wifi_rssi_dbm", "Last associated RSSI.", func(r Report) float64 { return float64(r.RSSI) })
	emitGauge(w, ids, s.m, "md_wake_duration_ms", "Active (awake) time last cycle.", func(r Report) float64 { return float64(r.WakeMS) })
	emitGauge(w, ids, s.m, "md_boot_count", "Total wake/boot count.", func(r Report) float64 { return float64(r.BootCount) })

	fmt.Fprintln(w, "# HELP md_last_seen_seconds Age of last telemetry report.")
	fmt.Fprintln(w, "# TYPE md_last_seen_seconds gauge")
	for _, id := range ids {
		fmt.Fprintf(w, "md_last_seen_seconds{device=%q}%d\n", id, int(now.Sub(s.m[id].lastSeen).Seconds()))
	}

	// Age since the device last actually repainted the panel (rendered=true), as opposed to
	// merely checking in — the ghosting/hardware-health signal md_last_seen_seconds can't give.
	// Omitted for devices that have never reported a render, same convention as room env below.
	fmt.Fprintln(w, "# HELP md_last_render_seconds Age of last actual panel repaint (rendered=true).")
	fmt.Fprintln(w, "# TYPE md_last_render_seconds gauge")
	for _, id := range ids {
		if lr := s.m[id].lastRendered; !lr.IsZero() {
			fmt.Fprintf(w, "md_last_render_seconds{device=%q}%d\n", id, int(now.Sub(lr).Seconds()))
		}
	}

	// Room environment (only for devices whose SHT4x reported this cycle).
	fmt.Fprintln(w, "# HELP md_room_temp_celsius Room temperature from the on-board SHT4x.")
	fmt.Fprintln(w, "# TYPE md_room_temp_celsius gauge")
	for _, id := range ids {
		if t := s.m[id].last.TempC; t != nil {
			fmt.Fprintf(w, "md_room_temp_celsius{device=%q}%g\n", id, *t)
		}
	}
	fmt.Fprintln(w, "# HELP md_room_humidity_percent Room relative humidity from the on-board SHT4x.")
	fmt.Fprintln(w, "# TYPE md_room_humidity_percent gauge")
	for _, id := range ids {
		if h := s.m[id].last.RH; h != nil {
			fmt.Fprintf(w, "md_room_humidity_percent{device=%q}%g\n", id, *h)
		}
	}

	// Info-style metric (value always 1, state carried entirely in the label) — what the panel
	// is actually displaying right now: available | starting_soon | in_meeting. Emitted for every
	// configured room with a computed schedule, independent of `reported`, since this comes from
	// the poller/calendar, not firmware telemetry.
	statusIDs := make([]string, 0, len(s.m))
	for id, st := range s.m {
		if st.roomStatus != "" {
			statusIDs = append(statusIDs, id)
		}
	}
	sort.Strings(statusIDs)
	fmt.Fprintln(w, "# HELP md_room_status_info Current displayed room status (see the status label).")
	fmt.Fprintln(w, "# TYPE md_room_status_info gauge")
	for _, id := range statusIDs {
		fmt.Fprintf(w, "md_room_status_info{device=%q,status=%q}1\n", id, s.m[id].roomStatus)
	}
}

func emitGauge(w io.Writer, ids []string, m map[string]*state, name, help string, f func(Report) float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	for _, id := range ids {
		fmt.Fprintf(w, "%s{device=%q}%g\n", name, id, f(m[id].last))
	}
}
