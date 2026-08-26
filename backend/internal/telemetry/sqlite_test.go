package telemetry

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestSQLite(t *testing.T) *SQLiteSink {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telemetry.db")
	sink, err := OpenSQLite(SQLiteOptions{Path: path})
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { sink.Close() })
	return sink
}

// TestSQLiteAppendLoadLatestRoundTrip pins the exact contract Store.NewWithSink depends on: every
// field of a Report survives an Append/LoadLatest round trip, including a nil TempC/RH (the SHT4x
// didn't report that cycle) staying nil rather than becoming a zero value that would be
// indistinguishable from "0.0 degrees, 0% humidity" in a chart.
func TestSQLiteAppendLoadLatestRoundTrip(t *testing.T) {
	sink := openTestSQLite(t)

	seenAt := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)
	renderedAt := seenAt.Add(-2 * time.Minute)
	temp := 21.5
	rh := 43.2

	rec := Record{
		DeviceID: "rt-1",
		Report: Report{
			FirmwareVer: "1.4.0", BatteryMV: 3700, BatteryPct: 61, HeapFree: 120000, HeapMinFree: 90000,
			RSSI: -58, WakeReason: "timer", WakeMS: 850, RenderedBool: true, ErrCode: "", BootCount: 12,
			TempC: &temp, RH: &rh,
		},
		LastSeen:     seenAt,
		LastRendered: renderedAt,
	}
	if err := sink.Append(rec); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// A device with no SHT4x this cycle — TempC/RH nil — must round-trip as nil, not 0.
	rec2 := Record{
		DeviceID: "rt-2",
		Report:   Report{FirmwareVer: "1.4.0", BatteryPct: 80, RenderedBool: false},
		LastSeen: seenAt,
	}
	if err := sink.Append(rec2); err != nil {
		t.Fatalf("Append rt-2: %v", err)
	}

	got, err := sink.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	byID := map[string]Record{}
	for _, r := range got {
		byID[r.DeviceID] = r
	}

	r1, ok := byID["rt-1"]
	if !ok {
		t.Fatal("rt-1 missing from LoadLatest")
	}
	if r1.Report.FirmwareVer != "1.4.0" || r1.Report.BatteryMV != 3700 || r1.Report.BatteryPct != 61 ||
		r1.Report.HeapFree != 120000 || r1.Report.HeapMinFree != 90000 || r1.Report.RSSI != -58 ||
		r1.Report.WakeReason != "timer" || r1.Report.WakeMS != 850 || !r1.Report.RenderedBool ||
		r1.Report.BootCount != 12 {
		t.Errorf("rt-1 Report round-trip mismatch: %+v", r1.Report)
	}
	if r1.Report.TempC == nil || *r1.Report.TempC != temp {
		t.Errorf("rt-1 TempC round-trip mismatch: %+v", r1.Report.TempC)
	}
	if r1.Report.RH == nil || *r1.Report.RH != rh {
		t.Errorf("rt-1 RH round-trip mismatch: %+v", r1.Report.RH)
	}
	if !r1.LastSeen.Equal(seenAt) {
		t.Errorf("rt-1 LastSeen mismatch: got %v want %v", r1.LastSeen, seenAt)
	}
	if !r1.LastRendered.Equal(renderedAt) {
		t.Errorf("rt-1 LastRendered mismatch: got %v want %v", r1.LastRendered, renderedAt)
	}

	r2, ok := byID["rt-2"]
	if !ok {
		t.Fatal("rt-2 missing from LoadLatest")
	}
	if r2.Report.TempC != nil || r2.Report.RH != nil {
		t.Errorf("rt-2 TempC/RH must stay nil when never reported: %+v / %+v", r2.Report.TempC, r2.Report.RH)
	}
	if !r2.LastRendered.IsZero() {
		t.Errorf("rt-2 has never rendered; LastRendered must be zero, got %v", r2.LastRendered)
	}
}

// TestSQLiteAppendNeverRegressesLastRendered pins the COALESCE upsert semantics: a later report
// that did NOT render must not erase the devices rollup's last_rendered_ts, matching the in-memory
// Store's state.lastRendered convention exactly.
func TestSQLiteAppendNeverRegressesLastRendered(t *testing.T) {
	sink := openTestSQLite(t)

	base := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)
	renderedAt := base

	if err := sink.Append(Record{
		DeviceID: "rt-1", Report: Report{RenderedBool: true}, LastSeen: base, LastRendered: renderedAt,
	}); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	// Second report, one minute later, does NOT render — LastRendered stays at the earlier value,
	// same as telemetry.Store.Ingest computes it.
	if err := sink.Append(Record{
		DeviceID: "rt-1", Report: Report{RenderedBool: false}, LastSeen: base.Add(time.Minute), LastRendered: renderedAt,
	}); err != nil {
		t.Fatalf("Append 2: %v", err)
	}

	got, err := sink.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 device, got %d", len(got))
	}
	if !got[0].LastSeen.Equal(base.Add(time.Minute)) {
		t.Errorf("LastSeen should advance to the latest report: got %v", got[0].LastSeen)
	}
	if !got[0].LastRendered.Equal(renderedAt) {
		t.Errorf("LastRendered must not regress: got %v want %v", got[0].LastRendered, renderedAt)
	}
}

// TestSQLitePruneRemovesOnlyOldSamplesNotDevices exercises Prune directly against the samples
// table and confirms the devices rollup — which warm-start reads — is untouched, since
// last_rendered_ts must survive even after the sample that originally set it is pruned away.
func TestSQLitePruneRemovesOnlyOldSamplesNotDevices(t *testing.T) {
	sink := openTestSQLite(t)

	now := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)
	old := now.Add(-100 * 24 * time.Hour)

	if err := sink.Append(Record{DeviceID: "rt-old", Report: Report{BatteryPct: 10}, LastSeen: old}); err != nil {
		t.Fatalf("Append old: %v", err)
	}
	if err := sink.Append(Record{DeviceID: "rt-new", Report: Report{BatteryPct: 20}, LastSeen: now}); err != nil {
		t.Fatalf("Append new: %v", err)
	}

	var sampleCount int
	if err := sink.db.QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&sampleCount); err != nil {
		t.Fatalf("count samples: %v", err)
	}
	if sampleCount != 2 {
		t.Fatalf("want 2 samples before prune, got %d", sampleCount)
	}

	removed, err := sink.Prune(now.Add(-90 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("want 1 row removed, got %d", removed)
	}

	if err := sink.db.QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&sampleCount); err != nil {
		t.Fatalf("count samples after prune: %v", err)
	}
	if sampleCount != 1 {
		t.Errorf("want 1 sample remaining after prune, got %d", sampleCount)
	}

	// devices rollup is untouched by Prune — both devices (including the pruned-sample one) must
	// still resolve via LoadLatest, which is what makes warm-start correct across retention.
	got, err := sink.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Prune must not touch the devices rollup; want 2 devices, got %d", len(got))
	}
}

// TestSQLiteSinkThroughStoreRunEndToEnd wires a real SQLiteSink through Store.NewWithSink and Run
// exactly as main.go does, proving the queue -> Append -> LoadLatest-on-restart path works with
// the real driver, not just the fake test double used in telemetry_test.go.
func TestSQLiteSinkThroughStoreRunEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")

	sink, err := OpenSQLite(SQLiteOptions{Path: path})
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	tlm, err := NewWithSink(sink, discardLog(), 16)
	if err != nil {
		t.Fatalf("NewWithSink: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tlm.Run(ctx, 0, 0) // retention 0 = never prune, isolates this test from prune timing

	now := time.Now()
	tlm.Ingest("rt-1", Report{BatteryPct: 77, RenderedBool: true}, now)

	waitFor(t, time.Second, func() bool {
		got, err := sink.LoadLatest()
		return err == nil && len(got) == 1
	})

	cancel()
	if err := tlm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a restart: open a fresh sink against the same file and warm-start a new Store.
	sink2, err := OpenSQLite(SQLiteOptions{Path: path})
	if err != nil {
		t.Fatalf("re-open OpenSQLite: %v", err)
	}
	defer sink2.Close()
	tlm2, err := NewWithSink(sink2, discardLog(), 16)
	if err != nil {
		t.Fatalf("re-open NewWithSink: %v", err)
	}
	defer tlm2.Close()

	snap, ok := tlm2.Snapshot("rt-1")
	if !ok || snap.Report.BatteryPct != 77 {
		t.Fatalf("warm-start after restart failed: snap=%+v ok=%v", snap, ok)
	}
}

// TestOpenSQLiteCreatesParentDir guards the "operator points telemetry.sqlite.path at a directory
// that doesn't exist yet" case documented in the config surface (default
// /var/lib/meeting-displays/telemetry.db) — OpenSQLite must create it rather than erroring, since
// main.go's fallback-to-memory path is for genuine failures (read-only mount, corruption), not a
// missing directory on first boot.
func TestOpenSQLiteCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "telemetry.db")
	sink, err := OpenSQLite(SQLiteOptions{Path: path})
	if err != nil {
		t.Fatalf("OpenSQLite should create missing parent dirs: %v", err)
	}
	defer sink.Close()
}
