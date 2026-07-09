package calendar

import (
	"context"
	"time"
)

// DemoProvider returns a canned schedule for every room so the broker can run with no Google
// setup — useful for local bring-up, the preview path, and fake_device.sh testing.
// Enable with the broker's -demo flag (or provider: "demo").
type DemoProvider struct{}

func NewDemo() *DemoProvider { return &DemoProvider{} }

func (DemoProvider) FetchSchedule(_ context.Context, roomEmail string, from, to time.Time) (*Schedule, error) {
	now := time.Now()
	mk := func(subj string, startMin, durMin int, private bool) Event {
		s := now.Add(time.Duration(startMin) * time.Minute)
		return Event{Subject: subj, Start: s, End: s.Add(time.Duration(durMin) * time.Minute), Private: private}
	}
	events := []Event{
		mk("Quarterly planning", -20, 60, false), // in progress now
		mk("Vendor demo", 55, 30, false),
		mk("Eng sync", 145, 30, true),
	}
	return normalize(roomEmail, events, from, now), nil
}

var _ Provider = (*DemoProvider)(nil)
