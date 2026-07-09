// Package poller refreshes every room's rendered payload on a fixed cadence, decoupled from
// device wakes. Devices only ever read the cache, so a slow/failing calendar API never blocks
// (or drains) a battery-powered client.
package poller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/render"
)

type Poller struct {
	cfg   *config.Config
	prov  calendar.Provider
	rend  *render.Renderer
	store *cache.Store
	log   *slog.Logger
}

func New(cfg *config.Config, prov calendar.Provider, rend *render.Renderer, store *cache.Store, log *slog.Logger) *Poller {
	return &Poller{cfg: cfg, prov: prov, rend: rend, store: store, log: log}
}

// Run blocks until ctx is cancelled, refreshing all rooms every PollInterval.
func (p *Poller) Run(ctx context.Context) {
	p.refreshAll(ctx) // prime cache immediately at startup
	t := time.NewTicker(p.cfg.PollInterval)
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
	loc := p.cfg.Location()
	now := time.Now().In(loc)
	// Window: from start of today to end of tomorrow (covers "up next" across midnight).
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	to := from.Add(48 * time.Hour)
	nextWake := p.cfg.NextWakeSeconds(now)

	var wg sync.WaitGroup
	// Bound fan-out; 12 rooms is tiny but keep it tidy for larger fleets.
	sem := make(chan struct{}, 6)
	for _, room := range p.cfg.Rooms {
		wg.Add(1)
		sem <- struct{}{}
		go func(room config.Room) {
			defer wg.Done()
			defer func() { <-sem }()
			p.refreshRoom(ctx, room, from, to, now, nextWake)
		}(room)
	}
	wg.Wait()
}

func (p *Poller) refreshRoom(ctx context.Context, room config.Room, from, to, now time.Time, nextWake uint32) {
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	sched, err := p.prov.FetchSchedule(rctx, room.Room, from, to)
	if err != nil {
		p.log.Error("fetch schedule failed", "device", room.DeviceID, "room", room.Room, "err", err)
		p.store.SetError(room.DeviceID, err)
		return
	}
	sched.RoomName = room.Name

	payload := p.rend.Render(sched, now, nextWake)
	etag := fmt.Sprintf("%q", fmt.Sprintf("%08x", payload.CRC32))

	prev, had := p.store.Get(room.DeviceID)
	p.store.Set(room.DeviceID, cache.Entry{
		Payload:    payload,
		ETag:       etag,
		RenderedAt: now,
	})
	if !had || prev.ETag != etag {
		p.log.Info("rendered", "device", room.DeviceID, "etag", etag,
			"bytes", len(payload.Bytes), "compressed", payload.Compressed)
	}
}
