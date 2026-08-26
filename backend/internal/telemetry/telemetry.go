// Package telemetry ingests device health reports and exposes them in Prometheus exposition
// format so the existing k3s monitoring stack can scrape /metrics.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
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

	// sink and everything below are nil/zero for New() — the default "memory" backend. Every path
	// below is gated on sink != nil so a flagless deployment gets byte-identical behavior to before
	// this change: no goroutines beyond nothing, no files, no extra /metrics lines.
	sink Sink
	log  *slog.Logger

	// q carries Records from Ingest to Run's single writer goroutine. Buffered and drained
	// asynchronously so a slow or full disk never adds latency to a device's telemetry POST — see
	// Ingest's comment for why that matters on a battery-powered device.
	q chan Record

	sinkErrs atomic.Int64 // Append/Prune failures, surfaced as md_telemetry_sink_errors_total
	dropped  atomic.Int64 // Records dropped because q was full, surfaced as md_telemetry_sink_dropped_total

	wg        sync.WaitGroup // Run's writer goroutine, so Close can wait for it to drain and exit
	closeOnce sync.Once

	// closeMu guards q's open/closed state against Ingest's send. A device telemetry POST can
	// land on Ingest at the exact moment Close runs (SIGTERM while net/http is still accepting
	// connections — there's no graceful srv.Shutdown — and k8s endpoint removal is asynchronous),
	// so Close cannot just close(q): Ingest's "select { case s.q <- rec: default: }" is only safe
	// from a concurrent close if the two are mutually exclusive. Ingest holds the read side for
	// the duration of its send; Close takes the write side, so it only closes q once it knows no
	// Ingest call is (or ever will be, since closed is set first) still sending — turning what used
	// to be a "send on closed channel" panic into a plain counted drop.
	closeMu sync.RWMutex
	closed  bool
}

func New() *Store { return &Store{m: make(map[string]*state)} }

// NewWithSink constructs a Store backed by a durable Sink and warm-starts the in-memory map from
// sink.LoadLatest(), so /metrics, /status and /dashboard are not blank for a whole poll cycle
// after a restart. queueSize sizes the channel Ingest feeds and Run drains (config_persistence's
// telemetry.sqlite.queue_size — default 256; see Ingest for why a bounded, droppable queue beats a
// blocking one here).
//
// roomStatus is deliberately left empty for every warm-started device: it's not part of Record
// (it comes from the poller's calendar computation, not firmware telemetry — see SetRoomStatus),
// and the poller repopulates it for every configured room within one tick of startup anyway.
func NewWithSink(sink Sink, log *slog.Logger, queueSize int) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if queueSize <= 0 {
		queueSize = 256
	}
	s := &Store{
		m:    make(map[string]*state),
		sink: sink,
		log:  log,
		q:    make(chan Record, queueSize),
	}
	recs, err := sink.LoadLatest()
	if err != nil {
		return nil, fmt.Errorf("telemetry: warm-start from sink failed: %w", err)
	}
	for _, rec := range recs {
		s.m[rec.DeviceID] = &state{
			last:         rec.Report,
			lastSeen:     rec.LastSeen,
			lastRendered: rec.LastRendered,
			reported:     true,
		}
	}
	return s, nil
}

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

	// Disk I/O must never happen under the fleet-wide RWMutex above, and must never block this
	// call at all: this runs on the device's telemetry POST, and every extra 100ms awake is battery
	// this project spent three months of design budget saving. A non-blocking send means a full
	// queue drops the newest history sample instead of stalling a wake — strictly the cheaper
	// failure of the two.
	if s.sink != nil {
		// closeMu's read side makes this send mutually exclusive with Close closing q (see closeMu's
		// doc comment) — without it, a POST landing during shutdown would panic instead of dropping.
		s.closeMu.RLock()
		if s.closed {
			s.dropped.Add(1)
		} else {
			select {
			case s.q <- Record{DeviceID: deviceID, Report: r, LastSeen: now, LastRendered: lastRendered}:
			default:
				s.dropped.Add(1)
			}
		}
		s.closeMu.RUnlock()
	}
}

// Run owns the only goroutine that ever touches the sink: it drains q as records arrive (so
// history is written close to real-time) and, on a ticker, prunes samples older than retention.
// It returns immediately when sink is nil (backend "memory"/"prometheus"), so those backends incur
// no extra goroutine at all.
//
// retention <= 0 disables pruning (telemetry.sqlite.retention_days: 0, documented as "keep
// forever, unbounded growth, not recommended").
func (s *Store) Run(ctx context.Context, retention, pruneInterval time.Duration) {
	if s.sink == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		var ticker *time.Ticker
		var tickC <-chan time.Time
		if pruneInterval > 0 {
			ticker = time.NewTicker(pruneInterval)
			defer ticker.Stop()
			tickC = ticker.C
		}

		// Prune once at start, not just on the first tick, so a broker that restarts more often
		// than pruneInterval (6h default) still eventually reclaims space rather than never pruning
		// at all.
		s.prune(retention)

		for {
			select {
			case rec, ok := <-s.q:
				if !ok {
					return
				}
				if err := s.sink.Append(rec); err != nil {
					s.sinkErrs.Add(1)
					s.log.Error("telemetry sink append failed", "device", rec.DeviceID, "err", err)
				}
			case <-tickC:
				s.prune(retention)
			case <-ctx.Done():
				// Drain whatever is already queued before exiting, so a normal shutdown doesn't
				// discard the last few in-flight reports.
				for {
					select {
					case rec, ok := <-s.q:
						if !ok {
							return
						}
						if err := s.sink.Append(rec); err != nil {
							s.sinkErrs.Add(1)
							s.log.Error("telemetry sink append failed", "device", rec.DeviceID, "err", err)
						}
					default:
						return
					}
				}
			}
		}
	}()
}

func (s *Store) prune(retention time.Duration) {
	if retention <= 0 {
		return
	}
	n, err := s.sink.Prune(time.Now().Add(-retention))
	if err != nil {
		s.sinkErrs.Add(1)
		s.log.Error("telemetry sink prune failed", "err", err)
		return
	}
	if n > 0 {
		s.log.Info("telemetry sink pruned old samples", "rows_removed", n)
	}
}

// Close drains the queue, waits for Run's writer goroutine to exit, and closes the sink. Safe to
// call multiple times (idempotent) and safe to call when sink is nil (both are no-ops beyond the
// first call).
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.sink == nil {
			return
		}
		s.closeMu.Lock()
		s.closed = true
		close(s.q)
		s.closeMu.Unlock()
		s.wg.Wait()
		err = s.sink.Close()
	})
	return err
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

	// Only emitted when a sink is configured (backend "sqlite"): emitting these unconditionally
	// would change /metrics output for the default "memory" backend, which the backwards-compat
	// constraint forbids. Fleet-wide (no device label) since they describe the sink itself, not any
	// one device.
	if s.sink != nil {
		fmt.Fprintln(w, "# HELP md_telemetry_sink_errors_total Telemetry sink Append/Prune failures.")
		fmt.Fprintln(w, "# TYPE md_telemetry_sink_errors_total counter")
		fmt.Fprintf(w, "md_telemetry_sink_errors_total %d\n", s.sinkErrs.Load())

		fmt.Fprintln(w, "# HELP md_telemetry_sink_dropped_total Telemetry records dropped because the sink's write queue was full.")
		fmt.Fprintln(w, "# TYPE md_telemetry_sink_dropped_total counter")
		fmt.Fprintf(w, "md_telemetry_sink_dropped_total %d\n", s.dropped.Load())
	}
}

func emitGauge(w io.Writer, ids []string, m map[string]*state, name, help string, f func(Report) float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	for _, id := range ids {
		fmt.Fprintf(w, "%s{device=%q}%g\n", name, id, f(m[id].last))
	}
}
