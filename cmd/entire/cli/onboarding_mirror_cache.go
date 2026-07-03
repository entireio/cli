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

// mirrorProbeFailureTTL is the shorter trust window for unreachable-core
// results: an authed-but-offline terminal hangs on the probe once per this
// window instead of on every status invocation, and recovers quickly once
// back online.
const mirrorProbeFailureTTL = 5 * time.Minute

// mirrorProbeCache is a best-effort per-user file cache of mirror-probe
// results keyed by "owner/repo". Read/write errors degrade to cache misses;
// the ground truth stays the control plane.
type mirrorProbeCache struct {
	path       string
	ttl        time.Duration
	failureTTL time.Duration
}

func defaultMirrorProbeCache() mirrorProbeCache {
	return mirrorProbeCache{
		path:       filepath.Join(userdirs.Cache(), "onboarding_mirror.json"),
		ttl:        mirrorProbeTTL,
		failureTTL: mirrorProbeFailureTTL,
	}
}

type mirrorProbeEntry struct {
	Mirrored    bool      `json:"mirrored"`
	Unreachable bool      `json:"unreachable,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
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

func (c mirrorProbeCache) get(slug string, now time.Time) (mirrored, unreachable, ok bool) {
	entry, found := c.load().Entries[slug]
	if !found {
		return false, false, false
	}
	ttl := c.ttl
	if entry.Unreachable {
		ttl = c.failureTTL
	}
	if now.Sub(entry.CheckedAt) > ttl {
		return false, false, false
	}
	return entry.Mirrored, entry.Unreachable, true
}

func (c mirrorProbeCache) put(slug string, mirrored bool, now time.Time) {
	c.write(slug, mirrorProbeEntry{Mirrored: mirrored, CheckedAt: now})
}

func (c mirrorProbeCache) putUnreachable(slug string, now time.Time) {
	c.write(slug, mirrorProbeEntry{Unreachable: true, CheckedAt: now})
}

func (c mirrorProbeCache) write(slug string, entry mirrorProbeEntry) {
	f := c.load()
	f.Entries[slug] = entry
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
