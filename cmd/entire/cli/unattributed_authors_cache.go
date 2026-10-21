package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// Cache — <git common dir>/entire-unattributed-authors.json, ONE entry, keyed
// by unattributedAuthorsCacheKey: the login, the OS username, and every ref
// the shortlog walk covers. A successful outcome with candidates is served
// for successCacheTTL; a no-candidates or skipped outcome only for
// transientCacheTTL, so neither a clean history nor a failing network call is
// re-paid (a `git shortlog` walk, or a cell round trip) by every
// `entire status`. Best-effort: any I/O or decode error is a miss.
const (
	unattributedAuthorsCacheFile = "entire-unattributed-authors.json"
	// transientCacheTTL bounds how long a no-candidates or skipped outcome is
	// served before detection tries again: a later login or a recovered cell
	// should be noticed without waiting for a ref to move.
	transientCacheTTL = 10 * time.Minute
	// successCacheTTL bounds a result that found candidates. Linking an
	// address is account-wide and can happen in another repo or on the web,
	// which no local key change reflects, so status re-checks hourly.
	successCacheTTL = time.Hour
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
// `git shortlog`: detection reads the cache by key before it computes
// Candidates, and a hit returns the stored list in its place. An entry with
// no Candidates (logged-in, clean history) is written and read back the same
// way, but — like a Skipped entry — only within transientCacheTTL; see
// readUnattributedAuthorsCache.
type cachedDetection struct {
	Key        string               `json:"key"`
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

// readUnattributedAuthorsCache hits when the stored key equals key. Detection
// reads the cache before placement is resolved, so there is no repo id yet to
// filter by; the stored RepoID (if any) rides along on the returned entry
// instead. A transient entry — no Candidates, or Skipped — misses once older
// than transientCacheTTL, any other entry once older than successCacheTTL.
func readUnattributedAuthorsCache(commonDir, key string, now time.Time) (cachedDetection, bool) {
	if key == "" {
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
	if json.Unmarshal(b, &c) != nil || c.Key != key {
		return cachedDetection{}, false
	}
	ttl := successCacheTTL
	if c.Skipped != "" || len(c.Candidates) == 0 {
		ttl = transientCacheTTL
	}
	if now.Sub(c.FetchedAt) > ttl {
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

// unattributedAuthorsCacheKey hashes everything a cached result depends on
// locally: who is logged in (a different account or core sees different
// counts and repo id), the OS username (it gates the candidates), and refs,
// the state of every ref the shortlog walk covers (a new commit on any
// branch can add a candidate). "" when refs is "", meaning no caching.
func unattributedAuthorsCacheKey(identity, username, refs string) string {
	if refs == "" {
		return ""
	}
	h := sha256.New()
	for _, part := range []string{identity, username, refs} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// localRefsState lists HEAD and every ref localCandidateAuthors walks, with
// their hashes, as one string; "" when git fails or the repo has no refs.
// Entire's own refs are left out for the same reason shortlog excludes them,
// and so checkpoint writes do not throw the cache away.
func localRefsState(ctx context.Context) string {
	out, err := runGitQuiet(ctx, 16<<20, "show-ref", "--head")
	if err != nil {
		logging.Debug(ctx, "unattributed authors: git show-ref failed; cache disabled", "error", err)
		return ""
	}
	var b strings.Builder
	for line := range strings.Lines(string(out)) {
		_, name, _ := strings.Cut(strings.TrimSpace(line), " ")
		if isEntireOwnedRef(name) {
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}

// isEntireOwnedRef matches the --exclude patterns in localCandidateAuthors.
func isEntireOwnedRef(name string) bool {
	if strings.HasPrefix(name, "refs/entire/") || strings.HasPrefix(name, "refs/heads/entire/") {
		return true
	}
	if rest, ok := strings.CutPrefix(name, "refs/remotes/"); ok {
		_, branch, _ := strings.Cut(rest, "/")
		return strings.HasPrefix(branch, "entire/")
	}
	return false
}
