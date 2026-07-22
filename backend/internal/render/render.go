package render

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/calendar"
)

// Renderer composes a room schedule into a Spectra 6 MDPF payload.
type Renderer struct {
	W, H   int
	Dither bool
}

func New(w, h int, dither bool) *Renderer { return &Renderer{W: w, H: h, Dither: dither} }

// sRGB anchors matching the palette, used while composing the RGBA canvas.
var (
	cWhite  = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	cBlack  = color.RGBA{0x00, 0x00, 0x00, 0xFF}
	cRed    = color.RGBA{0xC0, 0x20, 0x20, 0xFF}
	cGreen  = color.RGBA{0x20, 0x80, 0x40, 0xFF}
	cYellow = color.RGBA{0xE0, 0xC8, 0x10, 0xFF}
)

// Layout constants for the vertical-split room-plaque design: a full-height color status panel
// on the left, room name + schedule detail on the right.
const (
	statusPanelW = 280
	margin       = 44
)

// Render composes the schedule as of `now` and returns the encoded payload. The embedded
// next-wake field is always 0 — the firmware only honors it as an override when nonzero (see
// net_client.cpp), and baking in a real value here would be a snapshot from whenever the poller
// last rendered this room (up to PollInterval stale), not from the device's actual request time.
// The authoritative value is computed fresh per-request in the HTTP handler instead.
func (r *Renderer) Render(sched *calendar.Schedule, now time.Time) Payload {
	pal := r.Compose(sched, now)
	packed := Pack(pal)
	return Encode(packed, r.W, r.H, 0, true)
}

// Compose builds the room layout and quantizes it onto the 6-color palette, returning the
// paletted image. Exposed so the preview tool (cmd/preview) can render it to PNG without a device.
func (r *Renderer) Compose(sched *calendar.Schedule, now time.Time) *image.Paletted {
	canvas := image.NewRGBA(image.Rect(0, 0, r.W, r.H))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{cWhite}, image.Point{}, draw.Src)

	cur := sched.Current(now)
	next := sched.Next(now)

	r.drawStatusPanel(canvas, cur, next, now)
	r.drawInfoPanel(canvas, sched, cur, next, now)

	// No current-time footer anywhere: baking `now` into the bitmap at minute resolution changes
	// the ETag on every broker render cycle (2 min), forcing a full Spectra 6 panel refresh on
	// every device wake even when the room schedule hasn't changed. 304s only fire when the
	// bitmap is stable across wakes with no schedule change.

	// Quantize once, pack, encode. Full refresh is required on color e-paper anyway.
	return quantize(canvas, r.Dither)
}

// drawStatusPanel fills the left-hand color panel and centers the status headline in it.
func (r *Renderer) drawStatusPanel(canvas *image.RGBA, cur, next *calendar.Event, now time.Time) {
	var bandCol color.RGBA
	var lines []string
	var detail string
	switch calendar.RoomStatus(cur, next, now) {
	case "in_meeting":
		bandCol, lines = cRed, []string{"IN", "MEETING"}
	case "starting_soon":
		// Static clock time, not a live "in Xm" countdown — a relative countdown recomputes (and
		// so changes the ETag, forcing a refresh) on every single poll regardless of whether
		// anything on the calendar actually changed, defeating the point of calendar-driven wake
		// scheduling (internal/config's smart mode): content should only change at real
		// transitions.
		bandCol, lines = cYellow, []string{"STARTING", "SOON"}
		detail = fmt.Sprintf("at %s", next.Start.In(now.Location()).Format("3:04 PM"))
	default:
		bandCol, lines = cGreen, []string{"AVAILABLE"}
	}
	fillRect(canvas, 0, 0, statusPanelW, r.H, bandCol)

	textCol := cWhite
	if bandCol == cYellow { // yellow needs dark text for contrast
		textCol = cBlack
	}

	const panelPadding = 24
	big := fitBitmapFont(lines, statusPanelW-2*panelPadding)
	small := regular(29)
	lineH := big.lineHeight() + 12

	total := lineH * len(lines)
	if detail != "" {
		total += 34
	}
	y := r.H/2 - total/2

	// The loop below places each line's nominal ascent+descent box starting at y, but the box is
	// generally taller than the actual glyph ink (e.g. an all-caps label has no descenders), so
	// centering on the nominal box visibly off-centers the ink itself within the panel. Measure
	// where the ink from the first and last visible lines would actually land at this y, and shift
	// y so the true ink extent — not the nominal box — is centered on the panel.
	firstTop, _ := big.inkExtent(lines[0])
	lastTop := y + lineH*(len(lines)-1)
	_, lastBottom := big.inkExtent(lines[len(lines)-1])
	lastBottom += lastTop
	if detail != "" {
		detailY := y + lineH*len(lines) - 6
		_, detailBottom := small.inkExtent(detail)
		lastBottom = detailY + detailBottom
	}
	inkCenter := (y + firstTop + lastBottom) / 2
	y += r.H/2 - inkCenter

	for _, ln := range lines {
		w := big.measure(ln)
		drawBM(canvas, big, (statusPanelW-w)/2, y, ln, textCol)
		y += lineH
	}
	if detail != "" {
		w := small.measure(detail)
		drawBM(canvas, small, (statusPanelW-w)/2, y-6, detail, textCol)
	}
}

// drawInfoPanel fills the right-hand white panel: room name, schedule detail, and a compact
// up-next line.
func (r *Renderer) drawInfoPanel(canvas *image.RGBA, sched *calendar.Schedule, cur, next *calendar.Event, now time.Time) {
	x0 := statusPanelW + margin
	maxW := r.W - x0 - margin

	nameFont := bold(53)
	y := 48
	for _, ln := range wrapTextBM(nameFont, sched.RoomName, maxW) {
		drawBM(canvas, nameFont, x0, y, ln, cBlack)
		y += 50
	}
	y += 30 // wider gap between the name group and the schedule group than within either group

	labelFont := regular(24)
	subjFont := bold(35)
	timeFont := regular(32)
	const labelTracking = 2 // px of extra space between letters, for a signage-style label

	switch {
	case cur != nil:
		drawBMTracked(canvas, labelFont, x0, y, "CURRENT MEETING", labelTracking, cBlack)
		y += 34
		drawBM(canvas, subjFont, x0, y, truncate(meetingTitle(cur), 26), cBlack)
		y += 40
		drawBM(canvas, timeFont, x0, y, fmt.Sprintf("%s - %s",
			cur.Start.In(now.Location()).Format("3:04 PM"), cur.End.In(now.Location()).Format("3:04 PM")), cBlack)
		y += 34
		// Back-to-back preview: only reveal it in the current meeting's last
		// calendar.BackToBackWindow, matching when the device actually wakes to check for it
		// (calendar.NextTransitionAt) — showing it the whole meeting would be premature info and
		// would also destabilize the ETag far earlier than necessary.
		if b2b := calendar.BackToBack(cur, next); b2b != nil && cur.End.Sub(now) <= calendar.BackToBackWindow {
			drawBM(canvas, labelFont, x0, y, fmt.Sprintf("Next: %s at %s",
				truncate(meetingTitle(b2b), 20), b2b.Start.In(now.Location()).Format("3:04 PM")), cBlack)
			y += 40
		}
	case next != nil:
		drawBMTracked(canvas, labelFont, x0, y, "NEXT MEETING", labelTracking, cBlack)
		y += 34
		drawBM(canvas, subjFont, x0, y, truncate(meetingTitle(next), 26), cBlack)
		y += 40
		// The poll window spans today+tomorrow (for cross-midnight "up next"), so the next
		// meeting can be tomorrow's — a bare time here would look like a same-day meeting on a
		// day with nothing left scheduled. The day abbreviation removes that ambiguity.
		drawBM(canvas, timeFont, x0, y, fmt.Sprintf("%s  %s - %s", dayAbbrev(next.Start, now),
			next.Start.In(now.Location()).Format("3:04 PM"), next.End.In(now.Location()).Format("3:04 PM")), cBlack)
		y += 40
	default:
		drawBM(canvas, subjFont, x0, y, "Free for the rest of the day", cBlack)
		y += 40
	}

	// Up-next: compact list of further events beyond the one already shown above.
	skip := cur
	if skip == nil {
		skip = next
	}
	upNextFont := regular(24)
	y = r.H - 100
	fillRect(canvas, x0, y-16, maxW, 2, cBlack)
	shown := 0
	for i := range sched.Events {
		e := &sched.Events[i]
		if !e.Start.After(now) || e == skip {
			continue
		}
		line := fmt.Sprintf("%s %s - %s   %s",
			dayAbbrev(e.Start, now),
			e.Start.In(now.Location()).Format("3:04"),
			e.End.In(now.Location()).Format("3:04 PM"),
			meetingTitle(e))
		drawBM(canvas, upNextFont, x0, y, truncate(line, 40), cBlack)
		y += 26
		if shown++; shown >= 2 {
			break
		}
	}
}

// dayAbbrev returns the 3-letter weekday (e.g. "MON") for t in now's location.
func dayAbbrev(t, now time.Time) string {
	return strings.ToUpper(t.In(now.Location()).Format("Mon"))
}

func meetingTitle(e *calendar.Event) string {
	if e.Private {
		return "Private meeting"
	}
	if e.Subject == "" {
		// Free/busy data carries no title — the room is simply occupied.
		return "Busy"
	}
	return e.Subject
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func fillRect(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	draw.Draw(img, image.Rect(x, y, x+w, y+h), &image.Uniform{c}, image.Point{}, draw.Src)
}
