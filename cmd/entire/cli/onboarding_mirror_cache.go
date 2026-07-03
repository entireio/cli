package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// mirrorProbeTTL bounds how long a mirror-probe result is trusted before
// re-asking the control plane. `entire status` is a hot, previously
// local-only command — the cache keeps its steady state offline-fast, while
// the TTL picks up mirrors created or removed out-of-band.
const mirrorProbeTTL = 15 * time.Minute

// mirrorProbeCache is a best-effort per-user file cache of mirror-probe
// results keyed by "owner/repo". Read/write errors degrade to cache misses;
// the ground truth stays the control plane.
type mirrorProbeCache struct {
	path string
	ttl  time.Duration
}

func defaultMirrorProbeCache() mirrorProbeCache {
	return mirrorProbeCache{
		path: filepath.Join(userdirs.Cache(), "onboarding_mirror.json"),
		ttl:  mirrorProbeTTL,
	}
}

type mirrorProbeEntry struct {
	Mirrored  bool      `json:"mirrored"`
	CheckedAt time.Time `json:"checked_at"`
}

type mirrorProbeFile struct {
	Entries map[string]mirrorProbeEntry `json:"entries"`
}

func (c mirrorProbeCache) load() mirrorProbeFile {
	var f mirrorProbeFile
	data, err := os.ReadFile(c.path)
	if err != nil || json.Unmarshal(data, &f) != nil || f.Entries == nil {
		return mirrorProbeFile{Entries: map[string]mirrorProbeEntry{}}
	}
	return f
}

func (c mirrorProbeCache) get(slug string, now time.Time) (mirrored, ok bool) {
	entry, found := c.load().Entries[slug]
	if !found || now.Sub(entry.CheckedAt) > c.ttl {
		return false, false
	}
	return entry.Mirrored, true
}

func (c mirrorProbeCache) put(slug string, mirrored bool, now time.Time) {
	f := c.load()
	f.Entries[slug] = mirrorProbeEntry{Mirrored: mirrored, CheckedAt: now}
	data, err := json.Marshal(f)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return
	}
	//nolint:errcheck,gosec // best-effort cache write; a miss next time is fine
	os.WriteFile(c.path, data, 0o600)
}
