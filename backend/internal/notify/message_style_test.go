package notify

import (
	"strings"
	"testing"
	"time"
)

// Each style's wording for each kind of alert, exactly. "plain" is the default; "detailed" is the
// original wording kept for anyone who wants it.
func TestMessageStyleWording(t *testing.T) {
	two := 2 * time.Hour
	type txt struct{ title, text string }
	for _, c := range []struct {
		style                      string
		battery, offline, recovery txt
	}{
		{"plain",
			txt{"🔋 Low battery: Aspen (45%)", "Please recharge or replace it soon."},
			txt{"📵 Display offline: Aspen", "It hasn't checked in for about 2 hours. Check its battery and Wi-Fi."},
			txt{"✅ Display back online: Aspen", ""}},
		{"minimal",
			txt{"🔋 Aspen: battery 45%", ""},
			txt{"📵 Aspen: offline", ""},
			txt{"✅ Aspen: back online", ""}},
		{"detailed",
			txt{"🔋 Low battery: Aspen (45%)", "Display \"Aspen\" (device rt-1) is at 45% (3690 mV). Please recharge/replace soon.\nNote: LiPo %→V is approximate near this range; 3690 mV is the raw reading."},
			txt{"📵 Display offline: Aspen", "Display \"Aspen\" (device rt-1) hasn't checked in for 2h0m0s — two of its scheduled check-ins. Check its battery and Wi-Fi."},
			txt{"✅ Display back online: Aspen", "Display \"Aspen\" (device rt-1) is checking in again."}},
	} {
		bt, bx := batteryText(c.style, "Aspen", "rt-1", 45, 3690)
		ot, ox := offlineText(c.style, "Aspen", "rt-1", two)
		rt, rx := recoveredText(c.style, "Aspen", "rt-1")
		for name, got := range map[string][2]string{"battery": {bt, bx}, "offline": {ot, ox}, "recovered": {rt, rx}} {
			want := map[string]txt{"battery": c.battery, "offline": c.offline, "recovered": c.recovery}[name]
			if got[0] != want.title || got[1] != want.text {
				t.Errorf("%s/%s:\n got  %q / %q\n want %q / %q", c.style, name, got[0], got[1], want.title, want.text)
			}
		}
	}
}

// Plain and minimal never show the technical details a regular user can't act on.
func TestPlainAndMinimalHideTechnicalDetail(t *testing.T) {
	for _, style := range []string{"", "plain", "minimal", "no-such-style"} {
		for _, parts := range [][2]string{
			func() [2]string { a, b := batteryText(style, "Aspen", "rt-1", 45, 3690); return [2]string{a, b} }(),
			func() [2]string { a, b := offlineText(style, "Aspen", "rt-1", time.Hour); return [2]string{a, b} }(),
			func() [2]string { a, b := recoveredText(style, "Aspen", "rt-1"); return [2]string{a, b} }(),
		} {
			all := parts[0] + parts[1]
			for _, tech := range []string{"rt-1", "mV", "LiPo", "device "} {
				if strings.Contains(all, tech) {
					t.Errorf("style %q leaks %q: %q", style, tech, all)
				}
			}
		}
	}
}

// Unset or unknown means plain, so a typo can't change or silence alerts.
func TestNormalizeStyle(t *testing.T) {
	for in, want := range map[string]string{"": "plain", "plain": "plain", "minimal": "minimal", "detailed": "detailed", "MINIMAL": "plain", "x": "plain"} {
		if got := NormalizeStyle(in); got != want {
			t.Errorf("NormalizeStyle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlainDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second:             "1 minute",
		time.Minute:                  "1 minute",
		40 * time.Minute:             "40 minutes",
		time.Hour:                    "about 1 hour",
		3*time.Hour + 20*time.Minute: "about 3 hours",
		30 * time.Hour:               "about 30 hours",
		72 * time.Hour:               "about 3 days",
	} {
		if got := plainDuration(d); got != want {
			t.Errorf("plainDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// A message with no body is just its title: no stray blank line on any channel.
func TestMessageBodyHasNoTrailingNewline(t *testing.T) {
	if got := (Message{Title: "t"}).Body(); got != "t" {
		t.Errorf("Body() = %q, want just the title", got)
	}
	if got := (Message{Title: "t", Text: "b"}).Body(); got != "t\nb" {
		t.Errorf("Body() = %q", got)
	}
}

// The manager reads its thresholds and style live: a change takes effect on the next evaluation,
// and a zero value keeps what the manager was built with.
func TestManagerReadsSettingsLive(t *testing.T) {
	m := newTestManager() // built with low=45
	cur := Settings{LowPct: 45, ClearPct: 55, Renotify: 24 * time.Hour}
	m.SetSettings(func() Settings { return cur })
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

	if m.EvaluateBattery("rt-1", "Aspen", 55, 3700, now) {
		t.Fatal("55% is above a 45% threshold")
	}
	cur.LowPct, cur.ClearPct = 60, 70 // edited in /admin
	if !m.EvaluateBattery("rt-2", "Birch", 55, 3700, now) {
		t.Fatal("the new 60% threshold was not applied")
	}
	cur = Settings{} // nothing set: fall back to what the manager was built with
	if m.EvaluateBattery("rt-3", "Cedar", 50, 3600, now) {
		t.Fatal("zero settings must keep the built-in 45% threshold, not alert at 50%")
	}
	if !m.EvaluateBattery("rt-3", "Cedar", 40, 3500, now) {
		t.Fatal("the built-in threshold must still alert at 40%")
	}
}
