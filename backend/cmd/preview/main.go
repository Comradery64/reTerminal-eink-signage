// Command preview renders sample room layouts to PNG so you can see exactly what a display will
// show — no hardware, no calendar, no broker required.
//
//	go run ./cmd/preview            # writes preview-available.png, preview-inuse.png, preview-soon.png
//	go run ./cmd/preview -dither=false
package main

import (
	"flag"
	"fmt"
	"image/png"
	"os"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/render"
)

func main() {
	dither := flag.Bool("dither", true, "Floyd–Steinberg dithering onto the 6-color palette")
	flag.Parse()

	loc, _ := time.LoadLocation("America/New_York")
	r := render.New(800, 480, *dither)

	// Real calendars overwhelmingly book on the hour or half hour — using arbitrary minute
	// offsets here (e.g. 9:52, 10:27) made every scenario look like a rendering bug rather than a
	// realistic schedule. All start times below land on the hour.
	at := func(h, m int, dayOffset int) time.Time {
		base := time.Date(2026, 6, 25, h, m, 0, 0, loc)
		return base.AddDate(0, 0, dayOffset)
	}
	ev := func(subj string, start, end time.Time, private bool) calendar.Event {
		return calendar.Event{Subject: subj, Start: start, End: end, Private: private}
	}

	scenarios := []struct {
		file  string
		now   time.Time
		sched *calendar.Schedule
	}{
		{"preview-available.png", at(10, 0, 0), &calendar.Schedule{RoomName: "Aspen", FetchedAt: at(10, 0, 0), Events: []calendar.Event{
			ev("Design review", at(12, 0, 0), at(13, 0, 0), false),
			ev("1:1 — Priya / Sam", at(14, 0, 0), at(14, 30, 0), true),
		}}},
		{"preview-inuse.png", at(10, 0, 0), &calendar.Schedule{RoomName: "Birch", FetchedAt: at(10, 0, 0), Events: []calendar.Event{
			ev("Quarterly planning", at(9, 0, 0), at(11, 0, 0), false),
			ev("Vendor demo", at(11, 0, 0), at(11, 30, 0), false),
			ev("Eng sync", at(13, 0, 0), at(14, 0, 0), false),
		}}},
		// now is 10 minutes before Standup — right at the edge of calendar.StartingSoonWindow —
		// so the panel shows yellow "STARTING SOON" instead of green "AVAILABLE".
		{"preview-soon.png", at(9, 50, 0), &calendar.Schedule{RoomName: "Cedar", FetchedAt: at(9, 50, 0), Events: []calendar.Event{
			ev("Standup", at(10, 0, 0), at(10, 30, 0), false),
			ev("Customer call", at(11, 0, 0), at(12, 0, 0), false),
		}}},
		// Nothing left today; the only upcoming event is tomorrow morning (poll window spans
		// today+tomorrow). Regression check for the day-abbreviation fix — without it, this looked
		// like a same-day meeting on a day with nothing actually scheduled.
		{"preview-nextday.png", at(18, 0, 0), &calendar.Schedule{RoomName: "Dogwood", FetchedAt: at(18, 0, 0), Events: []calendar.Event{
			ev("Team offsite prep", at(7, 0, 1), at(8, 0, 1), false),
		}}},
		// now is 1 minute before Sprint retro ends, with Roadmap sync starting the moment it ends
		// (inside calendar.BackToBackWindow) — regression check that the up-next list (not an
		// inline "Next: ..." line) is what surfaces the following meeting.
		{"preview-backtoback.png", at(9, 59, 0), &calendar.Schedule{RoomName: "Elm", FetchedAt: at(9, 59, 0), Events: []calendar.Event{
			ev("Sprint retro", at(9, 0, 0), at(10, 0, 0), false),
			ev("Roadmap sync", at(10, 0, 0), at(10, 45, 0), false),
		}}},
	}

	for _, sc := range scenarios {
		img := r.Compose(sc.sched, sc.now)
		f, err := os.Create(sc.file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			os.Exit(1)
		}
		if err := png.Encode(f, img); err != nil {
			fmt.Fprintln(os.Stderr, "encode:", err)
			os.Exit(1)
		}
		f.Close()
		fmt.Printf("wrote %s\n", sc.file)
	}
}
