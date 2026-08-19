package pricing

import "embed"

// modelsFS holds the embedded per-provider pricing tables (models/*.json),
// parsed by LoadTable.
//
//go:embed models/*.json
var modelsFS embed.FS
