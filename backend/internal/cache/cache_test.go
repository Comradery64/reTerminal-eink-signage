package cache

import (
	"testing"
	"time"
)

func TestShouldForceFullRefresh(t *testing.T) {
	loc := time.UTC
	day1 := time.Date(2026, 7, 11, 3, 0, 0, 0, loc)

	s := New()
	if s.ShouldForceFullRefresh("rt-1", day1, 4, loc) {
		t.Fatal("wrong hour must not force a refresh")
	}
	if !s.ShouldForceFullRefresh("rt-1", day1, 3, loc) {
		t.Fatal("first call in the forced hour must force a refresh")
	}
	if s.ShouldForceFullRefresh("rt-1", day1, 3, loc) {
		t.Fatal("second call same day must not force again")
	}
	// A different device is tracked independently.
	if !s.ShouldForceFullRefresh("rt-2", day1, 3, loc) {
		t.Fatal("a different device must still get its own forced refresh")
	}

	day2 := day1.AddDate(0, 0, 1)
	if !s.ShouldForceFullRefresh("rt-1", day2, 3, loc) {
		t.Fatal("the next calendar day must force again")
	}
}
