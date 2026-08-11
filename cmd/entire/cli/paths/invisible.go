package paths

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Invisible-mode routing.
//
// A repo tracked only by the user-global settings tier (no
// .entire/settings.json or .entire/settings.local.json in the worktree,
// global.enabled true in the user settings file) must never gain files in the
// worktree — that is the product guarantee of global tracking. AbsPath
// therefore reroutes the runtime-data directories below from
// <worktree>/.entire/<sub> to <git-common-dir>/entire/worktree/<sub> for such
// repos. Any repo-level setup pins every path to the worktree, byte-identical
// to the historical behavior.

// invisibleRuntimeSubdir is the directory inside the git common dir that
// holds rerouted runtime data. It sits next to entire/preferences.json.
const invisibleRuntimeSubdir = "entire/worktree"

// settingsLocalFileName mirrors settings.EntireSettingsLocalFile's basename.
// The settings package imports paths, so the constant cannot be shared.
const settingsLocalFileName = "settings.local.json"

// runtimeDataPrefixes are the .entire subtrees that hold runtime data (as
// opposed to configuration). Only these are ever rerouted: settings files,
// .entire/.gitignore, and redactor packs must stay worktree-resolved — the
// settings files' worktree presence is itself the routing discriminator.
var runtimeDataPrefixes = []string{EntireMetadataDir, EntireLogsDir, EntireTmpDir}

// runtimeDataSubpath reports whether relPath addresses runtime data and, if
// so, returns its path relative to the .entire directory (slash-separated,
// e.g. "metadata/<session>/prompt.txt").
func runtimeDataSubpath(relPath string) (string, bool) {
	rel := filepath.ToSlash(relPath)
	for _, prefix := range runtimeDataPrefixes {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return strings.TrimPrefix(rel, EntireDir+"/"), true
		}
	}
	return "", false
}

// invisibleBaseCache caches the routing decision per worktree root. Hooks are
// one-shot processes, so one probe per process is enough; the root key keeps
// in-process tests with multiple temp repos isolated.
var (
	invisibleMu        sync.Mutex
	invisibleCacheRoot string
	invisibleCacheBase string
)

// ClearInvisibleRuntimeCache clears the cached invisible-routing decision.
// Primarily useful for tests that change global settings for one repo root.
func ClearInvisibleRuntimeCache() {
	invisibleMu.Lock()
	invisibleCacheRoot = ""
	invisibleCacheBase = ""
	invisibleMu.Unlock()
}

// invisibleRuntimeBase returns the absolute directory runtime data resolves
// under for the repo rooted at root (the caller's already-resolved worktree
// root), or "" when it lives in the worktree (repo-level setup present,
// global tier off, or any probe failure — every error path keeps today's
// worktree behavior).
func invisibleRuntimeBase(ctx context.Context, root string) string {
	invisibleMu.Lock()
	defer invisibleMu.Unlock()
	if invisibleCacheRoot == root {
		return invisibleCacheBase
	}
	base := computeInvisibleRuntimeBase(ctx, root)
	invisibleCacheRoot = root
	invisibleCacheBase = base
	return base
}

func computeInvisibleRuntimeBase(ctx context.Context, root string) string {
	// Any repo-level setup — either settings scope — pins .entire to the
	// worktree. Lstat (not Stat) matches settings.IsSetUpAny: a dangling
	// symlink still counts as setup.
	for _, name := range []string{SettingsFileName, settingsLocalFileName} {
		if _, err := os.Lstat(filepath.Join(root, EntireDir, name)); err == nil {
			return ""
		}
	}
	if !userGlobalTierEnabled() {
		return ""
	}
	commonDir, err := gitCommonDir(ctx)
	if err != nil {
		return ""
	}
	return filepath.Join(commonDir, filepath.FromSlash(invisibleRuntimeSubdir))
}

// userGlobalTierEnabled reports whether the user-global settings file enables
// global mode. This is a minimal mirror of settings.LoadUserSettings +
// GlobalModeActive's enabled bit — paths cannot import settings (settings
// imports paths), and the full gate (strict decoding, exclude lists) stays in
// settings where hooks consult it.
//
// Divergence from the strict gate is deliberately one-sided: lenient
// decoding and no exclude lists make this probe a strict superset of the
// strict gate, so divergence can only route reads to an empty .git-side
// location, never produce worktree writes.
func userGlobalTierEnabled() bool {
	// settings.json inside userdirs.Config() = settings.UserSettingsFileName.
	data, err := os.ReadFile(filepath.Join(userdirs.Config(), "settings.json"))
	if err != nil {
		return false
	}
	var us struct {
		Global *struct {
			Enabled bool `json:"enabled"`
		} `json:"global"`
	}
	if json.Unmarshal(data, &us) != nil {
		return false
	}
	return us.Global != nil && us.Global.Enabled
}

// gitCommonDir returns the absolute git common dir for the current working
// directory. Corrected copy of session.GetGitCommonDir — importing session
// (or strategy) here would cycle back into paths via logging — with two
// deliberate differences: a relative `git rev-parse --git-common-dir` result
// is absolutized via filepath.Abs (session's filepath.Join(".", dir) leaves
// it relative), and there is no per-cwd cache (invisibleRuntimeBase already
// caches the final routing decision per worktree root). Do not unify this
// back onto session's weaker behavior.
func gitCommonDir(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", err //nolint:wrapcheck // internal helper; callers only branch on failure
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		abs, absErr := filepath.Abs(dir)
		if absErr != nil {
			return "", absErr //nolint:wrapcheck // internal helper; callers only branch on failure
		}
		dir = abs
	}
	return filepath.Clean(dir), nil
}
