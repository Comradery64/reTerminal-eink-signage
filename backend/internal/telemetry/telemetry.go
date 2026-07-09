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
	FirmwareVer  string  `json:"fw"`
	BatteryMV    int     `json:"batt_mv"`
	BatteryPct   int     `json:"batt_pct"`
	HeapFree     int     `json:"heap_free"`
	HeapMinFree  int     `json:"heap_min"`
	RSSI         int     `json:"rssi"`
	WakeReason   string  `json:"wake"`        // "timer" | "touch" | "poweron"
	WakeMS       int     `json:"wake_ms"`     // active time this cycle
	RenderedBool bool    `json:"rendered"`    // did we refresh the panel this wake?
	ErrCode      string  `json:"err,omitempty"`
	BootCount    int     `json:"boot"`
	TempC        *float64 `json:"temp_c,omitempty"` // room temperature (SHT4x); nil if not reported
	RH           *float64 `json:"rh,omitempty"`     // room humidity %; nil if not reported
}

type state struct {
	last     Report
	lastSeen time.Time
}

type Store struct {
	mu sync.RWMutex
	m  map[string]*state
}

func New() *Store { return &Store{m: make(map[string]*state)} }

func (s *Store) Ingest(deviceID string, r Report, now time.Time) {
	s.mu.Lock()
	s.m[deviceID] = &state{last: r, lastSeen: now}
	s.mu.Unlock()
}

// WriteMetrics emits Prometheus exposition text.
func (s *Store) WriteMetrics(w io.Writer, now time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := make([]string, 0, len(s.m))
	for id := range s.m {
		ids = append(ids, id)
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
}

func emitGauge(w io.Writer, ids []string, m map[string]*state, name, help string, f func(Report) float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	for _, id := range ids {
		fmt.Fprintf(w, "%s{device=%q}%g\n", name, id, f(m[id].last))
	}
}
