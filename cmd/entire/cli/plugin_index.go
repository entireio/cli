package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Plugin discovery rides on a git-synced index, krew-style: the index is
// itself a git repository containing index.json, shallow-cloned into the
// user cache and re-pulled on a TTL. No hosted service, no forge REST API —
// any git server can host an index. An organization points the CLI at an
// internal catalog with ENTIRE_PLUGIN_INDEX_URL or --index; see
// resolvePluginIndexURL for why a committed settings file cannot.
const (
	// defaultPluginIndexURL is the built-in curated index.
	defaultPluginIndexURL = "https://github.com/entireio/plugin-index"
	// pluginIndexEnvVar overrides the index URL (forwarded to plugin
	// children automatically via the ENTIRE_ allowlist prefix).
	pluginIndexEnvVar = "ENTIRE_PLUGIN_INDEX_URL"
	// pluginIndexFileName is the catalog file at the index repo root.
	pluginIndexFileName = "index.json"
	// pluginIndexSchemaVersion is the index schema this CLI reads. Advisory:
	// a higher declared version logs a warning and is still read (see
	// loadPluginIndexFromDir).
	pluginIndexSchemaVersion = 1
	// pluginIndexSyncMarker records the last successful sync time as the
	// marker file's mtime. Untracked, so fetch/reset never touches it.
	pluginIndexSyncMarker = ".entire-last-sync"
	// maxPluginIndexSize bounds index.json. krew's catalog carries a few
	// hundred entries; 8 MiB leaves room for far more without letting a hostile
	// index exhaust memory.
	maxPluginIndexSize = 8 << 20
	// pluginIndexTTL is how long a synced copy is considered fresh. Fixed
	// rather than configurable: `plugin index update` already forces a refresh
	// and stale-on-offline already covers a failed one, so a knob here tuned a
	// problem solved twice over while costing a settings load per sync.
	pluginIndexTTL = 24 * time.Hour
)

// PluginIndex is the parsed catalog. Decoding is deliberately lenient
// (unknown fields ignored) so an index that grows new fields doesn't break
// older CLI versions fleet-wide.
type PluginIndex struct {
	// Version is the declared schema version. Advisory only — see
	// loadPluginIndexFromDir for why it isn't enforced.
	Version int                `json:"version"`
	Plugins []PluginIndexEntry `json:"plugins"`
}

// PluginIndexEntry describes one plugin in the catalog.
type PluginIndexEntry struct {
	Name        string `json:"name"`
	RepoURL     string `json:"repo_url"`
	Description string `json:"description,omitempty"`
	Official    bool   `json:"official,omitempty"`
	// Platforms lists supported GOOS values when the plugin doesn't ship
	// the full matrix; empty means all.
	Platforms []string `json:"platforms,omitempty"`
}

// Find returns the entry with the given bare name, or nil.
func (idx *PluginIndex) Find(name string) *PluginIndexEntry {
	if idx == nil {
		return nil
	}
	for i := range idx.Plugins {
		if idx.Plugins[i].Name == name {
			return &idx.Plugins[i]
		}
	}
	return nil
}

// Search returns entries whose name or description contains term
// (case-insensitive). An empty term returns everything.
func (idx *PluginIndex) Search(term string) []PluginIndexEntry {
	if idx == nil {
		return nil
	}
	if term == "" {
		return idx.Plugins
	}
	t := strings.ToLower(term)
	var out []PluginIndexEntry
	for _, e := range idx.Plugins {
		if strings.Contains(strings.ToLower(e.Name), t) || strings.Contains(strings.ToLower(e.Description), t) {
			out = append(out, e)
		}
	}
	return out
}

// FindByRepoURL returns the entry published at repoURL, or nil. Comparison
// normalizes the .git suffix and trailing slashes.
//
// The name matters as much as the presence: a URL install of a listed repo is
// trusted and never prompts, so the catalog entry is the only thing that says
// what the plugin should be called. Without it the remote picks the name
// unchallenged, and --force would replace whichever plugin it named.
func (idx *PluginIndex) FindByRepoURL(repoURL string) *PluginIndexEntry {
	if idx == nil {
		return nil
	}
	want := normalizeRepoURL(repoURL)
	for i := range idx.Plugins {
		if normalizeRepoURL(idx.Plugins[i].RepoURL) == want {
			return &idx.Plugins[i]
		}
	}
	return nil
}

func normalizeRepoURL(u string) string {
	return strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(u), "/"), ".git")
}

// resolvePluginIndexURL applies the documented precedence: --index flag >
// ENTIRE_PLUGIN_INDEX_URL > built-in default.
//
// Repo-level settings are deliberately NOT a source. .entire/settings.json is a
// committed file resolved from the working directory, and an index-listed repo
// installs with no confirmation prompt — so honoring it there meant a cloned
// repository could redirect the catalog and get an attacker-chosen binary
// downloaded, chmod 0755, and linked onto the user's PATH without a prompt.
// The value of that setting was precisely that contributors got a different
// catalog *without knowing*, which is the vulnerability stated as a feature; it
// cannot be kept and made safe, only removed or made visible.
//
// Organizations wanting an internal catalog set ENTIRE_PLUGIN_INDEX_URL, which
// applies across repos rather than per-repo, and cannot be chosen by content
// the user merely checked out. This matches Go, where no repo-committed file
// can redirect GOPROXY; npm's per-project registry override is the
// cautionary counter-example.
func resolvePluginIndexURL(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv(pluginIndexEnvVar); v != "" {
		return v
	}
	return defaultPluginIndexURL
}

// pluginIndexCacheDir is the per-URL local copy. Keyed by a hash of the
// URL so switching indexes (or per-repo overrides) never serves one
// catalog's cache for another.
func pluginIndexCacheDir(indexURL string) (string, error) {
	cache := userdirs.Cache()
	if cache == "" {
		return "", errors.New("cannot resolve user cache directory")
	}
	sum := sha256.Sum256([]byte(normalizeRepoURL(indexURL)))
	return filepath.Join(cache, "plugin-index", hex.EncodeToString(sum[:6])), nil
}

// SyncPluginIndex returns the catalog for indexURL, cloning or refreshing
// the local copy as needed. force bypasses the TTL. When a refresh fails
// but a previous copy exists, the stale copy is used with a warning logged
// — discovery shouldn't hard-fail because a laptop is offline.
func SyncPluginIndex(ctx context.Context, indexURL string, force bool) (*PluginIndex, error) {
	// The index URL reaches `git clone` as a positional; it can come from
	// --index or ENTIRE_PLUGIN_INDEX_URL, neither of which the settings
	// validator sees. Reject non-URLs before git can read one as an option.
	if err := validatePluginRepoURL(indexURL); err != nil {
		return nil, fmt.Errorf("plugin index URL: %w", err)
	}
	dir, err := pluginIndexCacheDir(indexURL)
	if err != nil {
		return nil, err
	}

	// The cache dir is shared by every concurrent `entire plugin` invocation
	// and syncing it is destructive: RemoveAll + clone on the cold path,
	// fetch + `reset --hard FETCH_HEAD` on the warm one. Serialize the whole
	// sync-and-read under an advisory lock, the same way discovery/cache.go
	// guards its shared cache files. Without it, one process cloning while
	// another reads index.json surfaces a spurious "no readable index.json",
	// and two cold syncs racing can leave exactly the half-created directory
	// the sweep below exists to recover from.
	//
	// The lock file is a *sibling* of the cache dir, not inside it: RemoveAll
	// on the dir would otherwise delete the lock file while we hold it.
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return nil, fmt.Errorf("create index cache dir: %w", err)
	}
	release, err := flock.AcquireContext(ctx, dir+".lock")
	if err != nil {
		return nil, fmt.Errorf("lock plugin index cache: %w", err)
	}
	defer release()

	marker := filepath.Join(dir, pluginIndexSyncMarker)
	// Lstat, not Stat: this asks whether *our* cache dir contains a clone.
	// Following a symlink would let a link pointing elsewhere answer yes.
	_, statErr := os.Lstat(filepath.Join(dir, ".git"))
	cloned := statErr == nil

	fresh := false
	if cloned && !force {
		if info, err := os.Lstat(marker); err == nil {
			// A negative age means the marker is dated in the future (clock
			// rollback, restored backup, bad container clock). Treating that as
			// fresh would freeze the catalog until someone ran `index update`
			// by hand, so only a non-negative age inside the window counts.
			if age := time.Since(info.ModTime()); age >= 0 && age < pluginIndexTTL {
				fresh = true
			}
		}
	}

	switch {
	case !cloned:
		// A previously interrupted clone can leave a partial directory
		// without .git. git refuses to clone into a non-empty target, so
		// without this sweep, discovery would stay wedged until the user
		// cleared the cache by hand.
		if err := os.RemoveAll(dir); err != nil {
			return nil, fmt.Errorf("clear stale index cache: %w", err)
		}
		if err := runGitDiscard(ctx, "clone", "--depth", "1", "--quiet", "--", indexURL, dir); err != nil {
			return nil, fmt.Errorf("clone plugin index %s: %w", redactURL(indexURL), err)
		}
		touchFile(marker)
	case !fresh:
		if err := refreshPluginIndexClone(ctx, dir); err != nil {
			logging.Warn(ctx, "plugin index refresh failed; using cached copy",
				slog.String("index", redactURL(indexURL)), slog.String("error", err.Error()))
		} else {
			touchFile(marker)
		}
	}

	return loadPluginIndexFromDir(ctx, dir, indexURL)
}

// refreshPluginIndexClone updates an existing shallow clone to the remote
// tip regardless of the remote's default branch name.
func refreshPluginIndexClone(ctx context.Context, dir string) error {
	if err := runGitDiscard(ctx, "-C", dir, "fetch", "--depth", "1", "--quiet", "origin", "HEAD"); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if err := runGitDiscard(ctx, "-C", dir, "reset", "--hard", "--quiet", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return nil
}

func touchFile(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return
	}
	if f, err := os.Create(path); err == nil { //nolint:gosec // marker file inside our cache dir
		_ = f.Close()
	}
}

func loadPluginIndexFromDir(ctx context.Context, dir, indexURL string) (*PluginIndex, error) {
	// Bounded: the catalog is cloned from a remote repository, so its size is
	// not ours to trust. A few hundred entries with descriptions is well under
	// a megabyte.
	data, err := readFileLimited(filepath.Join(dir, pluginIndexFileName), maxPluginIndexSize)
	if err != nil {
		return nil, fmt.Errorf("plugin index %s has no readable %s: %w", redactURL(indexURL), pluginIndexFileName, err)
	}
	var idx PluginIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse %s from %s: %w", pluginIndexFileName, redactURL(indexURL), err)
	}
	// version is recorded, not enforced. Refusing an unrecognized value would
	// protect against a migration that can never happen: the index is one
	// shared resource read by every CLI version ever shipped, so bumping it
	// would break discovery fleet-wide with no gradual rollout and no undo for
	// installed binaries. An incompatible schema therefore ships at a new path
	// (index-v2.json, another branch, another repo), which needs no gate here.
	// Enforcing it only ever fired by accident — most painfully on an index
	// that simply omits the field (version 0), which told the author to
	// upgrade the CLI when the fix was in their own file. Hand-written
	// internal catalogs are a first-class case (ENTIRE_PLUGIN_INDEX_URL).
	//
	// Additive changes, the ones that actually happen, are already absorbed:
	// decoding ignores unknown fields and unreadable entries are dropped
	// below. Degrading per entry beats refusing the catalog.
	if idx.Version > pluginIndexSchemaVersion {
		logging.Warn(ctx, "plugin index declares a newer schema version; reading what this CLI understands",
			slog.String("index", redactURL(indexURL)),
			slog.Int("declared_version", idx.Version),
			slog.Int("understood_version", pluginIndexSchemaVersion))
	}
	var valid []PluginIndexEntry
	for _, e := range idx.Plugins {
		// repo_url is validated here, not just at the git boundary: an
		// index-resolved install is treated as trusted and never prompts, so
		// a hostile catalog entry must not reach the git CLI at all.
		if validatePluginName(e.Name) != nil || validatePluginRepoURL(e.RepoURL) != nil {
			continue // tolerate bad entries rather than failing the catalog
		}
		// description has no validator of its own — it is free text, and the
		// only thing that can go wrong with it is what it does to a terminal.
		// Checked here so search, info, and the browse picker can print it
		// without each remembering to.
		if hasTerminalControlChars(e.Description) {
			continue
		}
		valid = append(valid, e)
	}
	idx.Plugins = valid
	return &idx, nil
}
