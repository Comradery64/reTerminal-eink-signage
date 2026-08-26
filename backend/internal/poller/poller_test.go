package poller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/cache"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/render"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/telemetry"
)

// countingProvider records every calendar address it was asked about, so a test can assert the
// poller fetched once per room rather than once per display.
type countingProvider struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (p *countingProvider) FetchSchedule(_ context.Context, roomEmail string, _, _ time.Time) (*calendar.Schedule, error) {
	p.mu.Lock()
	p.calls = append(p.calls, roomEmail)
	p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	return &calendar.Schedule{FetchedAt: time.Now()}, nil
}

func (p *countingProvider) countFor(addr string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c == addr {
			n++
		}
	}
	return n
}

func testPoller(t *testing.T, rooms []config.Room, prov calendar.Provider) (*Poller, *cache.Store) {
	t.Helper()
	cfg := &config.Config{
		Provider:     "demo",
		PollInterval: time.Minute,
		Rooms:        rooms,
		Wake:         config.WakeConfig{Timezone: "UTC"},
		Render:       config.RenderConfig{Width: 800, Height: 480},
	}
	store := cache.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(config.NewLive(cfg), prov, render.New(800, 480, false), store, telemetry.New(), log)
	return p, store
}

func twoPanelRoom() []config.Room {
	return []config.Room{
		{DeviceID: "rt-door", Name: "Aspen", Room: "aspen@x", Label: "Door"},
		{DeviceID: "rt-wall", Name: "Aspen", Room: "aspen@x", Label: "Interior wall"},
		{DeviceID: "rt-other", Name: "Birch", Room: "birch@x"},
	}
}

// The whole point of grouping by address: three displays across two rooms must cost two freebusy
// calls per tick, not three. Polling per display would burn Google quota recomputing an identical
// answer for every extra panel a room gains.
func TestRefreshAllFetchesOncePerRoomAddress(t *testing.T) {
	prov := &countingProvider{}
	p, _ := testPoller(t, twoPanelRoom(), prov)

	p.refreshAll(context.Background())

	if n := prov.countFor("aspen@x"); n != 1 {
		t.Errorf("aspen@x fetched %d times, want 1 despite having two displays", n)
	}
	if n := prov.countFor("birch@x"); n != 1 {
		t.Errorf("birch@x fetched %d times, want 1", n)
	}
}

// Both panels in a room still have to end up with their own cache entry — deduplicating the fetch
// must not deduplicate the delivery.
func TestRefreshAllPopulatesEveryDisplayInARoom(t *testing.T) {
	prov := &countingProvider{}
	p, store := testPoller(t, twoPanelRoom(), prov)

	p.refreshAll(context.Background())

	for _, id := range []string{"rt-door", "rt-wall", "rt-other"} {
		entry, ok := store.Get(id)
		if !ok {
			t.Errorf("no cache entry for %s", id)
			continue
		}
		if len(entry.Payload.Bytes) == 0 {
			t.Errorf("%s has an empty payload", id)
		}
	}

	// Panels sharing a room and a name show the same thing, so they must agree byte for byte —
	// a device that got a different ETag would refresh out of step with its sibling.
	door, _ := store.Get("rt-door")
	wall, _ := store.Get("rt-wall")
	if door.ETag != wall.ETag {
		t.Errorf("two panels in one room disagree: door ETag %s, wall ETag %s", door.ETag, wall.ETag)
	}
}

// A failed fetch has to fail every display showing that room; marking only the first would leave
// its siblings serving a stale schedule with no error recorded against them.
func TestRefreshAllRecordsErrorForEveryDisplayInARoom(t *testing.T) {
	prov := &countingProvider{err: errors.New("freebusy boom")}
	p, store := testPoller(t, twoPanelRoom(), prov)

	p.refreshAll(context.Background())

	for _, id := range []string{"rt-door", "rt-wall"} {
		entry, ok := store.Get(id)
		if !ok {
			t.Errorf("no cache entry recorded for %s", id)
			continue
		}
		if entry.Err == nil {
			t.Errorf("%s has no error recorded despite the room's fetch failing", id)
		}
		if len(entry.Payload.Bytes) != 0 {
			t.Errorf("%s got a payload despite the fetch failing", id)
		}
	}
}

func TestGroupByRoomAddress(t *testing.T) {
	groups := groupByRoomAddress([]config.Room{
		{DeviceID: "a", Room: "one@x"},
		{DeviceID: "b", Room: "two@x"},
		{DeviceID: "c", Room: "ONE@X"}, // same room, different case
	})
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(groups))
	}
	if len(groups[0]) != 2 || groups[0][0].DeviceID != "a" || groups[0][1].DeviceID != "c" {
		t.Errorf("first group should hold a and c in config order, got %+v", groups[0])
	}
	if len(groups[1]) != 1 || groups[1][0].DeviceID != "b" {
		t.Errorf("second group should hold just b, got %+v", groups[1])
	}
}
