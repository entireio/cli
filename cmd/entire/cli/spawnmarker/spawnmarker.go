// Package spawnmarker throttles detached one-shot child processes to one per
// repository per window.
//
// Every detached worker in this CLI (the trail-enablement refresh and the
// zombie-session sweep in package cli, the OPF flush in package strategy) is
// nominated by a hook that can fire in bursts and from several worktrees at
// once. Without a shared throttle each of those hooks forks its own child,
// every child re-opens the repository, and a condition that cannot self-heal
// is retried once per hook forever.
//
// The marker lives here rather than beside any one worker because package
// strategy cannot import package cli: the CLI commands are built on top of the
// strategy, so the dependency only runs one way.
package spawnmarker

import (
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// DirName is the marker directory inside the git common dir. It is the same
// "entire" directory clone preferences live in.
const DirName = "entire"

// RecentlySpawned reports whether the named spawn marker under the shared
// git-common-dir was refreshed within ttl and, when it wasn't, records now as
// the most recent spawn. The read-and-record is serialized with a flock keyed
// to the marker (so every worktree of the repo agrees), collapsing a burst of
// concurrent hooks to a single detached child rather than one per hook.
// Best-effort: any error resolving, locking, or writing the marker falls
// through to spawning — never worse than having no guard at all. Each caller
// passes its own marker name and ttl.
func RecentlySpawned(commonDir, marker string, ttl time.Duration, now time.Time) bool {
	root, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return false
	}
	// Create the directory before acquiring the lock: flock opens the lock file,
	// which fails if its parent doesn't exist yet (mirrors
	// ModifyClonePreferences, which creates before locking).
	if err := osroot.MkdirAllNoSymlink(root, DirName, 0o750); err != nil {
		return false
	}
	markerName := DirName + "/" + marker
	release, err := flock.AcquireIn(root, markerName+".lock")
	if err != nil {
		return false
	}
	defer release()

	if data, readErr := osroot.ReadFileNoFollow(root, markerName); readErr == nil {
		if last, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data))); parseErr == nil &&
			now.After(last) && now.Sub(last) < ttl {
			return true
		}
	}
	//nolint:errcheck // best-effort marker; a failed write just means the next hook re-spawns
	_ = jsonutil.WriteFileAtomicIn(root, markerName, []byte(now.UTC().Format(time.RFC3339Nano)), 0o600)
	return false
}
