package render

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

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
	cBlue   = color.RGBA{0x20, 0x30, 0xA0, 0xFF}
	cYellow = color.RGBA{0xE0, 0xC8, 0x10, 0xFF}
)

// Render composes the schedule as of `now` and returns the encoded payload.
// Render composes the schedule and returns the encoded MDPF payload for the device.
func (r *Renderer) Render(sched *calendar.Schedule, now time.Time, nextWakeS uint32) Payload {
	pal := r.Compose(sched, now)
	packed := Pack(pal)
	return Encode(packed, r.W, r.H, nextWakeS, true)
}

// Compose builds the room layout and quantizes it onto the 6-color palette, returning the
// paletted image. Exposed so the preview tool (cmd/preview) can render it to PNG without a device.
func (r *Renderer) Compose(sched *calendar.Schedule, now time.Time) *image.Paletted {
	canvas := image.NewRGBA(image.Rect(0, 0, r.W, r.H))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{cWhite}, image.Point{}, draw.Src)

	cur := sched.Current(now)
	next := sched.Next(now)

	// ── Status band ───────────────────────────────────────────────────────────
	bandH := 150
	var bandCol color.RGBA
	var status string
	switch {
	case cur != nil:
		bandCol, status = cRed, "IN USE"
	case next != nil && next.Start.Sub(now) <= 15*time.Minute:
		bandCol, status = cYellow, "STARTING SOON"
	default:
		bandCol, status = cGreen, "AVAILABLE"
	}
	fillRect(canvas, 0, 0, r.W, bandH, bandCol)

	bandText := cWhite
	if status == "STARTING SOON" { // yellow band needs dark text
		bandText = cBlack
	}
	drawScaled(canvas, 40, 38, sched.RoomName, 4, bandText)
	drawScaled(canvas, 40, 96, status, 5, bandText)

	// ── Body ────────────────────────────────────────────────────────────────
	y := bandH + 44
	switch {
	case cur != nil:
		subj := meetingTitle(cur)
		drawScaled(canvas, 40, y, subj, 4, cBlack)
		y += 64
		remaining := cur.End.Sub(now).Round(5 * time.Minute)
		drawScaled(canvas, 40, y, fmt.Sprintf("Ends %s  (%s left)",
			cur.End.In(now.Location()).Format("3:04 PM"), humanDur(remaining)), 3, cBlack)
		y += 56
	case next != nil:
		drawScaled(canvas, 40, y, "Free until", 4, cBlack)
		y += 56
		drawScaled(canvas, 40, y, next.Start.In(now.Location()).Format("3:04 PM"), 6, cGreen)
		y += 76
	default:
		drawScaled(canvas, 40, y, "Free for the rest of the day", 4, cBlack)
		y += 64
	}

	// ── Up next list ──────────────────────────────────────────────────────────
	listY := r.H - 150
	fillRect(canvas, 0, listY-14, r.W, 3, cBlue)
	drawScaled(canvas, 40, listY, "UP NEXT", 2, cBlue)
	listY += 30
	shown := 0
	for i := range sched.Events {
		e := &sched.Events[i]
		if !e.Start.After(now) { // skip current/past
			continue
		}
		line := fmt.Sprintf("%s - %s   %s",
			e.Start.In(now.Location()).Format("3:04"),
			e.End.In(now.Location()).Format("3:04 PM"),
			meetingTitle(e))
		drawScaled(canvas, 40, listY, truncate(line, 56), 2, cBlack)
		listY += 30
		if shown++; shown >= 3 {
			break
		}
	}
	if shown == 0 && cur == nil {
		drawScaled(canvas, 40, listY, "(no further meetings today)", 2, cBlack)
	}

	// ── Footer ──────────────────────────────────────────────────────────────
	// No current-time footer: baking `now` into the bitmap at minute resolution changes the ETag
	// on every broker render cycle (2 min), forcing a full Spectra 6 panel refresh on every device
	// wake even when the room schedule hasn't changed. 304s only fire when this bitmap is stable.

	// Quantize once, pack, encode. Full refresh is required on color e-paper anyway.
	return quantize(canvas, r.Dither)
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

func humanDur(d time.Duration) string {
	m := int(d.Minutes())
	if m >= 60 {
		return fmt.Sprintf("%dh %02dm", m/60, m%60)
	}
	return fmt.Sprintf("%dm", m)
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

// drawScaled renders text using the built-in 7x13 face, integer-upscaled by `scale`.
// Crisp blocky glyphs are ideal for e-paper (no antialiasing — we only have 6 inks).
// y is the glyph baseline-ish top; we offset by ascent internally.
func drawScaled(dst *image.RGBA, x, y int, s string, scale int, c color.RGBA) {
	if s == "" {
		return
	}
	face := basicfont.Face7x13
	// Render to a 1x scratch alpha mask first.
	w := font.MeasureString(face, s).Ceil()
	h := face.Metrics().Height.Ceil()
	if w <= 0 {
		return
	}
	scratch := image.NewRGBA(image.Rect(0, 0, w, h+2))
	d := font.Drawer{
		Dst:  scratch,
		Src:  &image.Uniform{color.RGBA{0, 0, 0, 0xFF}},
		Face: face,
		Dot:  fixed.P(0, face.Metrics().Ascent.Ceil()),
	}
	d.DrawString(s)

	// Nearest-neighbor upscale, blitting only inked pixels in color c.
	for sy := 0; sy < scratch.Bounds().Dy(); sy++ {
		for sx := 0; sx < w; sx++ {
			_, _, _, a := scratch.At(sx, sy).RGBA()
			if a < 0x8000 {
				continue
			}
			fillRect(dst, x+sx*scale, y+sy*scale, scale, scale, c)
		}
	}
}
