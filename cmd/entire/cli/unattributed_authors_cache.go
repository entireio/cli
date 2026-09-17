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
)

// Cache — <git common dir>/entire-unattributed-authors.json, ONE entry. A
// successful outcome is valid while the origin default-branch tip, else HEAD,
// is unchanged; a skipped outcome for skippedCacheTTL, so a failing network
// call is not re-paid by every `entire status`. Best-effort: any I/O or decode
// error is a miss.
const (
	unattributedAuthorsCacheFile = "entire-unattributed-authors.json"
	skippedCacheTTL              = 10 * time.Minute
)

// cachedDetection is the on-disk cache entry. A placement-failed entry
// (RepoID == "") is served, Skipped and all, for skippedCacheTTL and
// short-circuits placement resolution until then — that repeated skip is
// intended, not a bug: it is what keeps a down cell from being re-dialed on
// every `entire status`. `entire login` does not clear it on its own; a caller
// that wants the next detection to try the network again (doctor's fix path,
// a future `entire login` hook) must call invalidateUnattributedAuthorsCache.
type cachedDetection struct {
	Tip       string               `json:"tip"`
	RepoID    string               `json:"repo_id,omitempty"` // empty when placement failed
	Authors   []unattributedAuthor `json:"authors,omitempty"`
	Skipped   string               `json:"skipped,omitempty"`
	FetchedAt time.Time            `json:"fetched_at"`
}

// unattributedAuthorsCacheRoot opens the shared *os.Root over commonDir, the
// same way the writer and reader must agree on. gitdir.OpenAt applies
// filepath.Abs itself, so this works whether or not strategy.GetGitCommonDir
// returned a relative path (its own Clean/Join does not guarantee absolute).
func unattributedAuthorsCacheRoot(commonDir string) (*os.Root, error) {
	return gitdir.OpenAt(commonDir) //nolint:wrapcheck // gitdir already names the directory and the failure
}

// readUnattributedAuthorsCache hits when the stored tip equals tip and, if
// repoID is non-empty, the stored repo id equals repoID. repoID "" accepts
// whatever id is stored: detection reads the cache before it has resolved
// placement, and the stored id is what it needs back.
func readUnattributedAuthorsCache(commonDir, tip, repoID string, now time.Time) (cachedDetection, bool) {
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
	if repoID != "" && c.RepoID != repoID {
		return cachedDetection{}, false
	}
	if c.Skipped != "" && now.Sub(c.FetchedAt) > skippedCacheTTL {
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
