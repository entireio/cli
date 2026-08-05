package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// jsonFileCache is the shared persistence shell for the onboarding caches:
// a JSON file holding a map of entries. It carries no invalidation policy —
// TTL (mirror probe) vs exact fingerprint (import scan) live in the caches
// themselves. Best-effort by design: read or write failures degrade to cache
// misses, never errors, because ground truth is always re-derivable.
type jsonFileCache[E any] struct {
	path string
}

type jsonCacheFile[E any] struct {
	Entries map[string]E `json:"entries"`
}

func (c jsonFileCache[E]) load() map[string]E {
	var f jsonCacheFile[E]
	data, err := os.ReadFile(c.path)
	if err != nil || json.Unmarshal(data, &f) != nil || f.Entries == nil {
		return map[string]E{}
	}
	return f.Entries
}

// store persists the full entry map atomically (tmp + rename), so a reader
// racing a writer sees either the old or the new file, never a torn one.
func (c jsonFileCache[E]) store(entries map[string]E) {
	data, err := json.Marshal(jsonCacheFile[E]{Entries: entries})
	if err != nil {
		return
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(c.path)+".tmp-*")
	if err != nil {
		return
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), c.path); err != nil {
		_ = os.Remove(tmp.Name())
	}
}
