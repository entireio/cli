// Package runnerdefaults embeds the canonical generic trail runner configs, so
// `entire runner setup` can scaffold them into a repository that has none yet.
// These are the structural contract (output adapters, result types, runtime)
// plus generic prompt templates; tune tailors the templates to the repo.
package runnerdefaults

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
)

//go:embed runners/*.json
var runnersFS embed.FS

// File is one default runner config: its base filename and raw JSON bytes.
type File struct {
	Name string
	Data []byte
}

// Files returns the embedded default runner configs, sorted by name.
func Files() ([]File, error) {
	entries, err := fs.ReadDir(runnersFS, "runners")
	if err != nil {
		return nil, fmt.Errorf("reading embedded runner defaults: %w", err)
	}
	out := make([]File, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := runnersFS.ReadFile(path.Join("runners", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading embedded runner %s: %w", e.Name(), err)
		}
		out = append(out, File{Name: e.Name(), Data: data})
	}
	return out, nil
}
