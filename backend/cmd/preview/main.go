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
	now := time.Date(2026, 6, 25, 10, 19, 0, 0, loc)
	r := render.New(800, 480, *dither)

	ev := func(subj string, startMin, durMin int, private bool) calendar.Event {
		s := now.Add(time.Duration(startMin) * time.Minute)
		return calendar.Event{Subject: subj, Start: s, End: s.Add(time.Duration(durMin) * time.Minute), Private: private}
	}

	scenarios := []struct {
		file   string
		sched  *calendar.Schedule
	}{
		{"preview-available.png", &calendar.Schedule{RoomName: "Aspen", FetchedAt: now, Events: []calendar.Event{
			ev("Design review", 95, 45, false),
			ev("1:1 — Priya / Sam", 200, 30, true),
		}}},
		{"preview-inuse.png", &calendar.Schedule{RoomName: "Birch", FetchedAt: now, Events: []calendar.Event{
			ev("Quarterly planning", -25, 60, false),
			ev("Vendor demo", 50, 30, false),
			ev("Eng sync", 140, 30, false),
		}}},
		{"preview-soon.png", &calendar.Schedule{RoomName: "Cedar", FetchedAt: now, Events: []calendar.Event{
			ev("Standup", 8, 15, false),
			ev("Customer call", 60, 45, false),
		}}},
	}

	for _, sc := range scenarios {
		img := r.Compose(sc.sched, now)
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
