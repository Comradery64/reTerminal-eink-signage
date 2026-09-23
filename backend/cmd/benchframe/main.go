// Command benchframe renders one sample room layout and writes the raw MDPF wire bytes (header +
// body, exactly what the broker would send over HTTP) to a file — for flashing directly into a
// firmware bench build that displays a static embedded frame with no WiFi/broker/provisioning at
// all. See firmware's CONFIG_MD_BENCH_STATIC_FRAME.
//
//	go run ./cmd/benchframe -out frame.mdpf
//	MD_ATLAS_DIR=/path/to/candidate/atlas go run ./cmd/benchframe -out frame.mdpf
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/render"
)

func main() {
	out := flag.String("out", "frame.mdpf", "output path for the raw MDPF payload")
	dither := flag.Bool("dither", true, "Floyd–Steinberg dithering onto the 6-color palette")
	flag.Parse()

	loc, _ := time.LoadLocation("America/New_York")
	r := render.New(800, 480, *dither)

	now := time.Date(2026, 6, 25, 10, 0, 0, 0, loc)
	sched := &calendar.Schedule{RoomName: "Aspen", FetchedAt: now, Events: []calendar.Event{
		{Subject: "Design review", Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)},
		{Subject: "1:1 — Priya / Sam", Start: now.Add(4 * time.Hour), End: now.Add(4*time.Hour + 30*time.Minute), Private: true},
	}}

	payload := r.Render(sched, now)

	if err := os.WriteFile(*out, payload.Bytes, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s: %d bytes (crc32=%08x)\n", *out, len(payload.Bytes), payload.CRC32)
}
