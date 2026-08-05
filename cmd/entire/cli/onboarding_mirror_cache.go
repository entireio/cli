package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
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

// mirrorProbeCache is a best-effort per-user cache of mirror-probe results,
// keyed by mirrorProbeKey (auth identity + "owner/repo"), on the shared
// jsonFileCache shell. The ground truth stays the control plane.
type mirrorProbeCache struct {
	path       string
	ttl        time.Duration
	failureTTL time.Duration
}

// mirrorProbeKey scopes a probe-cache entry to the auth identity the probe
// runs under. probeRepoMirrored consults the *active context's* core, so the
// answer is identity-dependent: after `entire auth use`, a result cached
// under the previous context could show the wrong identity's mirror state for
// the rest of the TTL. ENTIRE_TOKEN sessions are scoped by a token digest — a
// changed token is a changed identity, and parsing the aud claim would cost
// more than it buys.
func mirrorProbeKey(slug string) string {
	if tok := os.Getenv(auth.EnvTokenVar); tok != "" {
		sum := sha256.Sum256([]byte(tok))
		return "env-" + hex.EncodeToString(sum[:4]) + "|" + slug
	}
	if _, current, err := auth.Contexts(); err == nil && current != "" {
		return "ctx-" + current + "|" + slug
	}
	return "ctx-none|" + slug
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
	Suspended   bool      `json:"suspended,omitempty"`
	Unreachable bool      `json:"unreachable,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

func (c mirrorProbeCache) shell() jsonFileCache[mirrorProbeEntry] {
	return jsonFileCache[mirrorProbeEntry]{path: c.path}
}

func (c mirrorProbeCache) get(slug string, now time.Time) (probe mirrorProbeResult, unreachable, ok bool) {
	entry, found := c.shell().load()[slug]
	if !found {
		return mirrorProbeResult{}, false, false
	}
	ttl := c.ttl
	if entry.Unreachable {
		ttl = c.failureTTL
	}
	if now.Sub(entry.CheckedAt) > ttl {
		return mirrorProbeResult{}, false, false
	}
	return mirrorProbeResult{Mirrored: entry.Mirrored, Suspended: entry.Suspended}, entry.Unreachable, true
}

func (c mirrorProbeCache) put(slug string, probe mirrorProbeResult, now time.Time) {
	c.write(slug, mirrorProbeEntry{Mirrored: probe.Mirrored, Suspended: probe.Suspended, CheckedAt: now})
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
