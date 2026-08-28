package repopolicy

import (
	"context"
	"fmt"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// SyncRemote is where this repository's checkpoint data leaves the machine:
// the elected checkpoint sync remote (checkpoint_push_remote → captured
// election → origin → sole remote → first remote) with every configured
// fetch and push URL, or the dedicated checkpoint_remote store's URL.
//
// Egress consent is keyed on this, not on "origin": since the sync-remote
// election landed, origin may be an unpushable base repo or a local mirror
// while checkpoints go to a fork or a private remote, and consent that names
// origin would say nothing about where transcripts actually go — and would
// stay valid when a captured election re-routes them.
type SyncRemote struct {
	// Name is the git remote name; "" when no remote is configured.
	Name string
	// URLs are the remote's configured fetch and push URLs, deduplicated, or
	// the single derived URL in dedicated checkpoint_remote mode.
	URLs []string
	// Dedicated reports checkpoint_remote URL mode (a dedicated metadata
	// store addressed directly, exempt from the remote election).
	Dedicated bool
}

// SyncRemoteResolver resolves the checkpoint sync remote for a repository.
type SyncRemoteResolver func(ctx context.Context, repository Repository) (SyncRemote, error)

// ResolveSyncRemote is the resolver egress consent keys on. This leaf cannot
// import the strategy package that owns the election (cycle through settings),
// so strategy installs the real resolver at package initialization — the same
// seam as LocalSettingsTrusted. The default here is the election's own default
// tier, origin, which is also the pre-election reading: correct for every
// repository that pushes where it fetches, and what runs in binaries and tests
// that do not link strategy.
var ResolveSyncRemote SyncRemoteResolver = func(ctx context.Context, repository Repository) (SyncRemote, error) {
	return RemoteURLsInDir(ctx, repository.WorktreeRoot, "origin")
}

// RemoteURLsInDir collects a configured remote's fetch and push URLs. A remote
// that is not configured yields Name == "" and no URLs; only a failure to read
// the git config is an error.
func RemoteURLsInDir(ctx context.Context, dir, name string) (SyncRemote, error) {
	urls, fetchFound, err := gitremote.GetRemoteURLsInDirIfSet(ctx, dir, name)
	if err != nil {
		return SyncRemote{}, fmt.Errorf("reading remote %q: %w", name, err)
	}
	pushURLs, pushFound, err := gitremote.GetRemotePushURLsInDirIfSet(ctx, dir, name)
	if err != nil {
		return SyncRemote{}, fmt.Errorf("reading remote %q pushurl: %w", name, err)
	}
	if !fetchFound && !pushFound {
		return SyncRemote{}, nil
	}
	all := make([]string, 0, len(urls)+len(pushURLs))
	for _, u := range append(urls, pushURLs...) {
		if u != "" && !slices.Contains(all, u) {
			all = append(all, u)
		}
	}
	return SyncRemote{Name: name, URLs: all}, nil
}
