package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"golang.org/x/mod/semver"
)

// This file mirrors versioncheck.CheckAndNotify for installed plugins: a
// post-run notice, a 24h cache, and silence on every error path. The latest
// version comes from the same tag resolution `entire plugin upgrade` uses, so
// the notice can never name a version the upgrade would not install.

const (
	// pluginCheckInterval mirrors versioncheck's checkInterval.
	pluginCheckInterval = 24 * time.Hour

	// pluginCheckTimeout mirrors versioncheck's httpTimeout: the check runs
	// synchronously after the plugin has exited, so the remote tag listing
	// gets the same 2s budget the CLI's own post-run check lives within,
	// not the much larger interactive-install git timeout.
	pluginCheckTimeout = 2 * time.Second

	// pluginCacheFileName sits beside versioncheck's version_check.json in
	// the global config directory.
	pluginCacheFileName = "plugin_version_check.json"
)

// pluginVersionCache is the plugin analog of versioncheck.VersionCache: the
// last check time per plugin name. No SkippedVersion: there is no interactive
// prompt, so there is no "skip this version" choice to remember.
type pluginVersionCache map[string]time.Time

// listPluginTagsForUpdateCheck is a test seam over the tag resolution
// `entire plugin upgrade` uses.
var listPluginTagsForUpdateCheck = listRemoteSemverTags

// checkPluginUpdateAndNotify prints an update notice for pluginName when a
// newer stable tag than the installed one exists. Silent on all errors to
// avoid interrupting plugin runs, mirroring versioncheck.CheckAndNotify.
//
// Plugins without an install manifest (local-dev symlinks, binaries dropped
// on PATH) and pinned installs are skipped — the same installs
// `entire plugin upgrade` skips.
func checkPluginUpdateAndNotify(ctx context.Context, w io.Writer, pluginName string) {
	cache := loadPluginVersionCache()
	if time.Since(cache[pluginName]) < pluginCheckInterval {
		return
	}

	m, err := LoadPluginManifest(pluginName)
	if err != nil || m == nil {
		return
	}
	if m.Pinned {
		return
	}

	// Stamp the cache BEFORE the network call, where versioncheck stamps
	// after. This path differs for two reasons: a config dir that cannot be
	// written must cost zero network — versioncheck gets that from its
	// ensureGlobalConfigDir gate, here the stamp write itself is the probe —
	// and plugin runs arrive in parallel from agent hooks, so the early
	// claim shrinks the window in which concurrent runs each start their
	// own ls-remote from the fetch's duration to the gap between gate and
	// write. The file stays lock-free last-writer-wins, like versioncheck's:
	// a stamp lost to an interleaved writer costs one extra bounded lookup,
	// which is not worth a lock.
	cache[pluginName] = time.Now()
	if err := savePluginVersionCache(cache); err != nil {
		logging.Debug(ctx, "plugin version check: failed to save cache",
			"plugin", pluginName, "error", err.Error())
		return
	}

	tagCtx, cancel := context.WithTimeout(ctx, pluginCheckTimeout)
	tags, err := listPluginTagsForUpdateCheck(tagCtx, m.RepoURL)
	cancel()
	if err != nil {
		logging.Debug(ctx, "plugin version check: failed to list tags",
			"plugin", pluginName, "error", err.Error())
		return
	}
	if len(tags) == 0 {
		return
	}

	// Same comparison UpgradeInstalledPlugin makes before reinstalling.
	latest := tags[0]
	if semver.Compare(canonicalSemver(latest), canonicalSemver(m.Tag)) <= 0 {
		return
	}

	fmt.Fprintf(w, "\nPlugin update available! %s %s -> %s\nTo update, run:\n  entire plugin upgrade %s\n",
		pluginName, strings.TrimPrefix(m.Tag, "v"), strings.TrimPrefix(latest, "v"), pluginName)
}

// loadPluginVersionCache returns the cached per-plugin check times, or an
// empty cache when the file is missing or unreadable — mirroring how
// versioncheck treats its cache file.
func loadPluginVersionCache() pluginVersionCache {
	root, err := userdirs.ConfigRootForRead()
	if err != nil {
		return pluginVersionCache{}
	}
	data, err := osroot.ReadFileNoFollow(root, pluginCacheFileName)
	if err != nil {
		return pluginVersionCache{}
	}
	var cache pluginVersionCache
	if err := json.Unmarshal(data, &cache); err != nil || cache == nil {
		return pluginVersionCache{}
	}
	return cache
}

// savePluginVersionCache writes the cache atomically. Going through
// userdirs.ConfigRoot also creates the config directory with private
// permissions when this runs before first login.
func savePluginVersionCache(cache pluginVersionCache) error {
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling cache: %w", err)
	}
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return fmt.Errorf("opening config directory: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, pluginCacheFileName, data, 0o600); err != nil {
		return fmt.Errorf("writing cache: %w", err)
	}
	return nil
}
