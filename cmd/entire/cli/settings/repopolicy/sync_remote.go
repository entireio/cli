package repopolicy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// SyncRemote is where this repository's checkpoint data leaves the machine:
// the elected checkpoint sync remote (checkpoint_push_remote → captured
// election → origin → sole remote → first remote) with the URLs a push to it
// delivers to, or the dedicated checkpoint_remote store's URL.
//
// Egress consent is keyed on this, not on "origin": since the sync-remote
// election landed, origin may be an unpushable base repo or a local mirror
// while checkpoints go to a fork or a private remote, and consent that names
// origin would say nothing about where transcripts actually go — and would
// stay valid when a captured election re-routes them.
type SyncRemote struct {
	// Name is the git remote name; "" when no remote is configured.
	Name string
	// URLs are where a push to this remote delivers: its pushurl entries when
	// any are configured (git then never pushes to the fetch URL), else its
	// fetch URLs; deduplicated. In dedicated checkpoint_remote mode it is the
	// single derived URL.
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
// seam as ClassifyLocalSettings. Every binary that links strategy (the CLI and
// its hooks) gets the election. The default fails loudly outside `go test`:
// silently keying consent on origin in a binary without the election would
// name a remote checkpoints may never go to. Under test it is the election's
// own default tier, origin, so leaf and settings tests need no installer.
var ResolveSyncRemote SyncRemoteResolver = defaultSyncRemoteResolver

// ErrSyncRemoteResolverMissing reports that no election was installed.
var ErrSyncRemoteResolverMissing = errors.New("checkpoint sync remote resolver not installed")

func defaultSyncRemoteResolver(ctx context.Context, repository Repository) (SyncRemote, error) {
	if !testing.Testing() {
		return SyncRemote{}, ErrSyncRemoteResolverMissing
	}
	return RemoteURLsInDir(ctx, repository.WorktreeRoot, "origin")
}

// RemoteURLsInDir collects the URLs a push to a configured remote delivers to:
// its pushurl entries when any are set, else its fetch URLs (git's own rule —
// a configured pushurl replaces, not supplements, the URL for pushes). A
// remote that is not configured yields Name == "" and no URLs; only a failure
// to read the git config is an error.
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
	delivery := dedupeURLs(pushURLs)
	if len(delivery) == 0 {
		delivery = dedupeURLs(urls)
	}
	return SyncRemote{Name: name, URLs: delivery}, nil
}

func dedupeURLs(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if u != "" && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}
