package server

import "embed"

// fontFS embeds Open Sans (Regular, latin + symbols subsets — the symbols subset is what covers
// the battery bar's block characters, █/░) so every page renders it identically with no external
// network dependency, the same self-contained-binary approach already used for the e-ink bitmap
// fonts in internal/render/fonts. See fonts/LICENSE (Apache 2.0).
//
//go:embed fonts/*.woff2
var fontFS embed.FS
