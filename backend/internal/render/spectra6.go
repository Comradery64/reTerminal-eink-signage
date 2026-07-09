package render

import (
	"image"
	"image/color"
)

// Spectra6Code is the 4-bit controller color code per the panel datasheet.
// CHANGE HERE (one place) if your E1002 panel revision uses a different mapping;
// must stay in sync with firmware/main/epd_spectra6.cpp EPD_CLR_*.
type Spectra6Code uint8

const (
	CodeBlack  Spectra6Code = 0x0
	CodeWhite  Spectra6Code = 0x1
	CodeYellow Spectra6Code = 0x2
	CodeRed    Spectra6Code = 0x3
	CodeBlue   Spectra6Code = 0x5
	CodeGreen  Spectra6Code = 0x6
)

// palEntry binds a displayable RGB to its controller code.
type palEntry struct {
	c    color.RGBA
	code Spectra6Code
}

// The 6 physical inks of E Ink Spectra 6, as approximate sRGB anchors used for nearest-color
// matching and dithering. Tuned toward the panel's real (muted) gamut, not pure primaries.
var palette = []palEntry{
	{color.RGBA{0x00, 0x00, 0x00, 0xFF}, CodeBlack},
	{color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}, CodeWhite},
	{color.RGBA{0xE0, 0xC8, 0x10, 0xFF}, CodeYellow},
	{color.RGBA{0xC0, 0x20, 0x20, 0xFF}, CodeRed},
	{color.RGBA{0x20, 0x30, 0xA0, 0xFF}, CodeBlue},
	{color.RGBA{0x20, 0x80, 0x40, 0xFF}, CodeGreen},
}

// Paletted returns an *image.Paletted whose palette indices line up with `palette`,
// so a renderer can draw with exact ink colors and we can pack losslessly afterward.
func newCanvas(w, h int) *image.Paletted {
	pal := make(color.Palette, len(palette))
	for i, p := range palette {
		pal[i] = p.c
	}
	img := image.NewPaletted(image.Rect(0, 0, w, h), pal)
	// default to white (paper)
	white := uint8(1)
	for i := range img.Pix {
		img.Pix[i] = white
	}
	return img
}

func nearestIndex(c color.RGBA) int {
	best, bestD := 0, 1<<31
	for i, p := range palette {
		dr := int(c.R) - int(p.c.R)
		dg := int(c.G) - int(p.c.G)
		db := int(c.B) - int(p.c.B)
		// luma-weighted distance — perceptually closer matches for text vs. fills.
		d := 2*dr*dr + 4*dg*dg + 3*db*db
		if d < bestD {
			bestD, best = d, i
		}
	}
	return best
}

// quantize maps an arbitrary RGBA image onto the 6-color palette, optionally with
// Floyd–Steinberg dithering, returning an *image.Paletted aligned to `palette`.
func quantize(src image.Image, dither bool) *image.Paletted {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	out := newCanvas(w, h)

	if !dither {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				r, g, bl, _ := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
				out.Pix[y*out.Stride+x] = uint8(nearestIndex(color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), 0xFF}))
			}
		}
		return out
	}

	// Floyd–Steinberg on a float error buffer.
	type fc struct{ r, g, b float64 }
	buf := make([]fc, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			buf[y*w+x] = fc{float64(r >> 8), float64(g >> 8), float64(bl >> 8)}
		}
	}
	clamp := func(v float64) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			old := buf[y*w+x]
			idx := nearestIndex(color.RGBA{clamp(old.r), clamp(old.g), clamp(old.b), 0xFF})
			np := palette[idx].c
			out.Pix[y*out.Stride+x] = uint8(idx)
			er := old.r - float64(np.R)
			eg := old.g - float64(np.G)
			eb := old.b - float64(np.B)
			spread := func(dx, dy int, f float64) {
				nx, ny := x+dx, y+dy
				if nx < 0 || nx >= w || ny < 0 || ny >= h {
					return
				}
				p := &buf[ny*w+nx]
				p.r += er * f
				p.g += eg * f
				p.b += eb * f
			}
			spread(1, 0, 7.0/16)
			spread(-1, 1, 3.0/16)
			spread(0, 1, 5.0/16)
			spread(1, 1, 1.0/16)
		}
	}
	return out
}

// Pack converts a palette-aligned image into the MDPF packed 4bpp framebuffer:
// 2 pixels per byte, high nibble = even/left pixel, nibble value = Spectra6Code.
func Pack(img *image.Paletted) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	out := make([]byte, w*h/2)
	oi := 0
	for y := 0; y < h; y++ {
		row := y * img.Stride
		for x := 0; x < w; x += 2 {
			hi := codeFor(img.Pix[row+x])
			lo := codeFor(img.Pix[row+x+1])
			out[oi] = byte(hi)<<4 | byte(lo)
			oi++
		}
	}
	return out
}

func codeFor(palIdx uint8) Spectra6Code {
	if int(palIdx) < len(palette) {
		return palette[palIdx].code
	}
	return CodeWhite
}
