package telemetry

import "time"

// Record is one durable telemetry row: the report plus the two derived timestamps the in-memory
// view needs to rebuild a device's state after a restart (see Store.NewWithSink). LastRendered is
// carried separately from Report because "did this report actually cause a repaint" is derived
// across reports (it only advances when RenderedBool is true), not a field of any single one.
type Record struct {
	DeviceID     string
	Report       Report
	LastSeen     time.Time
	LastRendered time.Time // zero if the device has never reported rendered=true
}

// Sink is the durable side of telemetry: everything /metrics, /status and /dashboard need lives in
// the in-memory Store regardless of whether a Sink is configured at all (backend "memory", the
// default). A Sink only ever adds a second, durable copy of the same data.
//
// Implementations are called from a single writer goroutine (see Store.Run), so they need no
// internal locking beyond what their own driver already requires — this is deliberate: it lets a
// sqlite-backed Sink hold one connection and never see SQLITE_BUSY from its own caller.
type Sink interface {
	// Append records one report. History is observability, not fleet operation: a failed Append
	// must never fail a device's telemetry POST, which is why Ingest only ever hands a Record to
	// Run's queue rather than calling Append directly (see telemetry.go).
	Append(rec Record) error

	// LoadLatest returns the most recent record per device, read once at startup to warm-start the
	// in-memory map. Without this, a restart would leave /metrics, /status and /dashboard blank for
	// every device until it next phones home — on a battery-powered device polling every 10-30
	// minutes, that's not a blip, it's most of an on-call shift looking like a total fleet outage.
	LoadLatest() ([]Record, error)

	// Prune deletes samples older than before, returning the number of rows removed. Called on a
	// ticker from Run, never inline with Append, so retention bookkeeping never adds latency to the
	// write path.
	Prune(before time.Time) (int64, error)

	Close() error
}
