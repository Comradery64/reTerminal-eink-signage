#!/usr/bin/env python3
"""Pre-rasterizes printable ASCII glyphs from a TTF into a full 8-bit antialiased-coverage bitmap
atlas, using a real hinting-aware rasterizer (FreeType via Pillow) instead of Go's un-hinted
x/image/font/sfnt. Output is a small custom binary format, embedded directly in the Go binary via
go:embed and alpha-blended at render time — no vector rasterization happens on the broker/device
anymore, so every stem width and glyph edge is exactly what was verified here, not recomputed
per-render.

Full 8-bit coverage (not a coarser bucketed level) matters specifically because Spectra 6 has no
gray ink at all — only one black and one white (plus 4 saturated colors). A blended edge pixel only
gets a chance at real Floyd-Steinberg dithering (visible speckle instead of a single deterministic
snap) when its value can land close to the black/white midpoint (127); a coarse 2-bit/4-level
scheme (tried first, see git history) only ever produced values solidly on one side of that
midpoint, so every edge pixel resolved to a fixed decision — indistinguishable from a hard 1-bit
threshold. Full 8-bit precision is what these reference tools (xtctool, PaperSpecimenS3,
EPDFont-style 2-bit converters aimed at *real* multi-level-gray panels) actually rely on.

Usage: python3 bake_font.py <ttf-path> <pixel-size> <out-path>
    python3 bake_font.py ../../internal/render/fonts/Inter-Regular.ttf 32 \
        ../../internal/render/fonts/atlas/regular_32.fnt

Format (big-endian):
    magic       4 bytes  "BFA3"
    ascent      int16
    descent     int16
    numGlyphs   uint16    (fixed 95: codepoints 32..126)
    per glyph, in codepoint order 32..126:
        w, h        uint16 each
        offsetX     int16   (pen-relative x of the bitmap's left edge)
        offsetY     int16   (pen-relative y of the bitmap's top edge, from the baseline)
        advance     uint16
        bitmap      w*h bytes, row-major, one byte per pixel = raw antialiased alpha 0-255
"""
import struct
import sys

from PIL import Image, ImageDraw, ImageFont

FIRST_CP, LAST_CP = 32, 126


def bake(ttf_path, px, out_path):
    font = ImageFont.truetype(ttf_path, size=px)
    ascent, descent = font.getmetrics()

    glyphs = []
    for cp in range(FIRST_CP, LAST_CP + 1):
        ch = chr(cp)
        advance = font.getlength(ch)
        bbox = font.getbbox(ch)  # (x0, y0, x1, y1), pen-relative, y down from baseline-origin
        if bbox is None or bbox[2] <= bbox[0] or bbox[3] <= bbox[1]:
            glyphs.append((0, 0, 0, 0, round(advance), b""))
            continue
        x0, y0, x1, y1 = bbox
        w, h = x1 - x0, y1 - y0
        img = Image.new("L", (w, h), 0)
        d = ImageDraw.Draw(img)
        d.text((-x0, -y0), ch, font=font, fill=255)
        px_data = img.load()

        bitmap = bytearray(w * h)
        for yy in range(h):
            for xx in range(w):
                bitmap[yy * w + xx] = px_data[xx, yy]

        glyphs.append((w, h, x0, y0, round(advance), bytes(bitmap)))

    with open(out_path, "wb") as f:
        f.write(b"BFA3")
        f.write(struct.pack(">hhH", ascent, descent, len(glyphs)))
        for w, h, ox, oy, adv, bitmap in glyphs:
            f.write(struct.pack(">HHhhH", w, h, ox, oy, adv))
            f.write(bitmap)

    print(f"wrote {out_path}: {px}px, {len(glyphs)} glyphs, "
          f"{sum(len(g[5]) for g in glyphs)} bitmap bytes")


if __name__ == "__main__":
    if len(sys.argv) != 4:
        print(__doc__)
        sys.exit(1)
    bake(sys.argv[1], int(sys.argv[2]), sys.argv[3])
