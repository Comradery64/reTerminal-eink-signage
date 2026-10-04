package config

import (
	"testing"
	"time"
)

func TestIsBusinessHoursExcludesWeekends(t *testing.T) {
	c := &Config{}
	c.Wake.Timezone = "America/Los_Angeles"
	c.Wake.BusinessStartHour, c.Wake.BusinessEndHour = 9, 18
	loc, _ := time.LoadLocation("America/Los_Angeles")
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"Fri noon", time.Date(2026, 10, 2, 12, 0, 0, 0, loc), true},
		{"Fri 8am", time.Date(2026, 10, 2, 8, 0, 0, 0, loc), false},
		{"Fri 6pm", time.Date(2026, 10, 2, 18, 0, 0, 0, loc), false},
		{"Sat noon", time.Date(2026, 10, 3, 12, 0, 0, 0, loc), false},
		{"Sun noon", time.Date(2026, 10, 4, 12, 0, 0, 0, loc), false},
		{"Mon 9am", time.Date(2026, 10, 5, 9, 0, 0, 0, loc), true},
	}
	for _, tc := range cases {
		if got := c.isBusinessHours(tc.at); got != tc.want {
			t.Errorf("%s: isBusinessHours = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSecondsUntilNextBoundarySkipsNearlyElapsedBoundary(t *testing.T) {
	// Boundaries sit at gridOffsetSeconds past each interval; 1s before 14:05 UTC.
	at := time.Date(2026, 10, 4, 14, 4, 59, 0, time.UTC)
	if got := secondsUntilNextBoundary(at, 3600); got != 3601 {
		t.Errorf("1s before an hourly boundary: got %ds, want 3601s (next boundary, not 1s)", got)
	}
	if got := secondsUntilNextBoundary(at, 600); got != 601 {
		t.Errorf("1s before a 10-min boundary: got %ds, want 601s", got)
	}
	// Exactly MinWakeSeconds out is a real wait, not an early wake.
	at = time.Date(2026, 10, 4, 14, 4, 0, 0, time.UTC)
	if got := secondsUntilNextBoundary(at, 600); got != 60 {
		t.Errorf("60s before a boundary: got %ds, want 60s", got)
	}
}

func TestClampWakeSecondsMatchesFirmware(t *testing.T) {
	for in, want := range map[uint32]uint32{1: 60, 60: 60, 3600: 3600, 21600: 21600, 68098: 21600} {
		if got := ClampWakeSeconds(in); got != want {
			t.Errorf("ClampWakeSeconds(%d) = %d, want %d", in, got, want)
		}
	}
}
