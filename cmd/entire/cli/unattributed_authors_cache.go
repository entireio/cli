package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// Cache — <git common dir>/entire-unattributed-authors.json, ONE entry. A
// successful outcome with candidates is valid while the origin default-branch
// tip, else HEAD, is unchanged; a no-candidates or skipped outcome only for
// transientCacheTTL, so neither a clean history nor a failing network call is
// re-paid (a `git shortlog` walk, or a cell round trip) by every
// `entire status`. Best-effort: any I/O or decode error is a miss.
const (
	unattributedAuthorsCacheFile = "entire-unattributed-authors.json"
	// transientCacheTTL bounds how long a no-candidates or skipped outcome is
	// served before detection tries again. Both are transient for the same
	// reason: candidates come from local refs, not just the cell, so keying a
	// no-candidates entry to the tip alone would let doctor miss a brand-new
	// local commit under a bad address for too long — a quiet repo's tip can
	// go unchanged for weeks. An outcome that did find candidates and got a
	// clean read from the cell keeps the until-tip-moves rule below.
	transientCacheTTL = 10 * time.Minute
)

// cachedDetection is the on-disk cache entry. A placement-failed entry
// (RepoID == "") is served, Skipped and all, for transientCacheTTL and
// short-circuits placement resolution until then — that repeated skip is
// intended, not a bug: it is what keeps a down cell from being re-dialed on
// every `entire status`. `entire login` does not clear it on its own; a caller
// that wants the next detection to try the network again (doctor's fix path,
// a future `entire login` hook) must call invalidateUnattributedAuthorsCache.
//
// Candidates rides along so a cache hit can be served without ever running
// `git shortlog`: detection reads the cache by tip before it computes
// Candidates, and a hit returns the stored list in its place. An entry with
// no Candidates (logged-in, clean history) is written and read back the same
// way, but — like a Skipped entry — only within transientCacheTTL; see
// readUnattributedAuthorsCache.
type cachedDetection struct {
	Tip        string               `json:"tip"`
	RepoID     string               `json:"repo_id,omitempty"` // empty when placement failed
	Candidates []string             `json:"candidates,omitempty"`
	Authors    []unattributedAuthor `json:"authors,omitempty"`
	Skipped    string               `json:"skipped,omitempty"`
	FetchedAt  time.Time            `json:"fetched_at"`
}

// unattributedAuthorsCacheRoot opens the shared *os.Root over commonDir, the
// same way the writer and reader must agree on. gitdir.OpenAt applies
// filepath.Abs itself, so this works whether or not strategy.GetGitCommonDir
// returned a relative path (its own Clean/Join does not guarantee absolute).
func unattributedAuthorsCacheRoot(commonDir string) (*os.Root, error) {
	return gitdir.OpenAt(commonDir) //nolint:wrapcheck // gitdir already names the directory and the failure
}

// readUnattributedAuthorsCache hits when the stored tip equals tip. Detection
// reads the cache before placement is resolved, so there is no repo id yet to
// filter by; the stored RepoID (if any) rides along on the returned entry
// instead.
//
// A transient entry — no Candidates, or Skipped — is served only within
// transientCacheTTL and misses beyond that: candidates come from local refs
// too, so tip-only invalidation would let a brand-new local commit under a
// bad address go unnoticed for as long as the tip doesn't move. An entry
// that did find candidates and got a clean read from the cell has none of
// that risk and keeps the plain until-tip-moves rule.
func readUnattributedAuthorsCache(commonDir, tip string, now time.Time) (cachedDetection, bool) {
	if tip == "" {
		return cachedDetection{}, false
	}
	root, err := unattributedAuthorsCacheRoot(commonDir)
	if err != nil {
		return cachedDetection{}, false
	}
	b, err := osroot.ReadFileNoFollow(root, unattributedAuthorsCacheFile)
	if err != nil {
		return cachedDetection{}, false
	}
	var c cachedDetection
	if json.Unmarshal(b, &c) != nil || c.Tip != tip {
		return cachedDetection{}, false
	}
	if (c.Skipped != "" || len(c.Candidates) == 0) && now.Sub(c.FetchedAt) > transientCacheTTL {
		return cachedDetection{}, false
	}
	return c, true
}

func writeUnattributedAuthorsCache(commonDir string, c cachedDetection) error {
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode unattributed-authors cache: %w", err)
	}
	root, err := unattributedAuthorsCacheRoot(commonDir)
	if err != nil {
		return fmt.Errorf("open %s: %w", commonDir, err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, unattributedAuthorsCacheFile, b, 0o600); err != nil {
		return fmt.Errorf("write unattributed-authors cache: %w", err)
	}
	return nil
}

// invalidateUnattributedAuthorsCache is called after a declare or release so
// status stops showing a count the user just repaired. Absence is not an error.
func invalidateUnattributedAuthorsCache(ctx context.Context, commonDir string) {
	root, err := unattributedAuthorsCacheRoot(commonDir)
	if err != nil {
		logging.Debug(ctx, "unattributed authors: cache invalidate failed", "error", err)
		return
	}
	if err := root.Remove(unattributedAuthorsCacheFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logging.Debug(ctx, "unattributed authors: cache invalidate failed", "error", err)
	}
}

// invalidateUnattributedAuthorsCacheForRepo resolves the current repo's git
// common dir and invalidates its cache entry, absorbing "no repository" the
// same way invalidateUnattributedAuthorsCache absorbs "no cache file" — a
// caller with no git common dir has nothing to invalidate. Shared by
// defaultUnattributedPromptDeps (declare) and defaultReleaseDeps (release) so
// both invalidate identically; matches the releaseDeps/unattributedPromptDeps
// `invalidate func(ctx context.Context)` shape directly, so it is assigned as
// a bare function value rather than wrapped in a closure at either call site.
func invalidateUnattributedAuthorsCacheForRepo(ctx context.Context) {
	if dir, err := strategy.GetGitCommonDir(ctx); err == nil {
		invalidateUnattributedAuthorsCache(ctx, dir)
	}
}

// originDefaultTip is the SHA of origin's default-branch tip, else HEAD; ""
// only when neither resolves. The default-branch chain is the existing one
// (origin/HEAD → origin/main → origin/master, getDefaultBranchFromRemote); a
// repo whose remote default branch is something else (e.g. a `develop`
// default added via `git remote add`) falls back to the current HEAD hash so
// the cache key still moves with the repo instead of being bypassed on every
// call.
func originDefaultTip(ctx context.Context) string {
	repo, err := gitrepo.OpenCurrent(ctx) // caller owns and closes
	if err != nil {
		return ""
	}
	defer repo.Close()
	if branch := getDefaultBranchFromRemote(repo); branch != "" {
		if ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true); err == nil {
			return ref.Hash().String()
		}
	}
	if h, err := repo.Head(); err == nil {
		return h.Hash().String()
	}
	return ""
}
