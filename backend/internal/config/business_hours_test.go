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
