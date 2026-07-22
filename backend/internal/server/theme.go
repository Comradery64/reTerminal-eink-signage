package server

import (
	"fmt"
	"html/template"
)

// baseCSS is the shared design system for the /admin, /manager, and login pages: a palette drawn
// from the fleet's actual E Ink Spectra-6 gamut (paper, ink, and the panel's four signal colors)
// rather than a generic dashboard palette, so the control surface visually quotes the hardware it
// manages. Deliberately light-mode only — the physical displays have no backlit dark mode, and
// these pages are meant to feel like an extension of them, not a typical SaaS console.
const baseCSS = `
/* Self-hosted (see internal/server/fonts.go + /assets/fonts route) so every page renders
   identically with no external network dependency. Two subsets: latin covers ordinary text;
   symbols covers the battery bar's block characters (█/░), which fall outside the latin range. */
@font-face {
  font-family: "Open Sans";
  font-style: normal;
  font-weight: 400;
  font-display: swap;
  src: url("/assets/fonts/OpenSans-Regular-latin.woff2") format("woff2");
  unicode-range: U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD;
}
@font-face {
  font-family: "Open Sans";
  font-style: normal;
  font-weight: 400;
  font-display: swap;
  src: url("/assets/fonts/OpenSans-Regular-symbols.woff2") format("woff2");
  unicode-range: U+2150-218F, U+2190, U+2192, U+2194-2199, U+21AF, U+21E6-21F0, U+21F3, U+2218-2219, U+2299, U+22C4-22C6, U+2300-243F, U+2440-244A, U+2460-24FF, U+25A0-27BF, U+2800-28FF;
}
:root {
  /* Palette: the fleet's actual E Ink Spectra-6 gamut, not a generic dashboard palette — every
     color here is one the physical panels can literally render, so status states are drawn from
     the hardware's own language rather than decorative SaaS red/amber/green. */
  --paper: #f1eee4;
  --paper-raised: #e7e2d2;
  --ink: #1c1b17;
  --ink-soft: #5c584d;
  --red: #a83226;
  --amber: #a67721;
  --blue: #2b5488;
  --green: #2b7047;
  --line: rgba(28, 27, 23, .18);
  --radius: 3px;
  /* Every button-like control (buttons, segmented-toggle labels) shares this height, so a real
     <button> and a styled <label> next to it never look like different-sized controls. */
  --control-h: 2.5rem;

  /* Spacing scale — every margin/padding/gap on these pages resolves to one of these, so the
     rhythm reads as one system rather than page-by-page guesswork. */
  --space-1: .25rem;
  --space-2: .5rem;
  --space-3: .75rem;
  --space-4: 1rem;
  --space-5: 1.5rem;
  --space-6: 2rem;
  --space-7: 3rem;

  /* Type scale, three families with fixed roles: display (headings, signage feel), body (UI
     prose/labels), mono (device IDs, readouts, hardware data — never mixed into prose). --font-mono
     is no longer an actual monospace face (Open Sans is proportional) — the role/usage split is
     kept as-is, only the typeface changed, so alignment-by-monospace (e.g. the battery bar) relies
     on the block characters' fixed advance width rather than the font being monospace overall. */
  --font-display: "Iowan Old Style", "Palatino Linotype", Georgia, serif;
  --font-body: -apple-system, "Segoe UI", system-ui, sans-serif;
  --font-mono: "Open Sans", -apple-system, "Segoe UI", system-ui, sans-serif;
  --text-xs: .68rem;
  --text-sm: .78rem;
  --text-base: .92rem;
  --text-md: 1.05rem;
  --text-lg: 1.4rem;
  --text-xl: 1.9rem;
}
* { box-sizing: border-box; }
html, body { margin: 0; }
body {
  background: var(--paper);
  color: var(--ink);
  font-family: var(--font-body);
  font-size: var(--text-base);
  -webkit-font-smoothing: antialiased;
  /* Faint dither texture — the same trick internal/render uses to fake grays on a 6-color panel,
     applied here at low opacity so the page reads as printed on the same stock as the displays. */
  background-image: repeating-conic-gradient(from 0deg at 0 0, rgba(28, 27, 23, .045) 0deg 90deg, transparent 90deg 180deg);
  background-size: 3px 3px;
}
h1 { font-family: var(--font-display); font-weight: 600; letter-spacing: -.01em; margin: 0 0 var(--space-1); font-size: var(--text-xl); }
h2 { font-family: var(--font-display); font-weight: 600; letter-spacing: -.01em; margin: 0 0 var(--space-1); font-size: var(--text-lg); }
h3 { font-family: var(--font-display); font-weight: 600; letter-spacing: -.01em; margin: 0 0 var(--space-1); font-size: var(--text-md); }
p { margin: 0 0 var(--space-3); color: var(--ink-soft); font-size: var(--text-base); }
a { color: var(--blue); text-decoration-thickness: 1px; }
:focus-visible { outline: 2px solid var(--blue); outline-offset: 2px; }

/* Brand mark: a cluster of three signal-color swatches (the literal palette the fleet's displays
   render in) beside the wordmark — used identically on the login card, /manager topbar, and
   /admin rail so the three surfaces read as one product, not three separately-styled pages. */
.brand { display: inline-flex; align-items: center; gap: var(--space-2); }
.brand .swatches { display: inline-flex; gap: 2px; }
.brand .swatches span { display: block; width: .5rem; height: .5rem; }
.brand .swatches span:nth-child(1) { background: var(--red); }
.brand .swatches span:nth-child(2) { background: var(--blue); }
.brand .swatches span:nth-child(3) { background: var(--green); }
.brand small {
  font-family: var(--font-mono); font-size: var(--text-xs); text-transform: uppercase;
  letter-spacing: .12em; color: var(--ink-soft);
}

.eyebrow {
  font-family: var(--font-mono); font-size: var(--text-xs); text-transform: uppercase; letter-spacing: .12em;
  color: var(--ink-soft); margin: 0 0 var(--space-2);
}
button, input, select {
  font: inherit; color: var(--ink); background: var(--paper); font-size: var(--text-base);
  border: 1px solid var(--line); border-radius: var(--radius); padding: var(--space-2) var(--space-3);
  vertical-align: middle;
}
button {
  background: var(--ink); color: var(--paper); border-color: var(--ink);
  cursor: pointer; font-weight: 600; letter-spacing: .03em; text-transform: uppercase; font-size: var(--text-xs);
  min-height: var(--control-h); display: inline-flex; align-items: center; justify-content: center;
  padding: 0 var(--space-4);
}
button:hover { background: var(--blue); border-color: var(--blue); }
button.ghost { background: transparent; color: var(--ink-soft); border-color: transparent; }
button.ghost:hover { background: var(--paper-raised); color: var(--ink); }
button.danger { background: transparent; color: var(--red); border-color: var(--red); }
button.danger:hover { background: var(--red); color: var(--paper); }
.mono { font-family: var(--font-mono); }

/* Surface: the one raised-panel treatment reused everywhere something needs to sit above the
   paper texture — the login card, room cards on /manager, the add/edit box on /admin. */
.surface { background: var(--paper-raised); border: 1px solid var(--line); border-radius: var(--radius); }

.topbar {
  display: flex; align-items: baseline; justify-content: space-between; gap: var(--space-4);
  padding: var(--space-5) var(--space-6); border-bottom: 1px solid var(--line);
}
.masthead { display: flex; flex-direction: column; gap: var(--space-2); }
/* Cross-links between /dashboard, /manager, and /admin — one login now grants whichever of these
   an account's role satisfies, so each page's topbar surfaces links to the others instead of
   requiring a separate login per door. */
.nav-links { display: flex; align-items: center; gap: var(--space-4); }
.nav-links a { font-size: var(--text-sm); }

/* Room roster: the read-only card grid shared by /manager and /dashboard — same status chip,
   battery readout, and card treatment either way, so the two views read as one product with
   different amounts of control layered on top, not two separately-designed pages. */
.grid {
  display: grid; grid-template-columns: repeat(auto-fill, minmax(17rem, 1fr));
  gap: var(--space-4); padding: var(--space-5) var(--space-6);
}
.card { position: relative; padding: var(--space-4) var(--space-5) var(--space-5); }
.card .chip { position: absolute; top: var(--space-4); right: var(--space-4); }
.card h2 { padding-right: 5.5rem; }
.card .id { margin-bottom: var(--space-3); font-size: var(--text-sm); }
.readout { font-family: var(--font-mono); font-size: var(--text-sm); color: var(--ink-soft); margin-bottom: var(--space-4); }
.battgauge {
  display: inline-block; width: 3.5rem; height: .55rem; vertical-align: middle;
  background: var(--paper); border: 1px solid var(--ink-soft); border-radius: 1px; overflow: hidden;
}
.battgauge-fill { display: block; height: 100%; background: var(--ink); }
.chip {
  display: inline-flex; align-items: center; gap: var(--space-1);
  font-family: var(--font-mono); font-size: .65rem; text-transform: uppercase; letter-spacing: .06em;
  padding: .2rem var(--space-2); border-radius: var(--radius); border: 1px solid currentColor;
}
.chip::before { content: ""; width: .45rem; height: .45rem; background: currentColor; }
.chip-ok { color: var(--green); }
.chip-stale, .chip-low_battery, .chip-warn { color: var(--amber); }
/* Neutral, not alarming — "unreported" commonly just means "hasn't had its first check-in yet"
   (e.g. right after a broker restart, or a device on a long calendar-driven wake cycle), which is
   a normal transient state, not a fault. Genuine faults use chip-stale (amber) above. */
.chip-unreported { color: var(--blue); }
.banner {
  margin: var(--space-4) var(--space-6) 0; padding: var(--space-2) var(--space-3);
  border-radius: var(--radius); font-size: var(--text-base);
}
.banner-error { border: 1px solid var(--red); color: var(--red); }
.banner-ok { border: 1px solid var(--green); color: var(--green); }
@media (prefers-reduced-motion: no-preference) {
  .flash { animation: eink-flash .6s steps(1, jump-none) 2; }
}
@keyframes eink-flash {
  50% { background: var(--ink); border-color: var(--ink); }
}
`

// brandMark is the shared wordmark+swatch snippet used on the login card, /manager topbar, and
// /admin rail — inserted via string concatenation (not a template partial, since each page's
// template is its own independent template.Must(...Parse(...)) literal) so all three surfaces
// share the exact same markup, not just the same CSS class names.
const brandMark = `<div class="brand"><span class="swatches"><span></span><span></span><span></span></span><small>Meeting display fleet</small></div>`

// batteryGauge renders a small CSS-drawn gauge (an outlined track with a proportionally-filled
// bar) for a battery percentage. Previously this was a string of block-character glyphs
// ("███████░░░"), which relied on every glyph having the same advance width to line up evenly —
// true under the old monospace font, but not under Open Sans (or whatever font a glyph without a
// block-character mapping falls back to), so the "filled" and "empty" portions rendered at visibly
// different, inconsistent widths. Drawing the fill as a percentage-width div sidesteps font
// metrics entirely.
func batteryGauge(pct int) template.HTML {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return template.HTML(fmt.Sprintf(`<span class="battgauge"><span class="battgauge-fill" style="width:%d%%"></span></span>`, pct))
}
