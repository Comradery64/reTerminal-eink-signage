package telemetry

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, keeps the broker a single static binary
)

// SQLiteOptions configures OpenSQLite. Retention isn't here: pruning is entirely owned by
// Store.Run's ticker (see telemetry.go), which is what actually calls Sink.Prune — OpenSQLite only
// opens the file and creates the schema, so a retention field on this struct would be dead state
// nobody reads, not "the obvious place" a future maintainer might assume it is.
type SQLiteOptions struct {
	Path string
}

// SQLiteSink is the "sqlite" telemetry backend: modernc.org/sqlite (pure Go, no cgo — verified
// building at CGO_ENABLED=0 for both linux/arm64 and linux/amd64) appending every accepted report
// to a typed time series plus a JSON rollup that warm-starts the in-memory Store after a restart.
type SQLiteSink struct {
	db *sql.DB
}

// OpenSQLite opens (creating if necessary) the sqlite file at opts.Path, applies pragmas, and
// creates the schema if missing. Callers (main.go) are expected to fall back to telemetry.New()
// (memory) on error rather than refusing to boot: telemetry history is observability, and a full
// disk or a bad mount must not take a room-display fleet offline. That is the opposite policy from
// config persistence, deliberately — losing history misleads nobody, a lost config write tells a
// human "Saved" when nothing was saved.
func OpenSQLite(opts SQLiteOptions) (*SQLiteSink, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("telemetry sqlite: empty path")
	}
	if dir := filepath.Dir(opts.Path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("telemetry sqlite: create dir %s: %w", dir, err)
		}
	}

	// busy_timeout+WAL+synchronous=NORMAL is the standard "single writer, don't corrupt on power
	// loss, don't fsync every row" combination for an append-mostly local file. MaxOpenConns(1)
	// because the only writer is Store.Run's single goroutine (see telemetry.go) — one connection
	// removes SQLITE_BUSY between our own callers entirely rather than needing busy_timeout to ever
	// actually kick in.
	dsn := "file:" + opts.Path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("telemetry sqlite: open %s: %w", opts.Path, err)
	}
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("telemetry sqlite: ping %s: %w", opts.Path, err)
	}

	// auto_vacuum must be set before any table is created (sqlite only honors it on an empty
	// database), so it runs first even though incremental_vacuum itself is only invoked after each
	// prune.
	if _, err := db.Exec(`PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("telemetry sqlite: set auto_vacuum: %w", err)
	}

	if err := createSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	return &SQLiteSink{db: db}, nil
}

func createSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS samples (
			device_id   TEXT    NOT NULL,
			ts          INTEGER NOT NULL,
			battery_mv  INTEGER NOT NULL,
			battery_pct INTEGER NOT NULL,
			rssi        INTEGER NOT NULL,
			heap_free   INTEGER NOT NULL,
			heap_min    INTEGER NOT NULL,
			wake_ms     INTEGER NOT NULL,
			boot_count  INTEGER NOT NULL,
			rendered    INTEGER NOT NULL,
			wake_reason TEXT    NOT NULL,
			fw          TEXT    NOT NULL,
			err         TEXT    NOT NULL DEFAULT '',
			temp_c      REAL,
			rh          REAL,
			PRIMARY KEY (device_id, ts)
		) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS samples_ts ON samples(ts)`,
		`CREATE TABLE IF NOT EXISTS devices (
			device_id        TEXT PRIMARY KEY,
			last_seen_ts     INTEGER NOT NULL,
			last_rendered_ts INTEGER,
			last_report      TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("telemetry sqlite: create schema: %w", err)
		}
	}
	return nil
}

// Append writes rec as one samples row (INSERT OR REPLACE — device_id+ts is the natural key, and a
// duplicate report for the same second is expected to overwrite, not conflict) and upserts the
// devices rollup that warm-start reads on the next boot.
func (sk *SQLiteSink) Append(rec Record) error {
	tx, err := sk.db.Begin()
	if err != nil {
		return fmt.Errorf("telemetry sqlite: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if Commit succeeded

	ts := rec.LastSeen.UTC().Unix()
	r := rec.Report
	rendered := 0
	if r.RenderedBool {
		rendered = 1
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO samples
		(device_id, ts, battery_mv, battery_pct, rssi, heap_free, heap_min, wake_ms, boot_count, rendered, wake_reason, fw, err, temp_c, rh)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.DeviceID, ts, r.BatteryMV, r.BatteryPct, r.RSSI, r.HeapFree, r.HeapMinFree, r.WakeMS, r.BootCount,
		rendered, r.WakeReason, r.FirmwareVer, r.ErrCode, r.TempC, r.RH)
	if err != nil {
		return fmt.Errorf("telemetry sqlite: insert sample: %w", err)
	}

	reportJSON, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("telemetry sqlite: marshal report: %w", err)
	}

	var lastRenderedTS any
	if !rec.LastRendered.IsZero() {
		lastRenderedTS = rec.LastRendered.UTC().Unix()
	}

	// COALESCE(?, last_rendered_ts) never regresses last_rendered_ts to NULL: a Record whose
	// LastRendered is zero (this report didn't render) must not erase a device's last known render
	// time — that timestamp has to survive across however many rendered=false reports come after it,
	// same semantic the in-memory Store's state.lastRendered already has.
	_, err = tx.Exec(`INSERT INTO devices (device_id, last_seen_ts, last_rendered_ts, last_report)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			last_seen_ts = excluded.last_seen_ts,
			last_rendered_ts = COALESCE(excluded.last_rendered_ts, devices.last_rendered_ts),
			last_report = excluded.last_report`,
		rec.DeviceID, ts, lastRenderedTS, string(reportJSON))
	if err != nil {
		return fmt.Errorf("telemetry sqlite: upsert device: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("telemetry sqlite: commit: %w", err)
	}
	return nil
}

// LoadLatest reads the devices rollup — not samples — so warm-start is an N-row read (N = fleet
// size) instead of a MAX(ts)-per-device scan over potentially weeks of history.
func (sk *SQLiteSink) LoadLatest() ([]Record, error) {
	rows, err := sk.db.Query(`SELECT device_id, last_seen_ts, last_rendered_ts, last_report FROM devices`)
	if err != nil {
		return nil, fmt.Errorf("telemetry sqlite: load latest: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var (
			deviceID       string
			lastSeenTS     int64
			lastRenderedTS sql.NullInt64
			reportJSON     string
		)
		if err := rows.Scan(&deviceID, &lastSeenTS, &lastRenderedTS, &reportJSON); err != nil {
			return nil, fmt.Errorf("telemetry sqlite: scan device row: %w", err)
		}
		var report Report
		if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
			return nil, fmt.Errorf("telemetry sqlite: unmarshal report for %s: %w", deviceID, err)
		}
		rec := Record{
			DeviceID: deviceID,
			Report:   report,
			LastSeen: time.Unix(lastSeenTS, 0).UTC(),
		}
		if lastRenderedTS.Valid {
			rec.LastRendered = time.Unix(lastRenderedTS.Int64, 0).UTC()
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("telemetry sqlite: iterate devices: %w", err)
	}
	return out, nil
}

// Prune deletes samples older than before and reclaims their pages via incremental_vacuum — with
// auto_vacuum=INCREMENTAL set at creation, a plain DELETE leaves free pages inside the file without
// this, which would mean the .db file only ever grows even at steady-state retention.
func (sk *SQLiteSink) Prune(before time.Time) (int64, error) {
	res, err := sk.db.Exec(`DELETE FROM samples WHERE ts < ?`, before.UTC().Unix())
	if err != nil {
		return 0, fmt.Errorf("telemetry sqlite: prune: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("telemetry sqlite: prune rows affected: %w", err)
	}
	if _, err := sk.db.Exec(`PRAGMA incremental_vacuum`); err != nil {
		// Not fatal to the prune itself — the rows are already gone — but worth surfacing so an
		// operator watching md_telemetry_sink_errors_total notices before disk usage creeps.
		return n, fmt.Errorf("telemetry sqlite: incremental_vacuum: %w", err)
	}
	return n, nil
}

func (sk *SQLiteSink) Close() error { return sk.db.Close() }
