#!/usr/bin/env python3
"""Pre-rasterizes printable ASCII glyphs from a TTF into a packed 1-bit bitmap atlas, using a
real hinting-aware rasterizer (FreeType via Pillow) instead of Go's un-hinted x/image/font/sfnt.
Output is a small custom binary format, embedded directly in the Go binary via go:embed and
blitted at render time — no vector rasterization happens on the broker/device anymore, so every
stem width and glyph edge is exactly what was verified here, not recomputed per-render.

Usage: python3 bake_font.py <ttf-path> <pixel-size> <out-path>
    python3 bake_font.py ../../internal/render/fonts/AtkinsonHyperlegible-Bold.ttf 53 \
        ../../internal/render/fonts/atlas/bold_53.fnt

Format (big-endian):
    magic       4 bytes  "BFA1"
    ascent      int16
    descent     int16
    numGlyphs   uint16    (fixed 95: codepoints 32..126)
    per glyph, in codepoint order 32..126:
        w, h        uint16 each
        offsetX     int16   (pen-relative x of the bitmap's left edge)
        offsetY     int16   (pen-relative y of the bitmap's top edge, from the baseline)
        advance     uint16
        bitmap      ceil(w/8)*h bytes, row-major, MSB-first, 1 = ink (thresholded)
"""
import struct
import sys

from PIL import Image, ImageDraw, ImageFont

THRESHOLD = 128  # alpha cutoff; matches the crisp binary-ink approach the renderer already uses

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

        row_bytes = (w + 7) // 8
        bitmap = bytearray(row_bytes * h)
        for yy in range(h):
            for xx in range(w):
                if px_data[xx, yy] >= THRESHOLD:
                    bitmap[yy * row_bytes + (xx // 8)] |= 0x80 >> (xx % 8)

        glyphs.append((w, h, x0, y0, round(advance), bytes(bitmap)))

    with open(out_path, "wb") as f:
        f.write(b"BFA1")
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
