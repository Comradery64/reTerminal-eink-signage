// Package poller refreshes every room's rendered payload on a fixed cadence, decoupled from
// device wakes. Devices only ever read the cache, so a slow/failing calendar API never blocks
// (or drains) a battery-powered client.
package poller

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"log/slog"
	"sync"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/render"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

type Poller struct {
	cfg   *config.Live
	prov  calendar.Provider
	rend  *render.Renderer
	store *cache.Store
	tlm   *telemetry.Store
	log   *slog.Logger
}

func New(cfg *config.Live, prov calendar.Provider, rend *render.Renderer, store *cache.Store, tlm *telemetry.Store, log *slog.Logger) *Poller {
	return &Poller{cfg: cfg, prov: prov, rend: rend, store: store, tlm: tlm, log: log}
}

// Run blocks until ctx is cancelled, refreshing all rooms every PollInterval.
func (p *Poller) Run(ctx context.Context) {
	p.refreshAll(ctx) // prime cache immediately at startup
	t := time.NewTicker(p.cfg.Load().PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refreshAll(ctx)
		}
	}
}

func (p *Poller) refreshAll(ctx context.Context) {
	// Snapshot once per tick: a config write mid-tick should only take effect starting the next
	// tick, not partway through this one.
	cfg := p.cfg.Load()
	loc := cfg.Location()
	now := time.Now().In(loc)
	// Window: from start of today to end of tomorrow (covers "up next" across midnight).
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	to := from.Add(48 * time.Hour)

	var wg sync.WaitGroup
	// Bound fan-out; 12 rooms is tiny but keep it tidy for larger fleets.
	sem := make(chan struct{}, 6)
	for _, room := range cfg.Rooms {
		wg.Add(1)
		sem <- struct{}{}
		go func(room config.Room) {
			defer wg.Done()
			defer func() { <-sem }()
			p.refreshRoom(ctx, room, from, to, now)
		}(room)
	}
	wg.Wait()
}

func (p *Poller) refreshRoom(ctx context.Context, room config.Room, from, to, now time.Time) {
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	sched, err := p.prov.FetchSchedule(rctx, room.Room, from, to)
	if err != nil {
		p.log.Error("fetch schedule failed", "device", room.DeviceID, "room", room.Room, "err", err)
		p.store.SetError(room.DeviceID, err)
		return
	}
	sched.RoomName = room.Name
	cur, next := sched.Current(now), sched.Next(now)
	p.tlm.SetRoomStatus(room.DeviceID, calendar.RoomStatus(cur, next, now))

	// Compose once and derive both the device payload and the human-readable preview from the
	// same paletted image, rather than composing the layout twice.
	pal := p.rend.Compose(sched, now)
	payload := render.Encode(render.Pack(pal), p.rend.W, p.rend.H, 0, true)
	etag := fmt.Sprintf("%q", fmt.Sprintf("%08x", payload.CRC32))

	// Encoded once here (not on demand per HTTP request) since PNG-encoding an 800x480 palette
	// image is cheap but needless to repeat for every /dashboard card load between polls.
	var previewPNG []byte
	var buf bytes.Buffer
	if err := png.Encode(&buf, pal); err == nil {
		previewPNG = buf.Bytes()
	} else {
		p.log.Error("preview PNG encode failed", "device", room.DeviceID, "err", err)
	}

	prev, had := p.store.Get(room.DeviceID)
	p.store.Set(room.DeviceID, cache.Entry{
		Payload:    payload,
		PreviewPNG: previewPNG,
		ETag:       etag,
		RenderedAt: now,
		Cur:        cur,
		Next:       next,
	})
	if !had || prev.ETag != etag {
		p.log.Info("rendered", "device", room.DeviceID, "etag", etag,
			"bytes", len(payload.Bytes), "compressed", payload.Compressed)
	}
}
