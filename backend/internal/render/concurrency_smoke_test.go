package render

import (
	"sync"
	"testing"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
)

// TestConcurrentCompose guards against sharing non-concurrent-safe font.Face objects across
// goroutines — the poller renders every room concurrently, one goroutine per room.
func TestConcurrentCompose(t *testing.T) {
	r := New(800, 480, true)
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sched := &calendar.Schedule{RoomName: "Room", Events: []calendar.Event{
				{Subject: "Meeting", Start: now.Add(-time.Duration(i) * time.Minute), End: now.Add(time.Duration(30-i) * time.Minute)},
			}}
			r.Compose(sched, now)
		}(i)
	}
	wg.Wait()
}
