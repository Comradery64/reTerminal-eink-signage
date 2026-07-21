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

// StartingSoonWindow is how far ahead of a meeting the room is considered "starting soon"
// rather than "available". Shared by the display panel, fleet telemetry, and wake scheduling so
// a building manager looking at Grafana always sees the same state as what's on the physical
// panel, and the device is woken exactly when this state would change.
const StartingSoonWindow = 10 * time.Minute

// BackToBackWindow is how close a following meeting must start to the current one's end to be
// treated as "back to back" — worth revealing early — rather than a real gap after the meeting.
const BackToBackWindow = 5 * time.Minute

// TransitionBuffer is added after every computed transition instant before waking the device, so
// the server-side state has unambiguously already changed by the time the device asks — the same
// race the grid-alignment offset guarded against, generalized to calendar-driven transitions.
const TransitionBuffer = 30 * time.Second

// RoomStatus derives the coarse room state from the current/next event, as of now.
func RoomStatus(cur, next *Event, now time.Time) string {
	switch {
	case cur != nil:
		return "in_meeting"
	case next != nil && next.Start.Sub(now) <= StartingSoonWindow:
		return "starting_soon"
	default:
		return "available"
	}
}

// BackToBack returns the immediately-following event if it starts within BackToBackWindow of
// cur's end, so the render/telemetry layers can decide whether to reveal a "next meeting" preview.
func BackToBack(cur *Event, next *Event) *Event {
	if cur == nil || next == nil {
		return nil
	}
	if next.Start.Sub(cur.End) <= BackToBackWindow {
		return next
	}
	return nil
}

// NextTransitionAt returns the next instant (already including TransitionBuffer) at which the
// room's displayed status could change, given cur/next as returned by Schedule.Current/Next at
// `now`. ok=false means there is no known future transition (nothing scheduled) — the caller
// should fall back to a periodic check-in interval instead.
//
// A meeting shorter than a lead window resolves itself naturally: if the computed reveal instant
// has already passed by `now`, we skip straight to the harder deadline (the meeting's actual
// start/end) rather than returning a moment in the past.
func NextTransitionAt(cur, next *Event, now time.Time) (time.Time, bool) {
	switch {
	case cur != nil:
		// The early reveal only matters if there's an actual back-to-back meeting to preview —
		// with a real gap (or nothing) after, there's nothing to reveal early; just wait for the
		// room to genuinely free up at the meeting's end.
		if BackToBack(cur, next) != nil {
			revealAt := cur.End.Add(-BackToBackWindow)
			if now.Before(revealAt) {
				return revealAt.Add(TransitionBuffer), true
			}
		}
		return cur.End.Add(TransitionBuffer), true
	case next != nil:
		soonAt := next.Start.Add(-StartingSoonWindow)
		if now.Before(soonAt) {
			return soonAt.Add(TransitionBuffer), true
		}
		return next.Start.Add(TransitionBuffer), true
	default:
		return time.Time{}, false
	}
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
