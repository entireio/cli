package cli

import (
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

// mirrorProbeCache is a best-effort per-user cache of mirror-probe results
// keyed by "owner/repo", on the shared jsonFileCache shell. The ground truth
// stays the control plane.
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

func (c mirrorProbeCache) shell() jsonFileCache[mirrorProbeEntry] {
	return jsonFileCache[mirrorProbeEntry]{path: c.path}
}

func (c mirrorProbeCache) get(slug string, now time.Time) (mirrored, unreachable, ok bool) {
	entry, found := c.shell().load()[slug]
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

// clearUnreachable drops every cached failure while keeping successful probe
// results. An explicit `entire enable` calls this before its checks: the user
// asked for setup, so a transient blip cached minutes ago must not suppress
// the mirror offer for the rest of the failure TTL.
func (c mirrorProbeCache) clearUnreachable() {
	shell := c.shell()
	entries := shell.load()
	changed := false
	for slug, entry := range entries {
		if entry.Unreachable {
			delete(entries, slug)
			changed = true
		}
	}
	if changed {
		shell.store(entries)
	}
}

func (c mirrorProbeCache) write(slug string, entry mirrorProbeEntry) {
	shell := c.shell()
	entries := shell.load()
	entries[slug] = entry
	shell.store(entries)
}
