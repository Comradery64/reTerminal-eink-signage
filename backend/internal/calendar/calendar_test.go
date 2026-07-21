package calendar

import (
	"testing"
	"time"
)

func mkEvent(startOffset, durMin int, base time.Time) Event {
	s := base.Add(time.Duration(startOffset) * time.Minute)
	return Event{Subject: "meeting", Start: s, End: s.Add(time.Duration(durMin) * time.Minute)}
}

func TestNextTransitionAt(t *testing.T) {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	t.Run("nothing scheduled", func(t *testing.T) {
		_, ok := NextTransitionAt(nil, nil, base)
		if ok {
			t.Fatal("want ok=false when nothing is scheduled")
		}
	})

	t.Run("available, next meeting far away — wake at starting-soon reveal", func(t *testing.T) {
		next := mkEvent(60, 30, base) // starts in 1h
		got, ok := NextTransitionAt(nil, &next, base)
		if !ok {
			t.Fatal("want ok=true")
		}
		want := base.Add(50 * time.Minute).Add(TransitionBuffer) // 60min - 10min lead
		if !got.Equal(want) {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("already within starting-soon window — wake at meeting start", func(t *testing.T) {
		next := mkEvent(4, 30, base) // starts in 4 min, already < 10min lead
		got, ok := NextTransitionAt(nil, &next, base)
		if !ok {
			t.Fatal("want ok=true")
		}
		want := base.Add(4 * time.Minute).Add(TransitionBuffer)
		if !got.Equal(want) {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("in meeting, real gap after — wake at meeting end", func(t *testing.T) {
		cur := mkEvent(-10, 60, base) // started 10min ago, 60min long -> ends in 50min
		got, ok := NextTransitionAt(&cur, nil, base)
		if !ok {
			t.Fatal("want ok=true")
		}
		want := cur.End.Add(TransitionBuffer)
		if !got.Equal(want) {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("in meeting, back-to-back follows — wake at reveal point (end - 5min)", func(t *testing.T) {
		cur := mkEvent(-10, 60, base) // ends in 50min
		next := mkEvent(40, 30, base) // starts at cur.End, i.e. back-to-back
		got, ok := NextTransitionAt(&cur, &next, base)
		if !ok {
			t.Fatal("want ok=true")
		}
		want := cur.End.Add(-BackToBackWindow).Add(TransitionBuffer)
		if !got.Equal(want) {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("in meeting, next event is a real gap away (not back-to-back) — wake at meeting end, no early reveal", func(t *testing.T) {
		cur := mkEvent(-10, 60, base) // ends in 50min
		next := mkEvent(120, 30, base) // starts much later, real gap — not back-to-back
		got, ok := NextTransitionAt(&cur, &next, base)
		if !ok {
			t.Fatal("want ok=true")
		}
		want := cur.End.Add(TransitionBuffer)
		if !got.Equal(want) {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("short meeting shorter than back-to-back window — skip straight to end", func(t *testing.T) {
		cur := mkEvent(-2, 3, base) // started 2min ago, 3min long -> ends in 1min; reveal point (end-5min) already passed
		got, ok := NextTransitionAt(&cur, nil, base)
		if !ok {
			t.Fatal("want ok=true")
		}
		want := cur.End.Add(TransitionBuffer)
		if !got.Equal(want) {
			t.Fatalf("got %s, want %s", got, want)
		}
	})
}

func TestBackToBack(t *testing.T) {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	cur := mkEvent(-10, 60, base) // ends in 50min

	t.Run("nil cur or next", func(t *testing.T) {
		if BackToBack(nil, nil) != nil {
			t.Fatal("want nil")
		}
	})

	t.Run("exactly back to back", func(t *testing.T) {
		next := mkEvent(40, 30, base) // starts exactly at cur.End
		if got := BackToBack(&cur, &next); got == nil {
			t.Fatal("want non-nil")
		}
	})

	t.Run("within window but not exact", func(t *testing.T) {
		next := mkEvent(43, 30, base) // starts 3min after cur.End
		if got := BackToBack(&cur, &next); got == nil {
			t.Fatal("want non-nil (within 5min window)")
		}
	})

	t.Run("real gap, outside window", func(t *testing.T) {
		next := mkEvent(60, 30, base) // starts 20min after cur.End
		if got := BackToBack(&cur, &next); got != nil {
			t.Fatal("want nil (gap exceeds window)")
		}
	})
}
