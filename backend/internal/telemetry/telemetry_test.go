package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestMemoryStoreMetricsUnchanged pins today's exact /metrics behavior for the default backend:
// no sink means no new lines, ever. This is the backwards-compat contract the whole pluggable-sink
// change rests on.
func TestMemoryStoreMetricsUnchanged(t *testing.T) {
	tlm := New()
	now := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)

	tlm.Ingest("rt-1", Report{BatteryPct: 80, RenderedBool: true}, now.Add(-time.Minute))

	var buf bytes.Buffer
	tlm.WriteMetrics(&buf, now)
	out := buf.String()

	if !strings.Contains(out, `md_battery_percent{device="rt-1"}80`) {
		t.Errorf("missing battery gauge:\n%s", out)
	}
	if !strings.Contains(out, "md_last_render_seconds") {
		t.Errorf("missing md_last_render_seconds:\n%s", out)
	}
	if strings.Contains(out, "md_telemetry_sink_errors_total") || strings.Contains(out, "md_telemetry_sink_dropped_total") {
		t.Errorf("sink counters must be absent with a nil sink (memory backend):\n%s", out)
	}
}

// TestLastRenderSeconds pins the semantics of the already-shipped lastRendered gauge (telemetry.go
// state.lastRendered / Ingest), which this workstream must not touch but does need to guard: a
// rendered:false report must never advance it, and a device that has never rendered must be
// omitted entirely rather than emitted with a bogus age.
func TestLastRenderSeconds(t *testing.T) {
	tlm := New()
	now := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)

	// rt-1 never renders.
	tlm.Ingest("rt-1", Report{BatteryPct: 50, RenderedBool: false}, now.Add(-time.Minute))

	var buf bytes.Buffer
	tlm.WriteMetrics(&buf, now)
	if strings.Contains(buf.String(), `md_last_render_seconds{device="rt-1"}`) {
		t.Errorf("rt-1 has never rendered; must be omitted from md_last_render_seconds:\n%s", buf.String())
	}

	// rt-2 renders once, 10 minutes ago, then checks in again 1 minute ago WITHOUT rendering.
	tlm.Ingest("rt-2", Report{BatteryPct: 60, RenderedBool: true}, now.Add(-10*time.Minute))
	tlm.Ingest("rt-2", Report{BatteryPct: 59, RenderedBool: false}, now.Add(-time.Minute))

	buf.Reset()
	tlm.WriteMetrics(&buf, now)
	if !strings.Contains(buf.String(), fmt.Sprintf(`md_last_render_seconds{device="rt-2"}%d`, 600)) {
		t.Errorf("rt-2's later rendered=false report must not advance md_last_render_seconds off the 10-minute-old render:\n%s", buf.String())
	}
}

// TestRoomStatusOnlyDeviceExcludedFromTelemetryGauges pins the existing convention (see state's
// "reported" field doc comment): a device known only via SetRoomStatus must not appear in
// telemetry-derived gauges, but must appear in md_room_status_info.
func TestRoomStatusOnlyDeviceExcludedFromTelemetryGauges(t *testing.T) {
	tlm := New()
	now := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)
	tlm.SetRoomStatus("rt-unflashed", "available")

	var buf bytes.Buffer
	tlm.WriteMetrics(&buf, now)
	out := buf.String()

	if strings.Contains(out, `md_battery_percent{device="rt-unflashed"}`) {
		t.Errorf("a room-status-only device must not appear in telemetry gauges:\n%s", out)
	}
	if !strings.Contains(out, `md_room_status_info{device="rt-unflashed",status="available"}1`) {
		t.Errorf("missing md_room_status_info for room-status-only device:\n%s", out)
	}
}

// fakeSink is an in-memory Sink test double, letting the counter/queue/warm-start tests exercise
// Store's sink plumbing without a real database.
type fakeSink struct {
	mu       sync.Mutex
	records  []Record
	appendFn func(Record) error // nil = always succeed
	closed   bool
}

func (f *fakeSink) Append(rec Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendFn != nil {
		if err := f.appendFn(rec); err != nil {
			return err
		}
	}
	f.records = append(f.records, rec)
	return nil
}

func (f *fakeSink) LoadLatest() ([]Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Last record per device, mirroring what a real "devices" rollup table would return.
	latest := map[string]Record{}
	for _, r := range f.records {
		latest[r.DeviceID] = r
	}
	out := make([]Record, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeSink) Prune(before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var kept []Record
	var removed int64
	for _, r := range f.records {
		if r.LastSeen.Before(before) {
			removed++
			continue
		}
		kept = append(kept, r)
	}
	f.records = kept
	return removed, nil
}

func (f *fakeSink) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

// waitFor polls until cond() is true or the timeout elapses, for asserting on Run's async writer
// goroutine without a fixed sleep racing against -race timing variance.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// TestSinkCountersPresentOnlyWithSink is the mirror of TestMemoryStoreMetricsUnchanged: the two
// new counters must appear, and only when a sink is actually configured.
func TestSinkCountersPresentOnlyWithSink(t *testing.T) {
	sink := &fakeSink{}
	tlm, err := NewWithSink(sink, discardLog(), 16)
	if err != nil {
		t.Fatalf("NewWithSink: %v", err)
	}
	defer tlm.Close()

	var buf bytes.Buffer
	tlm.WriteMetrics(&buf, time.Now())
	out := buf.String()
	if !strings.Contains(out, "md_telemetry_sink_errors_total") || !strings.Contains(out, "md_telemetry_sink_dropped_total") {
		t.Errorf("sink counters must be present when a sink is configured:\n%s", out)
	}
}

// TestIngestNeverBlocksOnFailingSink is the core battery-latency guarantee: a sink whose Append
// always errors must never slow down or fail Ingest, and the failure must be visible as
// sinkErrs rather than silently swallowed.
func TestIngestNeverBlocksOnFailingSink(t *testing.T) {
	sink := &fakeSink{appendFn: func(Record) error { return fmt.Errorf("disk is on fire") }}
	tlm, err := NewWithSink(sink, discardLog(), 16)
	if err != nil {
		t.Fatalf("NewWithSink: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	tlm.Run(ctx, 0, 0)

	now := time.Now()
	for i := 0; i < 8; i++ {
		tlm.Ingest("rt-1", Report{BatteryPct: 50}, now)
	}
	// Ingest itself must have returned already (it's synchronous) — the assertion here is that the
	// in-memory read reflects the last Ingest regardless of the sink's health.
	if snap, ok := tlm.Snapshot("rt-1"); !ok || snap.Report.BatteryPct != 50 {
		t.Fatalf("Ingest must update the in-memory view even when the sink fails: %+v ok=%v", snap, ok)
	}

	waitFor(t, time.Second, func() bool { return tlm.sinkErrs.Load() > 0 })

	cancel()
	if err := tlm.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestQueueFullDropsIncrementCounter proves the design's core trade-off directly: a full queue
// drops the record and increments dropped rather than blocking Ingest.
func TestQueueFullDropsIncrementCounter(t *testing.T) {
	blockAppend := make(chan struct{})
	sink := &fakeSink{appendFn: func(Record) error {
		<-blockAppend // hold the writer goroutine so the queue actually fills up
		return nil
	}}
	tlm, err := NewWithSink(sink, discardLog(), 1)
	if err != nil {
		t.Fatalf("NewWithSink: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tlm.Run(ctx, 0, 0)

	now := time.Now()
	// First Ingest is picked up by Run's goroutine and blocks inside Append; the rest queue up
	// (capacity 1) and then overflow.
	for i := 0; i < 20; i++ {
		tlm.Ingest("rt-1", Report{BatteryPct: 50}, now)
	}

	waitFor(t, time.Second, func() bool { return tlm.dropped.Load() > 0 })

	close(blockAppend)
	cancel()
	if err := tlm.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestWarmStartRepopulatesSnapshotAndMetrics is the restart-recovery guarantee: NewWithSink must
// read the sink's LoadLatest and make it show up in both Snapshot and WriteMetrics immediately,
// with no dependency on a device phoning home again first.
func TestWarmStartRepopulatesSnapshotAndMetrics(t *testing.T) {
	seenAt := time.Date(2026, 6, 24, 8, 0, 0, 0, time.UTC)
	renderedAt := seenAt.Add(-5 * time.Minute)
	sink := &fakeSink{records: []Record{
		{DeviceID: "rt-1", Report: Report{BatteryPct: 42, FirmwareVer: "1.2.3"}, LastSeen: seenAt, LastRendered: renderedAt},
	}}

	tlm, err := NewWithSink(sink, discardLog(), 16)
	if err != nil {
		t.Fatalf("NewWithSink: %v", err)
	}
	defer tlm.Close()

	snap, ok := tlm.Snapshot("rt-1")
	if !ok {
		t.Fatal("warm-started device must be present in Snapshot")
	}
	if snap.Report.BatteryPct != 42 || snap.Report.FirmwareVer != "1.2.3" {
		t.Errorf("warm-started Report mismatch: %+v", snap.Report)
	}
	if !snap.LastSeen.Equal(seenAt) || !snap.LastRendered.Equal(renderedAt) {
		t.Errorf("warm-started timestamps mismatch: lastSeen=%v lastRendered=%v", snap.LastSeen, snap.LastRendered)
	}

	var buf bytes.Buffer
	tlm.WriteMetrics(&buf, seenAt)
	if !strings.Contains(buf.String(), `md_battery_percent{device="rt-1"}42`) {
		t.Errorf("warm-started device must show up in /metrics without waiting for a new report:\n%s", buf.String())
	}
}

// TestCloseIsIdempotent guards Close's closeOnce: calling it twice (e.g. once from a deferred
// cleanup and once from an explicit shutdown path) must not panic (double-close of the channel) or
// double-close the sink.
func TestCloseIsIdempotent(t *testing.T) {
	sink := &fakeSink{}
	tlm, err := NewWithSink(sink, discardLog(), 4)
	if err != nil {
		t.Fatalf("NewWithSink: %v", err)
	}
	tlm.Run(context.Background(), 0, 0)

	if err := tlm.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tlm.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !sink.closed {
		t.Error("sink must be closed")
	}
}

// TestConcurrentIngestDuringClose reproduces a device telemetry POST landing on Ingest at the
// exact moment Close runs — SIGTERM while net/http is still accepting connections (there is no
// graceful srv.Shutdown) and k8s endpoint removal is asynchronous, so this is a real production
// window, not a contrived one. Before closeMu, Close's close(s.q) raced Ingest's
// "select { case s.q <- rec: default: }" and panicked with "send on closed channel"; this must now
// resolve to a counted drop instead.
func TestConcurrentIngestDuringClose(t *testing.T) {
	sink := &fakeSink{}
	tlm, err := NewWithSink(sink, discardLog(), 4)
	if err != nil {
		t.Fatalf("NewWithSink: %v", err)
	}
	tlm.Run(context.Background(), 0, 0)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			now := time.Now()
			for {
				select {
				case <-stop:
					return
				default:
					tlm.Ingest(fmt.Sprintf("rt-%d", i), Report{BatteryPct: 50}, now)
				}
			}
		}(i)
	}

	// Give the goroutines a moment to actually start racing Ingest against the Close below.
	time.Sleep(5 * time.Millisecond)
	if err := tlm.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	close(stop)
	wg.Wait() // a panic in any goroutine fails the test regardless of this Wait returning
}

// TestCloseOnNilSinkIsNoop guards the "memory" backend's Close path: New() never sets a sink or
// starts Run's goroutine, so Close must return immediately without touching a nil channel.
func TestCloseOnNilSinkIsNoop(t *testing.T) {
	tlm := New()
	if err := tlm.Close(); err != nil {
		t.Fatalf("Close on memory-only store: %v", err)
	}
}

// TestRunNoopWithNilSink guards that Run costs nothing at all for the default backend — no
// goroutine, no panic on a nil channel — matching the design's "no-op Run" requirement.
func TestRunNoopWithNilSink(t *testing.T) {
	tlm := New()
	ctx, cancel := context.WithCancel(context.Background())
	tlm.Run(ctx, time.Hour, time.Minute)
	cancel()
	// Nothing to assert beyond "did not panic/hang" — Run must return immediately when sink==nil.
}

// TestPruneRemovesOnlyOldRows exercises Store.Run's ticking prune path end-to-end against the fake
// sink, pinning that Prune is driven off retention and never touches records newer than the cutoff.
func TestPruneRemovesOnlyOldRows(t *testing.T) {
	sink := &fakeSink{}
	tlm, err := NewWithSink(sink, discardLog(), 16)
	if err != nil {
		t.Fatalf("NewWithSink: %v", err)
	}

	// Run must be draining the queue before Prune has anything to find, so start it first (with a
	// short prune interval so the test doesn't need to wait out a real 6h-scale ticker) and only
	// then Ingest the two records.
	ctx, cancel := context.WithCancel(context.Background())
	tlm.Run(ctx, 24*time.Hour, 20*time.Millisecond)

	now := time.Now()
	tlm.Ingest("old", Report{BatteryPct: 10}, now.Add(-48*time.Hour))
	tlm.Ingest("new", Report{BatteryPct: 20}, now)

	waitFor(t, time.Second, func() bool { return sink.count() == 2 })
	waitFor(t, time.Second, func() bool { return sink.count() == 1 })
	if sink.count() != 1 {
		t.Fatalf("want 1 row remaining after prune, got %d", sink.count())
	}

	cancel()
	tlm.Close()
}
