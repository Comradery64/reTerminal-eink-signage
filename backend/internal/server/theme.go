package server

// baseCSS is the shared design system for the /admin, /manager, and login pages: a palette drawn
// from the fleet's actual E Ink Spectra-6 gamut (paper, ink, and the panel's four signal colors)
// rather than a generic dashboard palette, so the control surface visually quotes the hardware it
// manages. Deliberately light-mode only — the physical displays have no backlit dark mode, and
// these pages are meant to feel like an extension of them, not a typical SaaS console.
const baseCSS = `
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
     prose/labels), mono (device IDs, readouts, hardware data — never mixed into prose). */
  --font-display: "Iowan Old Style", "Palatino Linotype", Georgia, serif;
  --font-body: -apple-system, "Segoe UI", system-ui, sans-serif;
  --font-mono: ui-monospace, "SF Mono", "Cascadia Mono", Menlo, monospace;
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
.readout .bar { color: var(--ink); letter-spacing: -1px; }
.chip {
  display: inline-flex; align-items: center; gap: var(--space-1);
  font-family: var(--font-mono); font-size: .65rem; text-transform: uppercase; letter-spacing: .06em;
  padding: .2rem var(--space-2); border-radius: var(--radius); border: 1px solid currentColor;
}
.chip::before { content: ""; width: .45rem; height: .45rem; background: currentColor; }
.chip-ok { color: var(--green); }
.chip-stale, .chip-low_battery, .chip-warn { color: var(--amber); }
.chip-unreported { color: var(--red); }
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

const batteryBarSegments = 10

// batteryBar renders a 10-segment block glyph ("███████░░░") for a battery percentage — a
// monospace, hardware-readout stand-in for a graphical battery icon, consistent with the theme's
// terminal-derived data typography.
func batteryBar(pct int) string {
	segments := (pct + 5) / 10 // round to nearest tenth
	if segments < 0 {
		segments = 0
	}
	if segments > batteryBarSegments {
		segments = batteryBarSegments
	}
	bar := make([]byte, 0, batteryBarSegments*3) // each glyph is a multi-byte UTF-8 rune
	for i := 0; i < segments; i++ {
		bar = append(bar, "█"...)
	}
	for i := segments; i < batteryBarSegments; i++ {
		bar = append(bar, "░"...)
	}
	return string(bar)
}
