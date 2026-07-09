// Package calendar reads room schedules from Google Workspace behind a single Provider
// interface that returns a normalized room schedule. (A demo provider is also available for
// local testing without any cloud setup — see demo.go.)
package calendar

import (
	"context"
	"sort"
	"time"
)

// Event is a normalized calendar entry, provider-agnostic.
type Event struct {
	Subject   string
	Organizer string
	Start     time.Time
	End       time.Time
	Private   bool // honor "private" sensitivity: subject is suppressed on the display
}

// Schedule is the day view for a single room.
type Schedule struct {
	RoomName  string
	Events    []Event // sorted ascending by Start, only today's relevant window
	FetchedAt time.Time
}

// Now-relative helpers used by the renderer.

// Current returns the meeting in progress at t, if any.
func (s *Schedule) Current(t time.Time) *Event {
	for i := range s.Events {
		if !t.Before(s.Events[i].Start) && t.Before(s.Events[i].End) {
			return &s.Events[i]
		}
	}
	return nil
}

// Next returns the soonest meeting starting at or after t, if any.
func (s *Schedule) Next(t time.Time) *Event {
	for i := range s.Events {
		if !s.Events[i].Start.Before(t) {
			return &s.Events[i]
		}
	}
	return nil
}

// Provider fetches the schedule for a room mailbox over a time window.
type Provider interface {
	// FetchSchedule returns events for roomEmail within [from, to].
	FetchSchedule(ctx context.Context, roomEmail string, from, to time.Time) (*Schedule, error)
}

// normalize sorts events and drops anything fully in the past relative to `from`.
func normalize(name string, events []Event, from time.Time, fetchedAt time.Time) *Schedule {
	out := events[:0]
	for _, e := range events {
		if e.End.After(from) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return &Schedule{RoomName: name, Events: out, FetchedAt: fetchedAt}
}
