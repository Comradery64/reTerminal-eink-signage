package render

import (
	"embed"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"strings"
	"sync"
)

// Text used to render via a live TTF rasterizer (golang.org/x/image/font/opentype), which does
// not execute a font's embedded TrueType hinting instructions. Un-hinted at small-to-medium sizes,
// that produced visibly uneven stem widths (e.g. the two legs of "M" landing on different
// sub-pixel offsets) and jagged flat tops on letters like "I" — see tools/bakefont/bake_font.py's
// doc comment for the full story. The fix: every glyph we need is now pre-rasterized once, offline,
// through a real hinting-aware rasterizer (FreeType via Pillow) and baked into a packed 1-bit
// bitmap atlas. At render time we only blit pre-verified pixels — no vector rasterization, no
// threshold-induced inconsistency, and (as a bonus) no concurrency hazard from sharing rasterizer
// state across the poller's per-room goroutines, since a parsed atlas is immutable data.
//
//go:embed fonts/atlas/*.fnt
var atlasFS embed.FS

// boldSizes/regularSizes must match the pixel sizes baked by tools/bakefont/bake_font.py into
// internal/render/fonts/atlas/ — every size any call site asks for must be loaded here.
// shrinkLadder is boldSizes' subset (ordered descending) used by fitBitmapFont for the status
// headline; it excludes 35, which is only ever used at a fixed size (the meeting subject line).
var (
	boldSizes    = []int{53, 45, 37, 35, 32, 27}
	regularSizes = []int{24, 29, 32}
	shrinkLadder = []int{53, 45, 37, 32, 27}
)

var (
	atlasOnce sync.Once
	boldFonts = map[int]*bitmapFont{}
	regFonts  = map[int]*bitmapFont{}
)

func loadAtlases() {
	load := func(m map[int]*bitmapFont, prefix string, sizes []int) {
		for _, sz := range sizes {
			data, err := atlasFS.ReadFile(fmt.Sprintf("fonts/atlas/%s_%d.fnt", prefix, sz))
			if err != nil {
				panic("render: read atlas: " + err.Error())
			}
			f, err := parseBitmapFont(data)
			if err != nil {
				panic("render: parse atlas: " + err.Error())
			}
			m[sz] = f
		}
	}
	load(boldFonts, "bold", boldSizes)
	load(regFonts, "regular", regularSizes)
}

// bold returns the pre-baked bold atlas at the given pixel size. Panics if sz isn't in boldSizes —
// a programming error (all call sites use the boldSizes/regularSizes constants), not a runtime
// condition to recover from.
func bold(sz int) *bitmapFont {
	atlasOnce.Do(loadAtlases)
	f, ok := boldFonts[sz]
	if !ok {
		panic(fmt.Sprintf("render: no baked bold atlas at size %d", sz))
	}
	return f
}

func regular(sz int) *bitmapFont {
	atlasOnce.Do(loadAtlases)
	f, ok := regFonts[sz]
	if !ok {
		panic(fmt.Sprintf("render: no baked regular atlas at size %d", sz))
	}
	return f
}

const (
	firstCP = 32
	lastCP  = 126
	numCP   = lastCP - firstCP + 1
)

type bmGlyph struct {
	w, h, rowBytes   int
	offsetX, offsetY int
	advance          int
	bitmap           []byte
}

type bitmapFont struct {
	ascent, descent int
	glyphs          [numCP]bmGlyph
}

func (f *bitmapFont) glyph(r rune) (bmGlyph, bool) {
	if r < firstCP || r > lastCP {
		return bmGlyph{}, false
	}
	return f.glyphs[r-firstCP], true
}

func (f *bitmapFont) lineHeight() int { return f.ascent + f.descent }

// inkExtent returns the actual top/bottom of s's drawn pixels, relative to the same "y" origin
// drawBM takes (i.e. top = min glyph top, bottom = max glyph bottom, both already folded through
// f.ascent+g.offsetY the way drawBM positions rows). Unlike lineHeight(), which is the font's
// nominal ascent+descent box, this reflects where the ink itself lands — glyph atlases baked from
// real font metrics commonly have ascent/descent boxes taller than a given string's actual ink
// (e.g. no descenders in an all-caps label), so centering on lineHeight() alone visibly off-centers
// the ink within its box.
func (f *bitmapFont) inkExtent(s string) (top, bottom int) {
	top, bottom = 1<<30, -(1 << 30)
	for _, r := range s {
		g, ok := f.glyph(r)
		if !ok || g.h == 0 {
			continue
		}
		gt, gb := f.ascent+g.offsetY, f.ascent+g.offsetY+g.h
		if gt < top {
			top = gt
		}
		if gb > bottom {
			bottom = gb
		}
	}
	if top > bottom {
		return 0, 0
	}
	return top, bottom
}

func (f *bitmapFont) measure(s string) int {
	w := 0
	for _, r := range s {
		if g, ok := f.glyph(r); ok {
			w += g.advance
		}
	}
	return w
}

// parseBitmapFont decodes the format written by tools/bakefont/bake_font.py.
func parseBitmapFont(data []byte) (*bitmapFont, error) {
	if len(data) < 10 || string(data[0:4]) != "BFA1" {
		return nil, fmt.Errorf("bad magic")
	}
	ascent := int(int16(binary.BigEndian.Uint16(data[4:6])))
	descent := int(int16(binary.BigEndian.Uint16(data[6:8])))
	n := int(binary.BigEndian.Uint16(data[8:10]))
	if n != numCP {
		return nil, fmt.Errorf("expected %d glyphs, got %d", numCP, n)
	}

	f := &bitmapFont{ascent: ascent, descent: descent}
	off := 10
	for i := 0; i < n; i++ {
		if off+10 > len(data) {
			return nil, fmt.Errorf("truncated glyph header at index %d", i)
		}
		w := int(binary.BigEndian.Uint16(data[off : off+2]))
		h := int(binary.BigEndian.Uint16(data[off+2 : off+4]))
		ox := int(int16(binary.BigEndian.Uint16(data[off+4 : off+6])))
		oy := int(int16(binary.BigEndian.Uint16(data[off+6 : off+8])))
		adv := int(binary.BigEndian.Uint16(data[off+8 : off+10]))
		off += 10

		rowBytes := (w + 7) / 8
		nbytes := rowBytes * h
		if off+nbytes > len(data) {
			return nil, fmt.Errorf("truncated bitmap at index %d", i)
		}
		f.glyphs[i] = bmGlyph{
			w: w, h: h, rowBytes: rowBytes,
			offsetX: ox, offsetY: oy, advance: adv,
			bitmap: data[off : off+nbytes],
		}
		off += nbytes
	}
	return f, nil
}

// drawBM blits s starting with its left edge at x, baseline at y+f.ascent. Returns the total
// advance (pen distance moved), which callers use as the drawn width.
func drawBM(dst *image.RGBA, f *bitmapFont, x, y int, s string, c color.RGBA) int {
	baseline := y + f.ascent
	penX := x
	for _, r := range s {
		g, ok := f.glyph(r)
		if !ok {
			continue
		}
		gx, gy := penX+g.offsetX, baseline+g.offsetY
		for yy := 0; yy < g.h; yy++ {
			row := g.bitmap[yy*g.rowBytes : (yy+1)*g.rowBytes]
			for xx := 0; xx < g.w; xx++ {
				if row[xx/8]&(0x80>>uint(xx%8)) == 0 {
					continue
				}
				dst.SetRGBA(gx+xx, gy+yy, c)
			}
		}
		penX += g.advance
	}
	return penX - x
}

// drawBMTracked draws s one rune at a time with trackingPx of extra space inserted between
// glyphs — a hand-rolled letter-spacing for uppercase signage-style labels.
func drawBMTracked(dst *image.RGBA, f *bitmapFont, x, y int, s string, trackingPx int, c color.RGBA) int {
	penX := x
	for _, r := range s {
		penX += drawBM(dst, f, penX, y, string(r), c) + trackingPx
	}
	return penX - x
}

// fitBitmapFont returns the largest pre-baked bold atlas whose widest line fits within maxW —
// single long status words (e.g. "AVAILABLE") can't wrap, so they must shrink instead of clipping.
func fitBitmapFont(lines []string, maxW int) *bitmapFont {
	for _, sz := range shrinkLadder {
		f := bold(sz)
		fits := true
		for _, ln := range lines {
			if f.measure(ln) > maxW {
				fits = false
				break
			}
		}
		if fits {
			return f
		}
	}
	return bold(shrinkLadder[len(shrinkLadder)-1])
}

// wrapTextBM greedily wraps s onto lines no wider than maxW when measured in f.
func wrapTextBM(f *bitmapFont, s string, maxW int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	lines := make([]string, 0, 2)
	cur := words[0]
	for _, w := range words[1:] {
		trial := cur + " " + w
		if f.measure(trial) <= maxW {
			cur = trial
			continue
		}
		lines = append(lines, cur)
		cur = w
	}
	return append(lines, cur)
}
