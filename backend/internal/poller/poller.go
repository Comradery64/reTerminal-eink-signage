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
	"strings"
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

	// Fan out per calendar address, not per display. A room with several panels (a door plaque
	// plus an interior display) has several config.Room entries sharing one address; polling each
	// entry separately would issue N identical freebusy calls per tick against the same calendar,
	// burning Google quota to compute the same answer N times.
	var wg sync.WaitGroup
	// Bound fan-out; 12 rooms is tiny but keep it tidy for larger fleets.
	sem := make(chan struct{}, 6)
	for _, group := range groupByRoomAddress(cfg.Rooms) {
		wg.Add(1)
		sem <- struct{}{}
		go func(group []config.Room) {
			defer wg.Done()
			defer func() { <-sem }()
			p.refreshRoom(ctx, group, from, to, now)
		}(group)
	}
	wg.Wait()
}

// groupByRoomAddress buckets displays by the calendar address they show, preserving the config's
// room ordering so logs stay stable tick to tick. Addresses are compared case-insensitively, the
// same way config.Validate enforces distinct labels within a room.
func groupByRoomAddress(rooms []config.Room) [][]config.Room {
	index := map[string]int{}
	var groups [][]config.Room
	for _, r := range rooms {
		key := strings.ToLower(r.Room)
		if i, ok := index[key]; ok {
			groups[i] = append(groups[i], r)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, []config.Room{r})
	}
	return groups
}

// refreshRoom fetches one calendar address once and updates every display showing it. group is
// never empty and every member shares the same Room address (see groupByRoomAddress); a fetch
// failure fails all of them together, since they'd all have shown the same stale schedule.
func (p *Poller) refreshRoom(ctx context.Context, group []config.Room, from, to, now time.Time) {
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	address := group[0].Room
	sched, err := p.prov.FetchSchedule(rctx, address, from, to)
	if err != nil {
		for _, room := range group {
			p.log.Error("fetch schedule failed", "device", room.DeviceID, "room", room.Room, "err", err)
			p.store.SetError(room.DeviceID, err)
		}
		return
	}

	// Compose is keyed on the schedule plus the displayed room name, so panels in the same room
	// that also share a Name render byte-identical images — compose once and hand the same payload
	// to each. Room.Label deliberately doesn't reach the renderer (it's a dashboard-side
	// distinction, not something to print on a door plaque), but Name is allowed to differ, so key
	// the memo on it rather than assuming one image per address.
	type composed struct {
		entry cache.Entry
		etag  string
	}
	byName := map[string]composed{}

	for _, room := range group {
		cur, next := sched.Current(now), sched.Next(now)
		p.tlm.SetRoomStatus(room.DeviceID, calendar.RoomStatus(cur, next, now))

		c, done := byName[room.Name]
		if !done {
			// Copy before setting RoomName: sched is shared by every display in this group, so
			// mutating it in place would let the last panel's name leak into the others.
			named := *sched
			named.RoomName = room.Name

			// Compose once and derive both the device payload and the human-readable preview from
			// the same paletted image, rather than composing the layout twice.
			pal := p.rend.Compose(&named, now)
			payload := render.Encode(render.Pack(pal), p.rend.W, p.rend.H, 0, true)
			c.etag = fmt.Sprintf("%q", fmt.Sprintf("%08x", payload.CRC32))

			// Encoded once here (not on demand per HTTP request) since PNG-encoding an 800x480
			// palette image is cheap but needless to repeat for every /dashboard card load between
			// polls.
			var previewPNG []byte
			var buf bytes.Buffer
			if err := png.Encode(&buf, pal); err == nil {
				previewPNG = buf.Bytes()
			} else {
				p.log.Error("preview PNG encode failed", "device", room.DeviceID, "err", err)
			}

			c.entry = cache.Entry{
				Payload:    payload,
				PreviewPNG: previewPNG,
				ETag:       c.etag,
				RenderedAt: now,
				Cur:        cur,
				Next:       next,
			}
			byName[room.Name] = c
		}

		prev, had := p.store.Get(room.DeviceID)
		p.store.Set(room.DeviceID, c.entry)
		if !had || prev.ETag != c.etag {
			p.log.Info("rendered", "device", room.DeviceID, "etag", c.etag,
				"bytes", len(c.entry.Payload.Bytes), "compressed", c.entry.Payload.Compressed)
		}
	}
}
