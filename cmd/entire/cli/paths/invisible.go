package paths

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// ErrUnroutableRuntimePath marks the routing failure state: the user-global
// tier owns this repo (no repo-level settings, global.enabled true) but the
// git-side runtime location could not be resolved (git common dir probe or
// worktree-ID classification failed). Direction for every consumer: for
// tier-owned repos, unroutable means SKIP the runtime write/read — never fall
// back to the worktree (which would violate the invisibility guarantee) and
// never fail the user's operation. Skips are logged at the highest level that
// is live at the site: Warn where hook logging is already initialized (e.g.
// the turn-end capture), Debug or silent where it is not.
// AbsPath surfaces this (wrapped) for runtime-data paths; callers detect it
// with IsUnroutableRuntimePath.
var ErrUnroutableRuntimePath = errors.New("global tier owns this repo but its git-side runtime location could not be resolved")

// IsUnroutableRuntimePath reports whether err carries ErrUnroutableRuntimePath.
func IsUnroutableRuntimePath(err error) bool {
	return errors.Is(err, ErrUnroutableRuntimePath)
}

// invisibleProbeFailForTesting forces the routing probe to fail for repos the
// global tier owns. The real failure modes (git exec failure, an unreadable
// .git file) cannot be produced portably on disk while git itself keeps
// working, so tests inject the failure here.
var invisibleProbeFailForTesting bool

// SetInvisibleProbeFailureForTesting toggles a forced probe failure in
// computeInvisibleRuntimeBase (see invisibleProbeFailForTesting). Callers must
// also ClearInvisibleRuntimeCache and reset to false when done.
func SetInvisibleProbeFailureForTesting(fail bool) {
	invisibleMu.Lock()
	invisibleProbeFailForTesting = fail
	invisibleMu.Unlock()
}

// Invisible-mode routing.
//
// A repo tracked only by the user-global settings tier (no explicit enabled
// key in either repo settings file, global.enabled true in the user settings
// file) must never gain runtime files in the worktree — that is the product
// guarantee of global tracking. AbsPath
// therefore reroutes the runtime-data directories below from
// <worktree>/.entire/<sub> to
// <git-common-dir>/entire/worktree/<worktree-key>/<sub> for such repos. The
// worktree key namespaces the shared common dir per worktree (linked
// worktrees of one clone share the common dir but must not interleave
// runtime data); it is the same worktree-ID hash shadow branch names use.
// Explicit repo-level activation pins every path to the worktree,
// byte-identical to the historical behavior. Settings created by unrelated
// features do not change routing.

// invisibleRuntimeSubdir is the directory inside the git common dir that
// holds rerouted runtime data, one <worktree-key> namespace per worktree.
// It sits next to entire/preferences.json.
const invisibleRuntimeSubdir = "entire/worktree"

// InvisibleRuntimeDir returns the directory invisible routing resolves
// runtime data under for the worktree rooted at root, given the repo's
// git common dir: <commonDir>/entire/worktree/<worktree-key>.
// Precondition: commonDir must be absolute — the result is joined onto it
// verbatim, so a relative commonDir would silently produce a cwd-relative
// runtime dir.
// The key is HashWorktreeID over the worktree's git identifier ("" for the
// main worktree), so every worktree of a clone gets its own namespace.
// This is the single source of truth for the layout — the enable-time
// migration (setup_global.go, arriving with the enable --global layer) uses
// it to move only the current worktree's namespace, and
// InvisibleRuntimeSubdirs enumerates the subtrees below it.
func InvisibleRuntimeDir(commonDir, root string) (string, error) {
	worktreeID, err := GetWorktreeID(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, filepath.FromSlash(invisibleRuntimeSubdir), HashWorktreeID(worktreeID)), nil
}

// settingsLocalFileName mirrors settings.EntireSettingsLocalFile's basename.
// The settings package imports paths, so the constant cannot be shared.
const settingsLocalFileName = "settings.local.json"

// runtimeDataPrefixes are the .entire subtrees that hold runtime data (as
// opposed to configuration). Only these are ever rerouted: settings files,
// .entire/.gitignore, and redactor packs must stay worktree-resolved — the
// settings files' worktree presence is itself the routing discriminator —
// and .entire/worktrees (trail checkout worktrees) stays too: those are
// working trees the user deliberately checks out and cds into, so hiding
// them under .git would defeat their purpose, and a repo with trail
// worktrees has repo-level setup anyway.
var runtimeDataPrefixes = []string{EntireMetadataDir, EntireLogsDir, EntireTmpDir}

// InvisibleRuntimeSubdirs returns the subtree names invisible routing places
// under InvisibleRuntimeDir — the layout's single source of truth, derived
// from runtimeDataPrefixes ("metadata", "logs", "tmp"). The enable-time
// migration iterates exactly these when moving a namespace back into the
// worktree.
func InvisibleRuntimeSubdirs() []string {
	subs := make([]string, len(runtimeDataPrefixes))
	for i, prefix := range runtimeDataPrefixes {
		subs[i] = strings.TrimPrefix(prefix, EntireDir+"/")
	}
	return subs
}

// runtimeDataSubpath reports whether relPath addresses runtime data and, if
// so, returns its path relative to the .entire directory (slash-separated,
// e.g. "metadata/<session>/prompt.txt"). It assumes every entry in
// runtimeDataPrefixes lives directly under EntireDir — the TrimPrefix below
// strips exactly that one leading component, so a prefix outside .entire (or
// nested deeper) would produce a wrong subpath rather than fail loudly.
func runtimeDataSubpath(relPath string) (string, bool) {
	rel := filepath.ToSlash(relPath)
	for _, prefix := range runtimeDataPrefixes {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return strings.TrimPrefix(rel, EntireDir+"/"), true
		}
	}
	return "", false
}

// invisibleCache caches the routing decision per worktree root. The root key
// keeps in-process tests with multiple temp repos isolated. A long-lived
// process can therefore hold a stale decision after a discriminator file
// (.entire/settings*.json, the user-global settings file) changes; that
// staleness is accepted for hook processes, which are short-lived, and
// production code paths that write or delete a discriminator file must call
// ClearInvisibleRuntimeCache — that is the invalidation contract. The repo
// settings save paths (settings.saveToFile, settings.saveRaw) honor it after
// every write; flows that delete discriminator files or write the user-global
// settings land upstack and clear at their own sites.
var (
	invisibleMu    sync.Mutex
	invisibleCache struct {
		valid bool // distinguishes the cleared state from a cached root=="" decision
		root  string
		base  string
		err   error
	}
)

// ClearInvisibleRuntimeCache clears the cached invisible-routing decision.
// Called by discriminator-file writers so a process observes its own write
// (see the invalidation contract on invisibleCache above); also used by
// tests that change global settings for one repo root.
func ClearInvisibleRuntimeCache() {
	invisibleMu.Lock()
	invisibleCache.valid = false
	invisibleCache.root = ""
	invisibleCache.base = ""
	invisibleCache.err = nil
	invisibleMu.Unlock()
}

// invisibleRuntimeBase returns the absolute directory runtime data resolves
// under for the repo rooted at root (the caller's already-resolved worktree
// root). "" with a nil error means runtime data lives in the worktree
// (repo-level setup present, or global tier off). A non-nil error — carrying
// ErrUnroutableRuntimePath, or the context's own cancellation — means the
// global tier owns the repo but the git-side location could not be resolved;
// callers must treat that as "skip", never as "use the worktree".
func invisibleRuntimeBase(ctx context.Context, root string) (string, error) {
	invisibleMu.Lock()
	defer invisibleMu.Unlock()
	if invisibleCache.valid && invisibleCache.root == root {
		// A cached ROUTING error must not mask the caller's own cancellation.
		// A cached base is still served: the memo read needs no I/O, and
		// teardown paths may resolve paths under an already-canceled context.
		if invisibleCache.err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", fmt.Errorf("resolving runtime base: %w", ctxErr)
			}
		}
		return invisibleCache.base, invisibleCache.err
	}
	base, err := computeInvisibleRuntimeBase(ctx, root)
	if ctx.Err() != nil {
		// Never cache a cancellation-tainted result: the next caller arrives
		// with a live context and must recompute.
		return base, err
	}
	invisibleCache.valid = true
	invisibleCache.root = root
	invisibleCache.base = base
	invisibleCache.err = err
	return base, err
}

// computeInvisibleRuntimeBase decides where runtime data lives for the repo
// rooted at root. Precondition: the process's current working directory is
// inside that repo — the git-common-dir probe below runs in the cwd, so a
// mismatched cwd would resolve another repo's common dir.
//
// Failure direction: once the global tier is known to own the repo (no
// explicit repo activation, tier enabled), a probe failure returns
// ErrUnroutableRuntimePath instead of "" — for tier-owned repos, unroutable
// means skip, never worktree.
func computeInvisibleRuntimeBase(ctx context.Context, root string) (string, error) {
	if repoActivationConfigured(ctx, root) {
		return "", nil
	}
	// Explicit repo activation above is authoritative even on a canceled
	// context. If the decision was not explicit, stop before consulting the
	// user tier or running another git probe after local-settings ownership
	// detection observed cancellation.
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("classifying repo activation: %w", err)
	}
	if !userGlobalTierEnabled() {
		return "", nil
	}
	if invisibleProbeFailForTesting {
		return "", fmt.Errorf("%w: forced probe failure (test seam)", ErrUnroutableRuntimePath)
	}
	commonDir, err := gitCommonDir(ctx)
	if err != nil {
		// A canceled context fails this probe too; propagate the cancellation
		// instead of disguising it as an unroutable path, which callers treat
		// as a silent skip.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("resolving git common dir: %w", ctxErr)
		}
		return "", fmt.Errorf("%w: resolving git common dir: %w", ErrUnroutableRuntimePath, err)
	}
	base, err := InvisibleRuntimeDir(commonDir, root)
	if err != nil {
		return "", fmt.Errorf("%w: classifying worktree: %w", ErrUnroutableRuntimePath, err)
	}
	return base, nil
}

// repoActivationConfigured mirrors settings.RepoActivationConfigured at the
// path-routing layer. paths cannot import settings because settings imports
// paths. Only a top-level enabled key records repo-local activation intent;
// incidental settings files keep runtime data on the invisible git-side
// route. Unreadable or malformed files are ignored here deliberately: the
// strict settings gate fails closed, while this lower-level probe must fail
// toward invisibility and never create worktree runtime data.
func repoActivationConfigured(ctx context.Context, root string) bool {
	for _, name := range []string{SettingsFileName, settingsLocalFileName} {
		data, err := os.ReadFile(filepath.Join(root, EntireDir, name)) //nolint:gosec // entire-join-ok: this read decides whether invisible routing applies; root is the resolved worktree and names are fixed
		if err != nil {
			continue
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal(data, &raw) != nil {
			continue
		}
		if !hasValidActivationKey(raw) {
			continue
		}
		if name == settingsLocalFileName {
			tracked, err := localSettingsTracked(ctx, root)
			if err != nil {
				return false
			}
			if tracked {
				continue
			}
		}
		return true
	}
	return false
}

func hasValidActivationKey(raw map[string]json.RawMessage) bool {
	v, ok := raw["enabled"]
	if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return false
	}
	var enabled bool
	return json.Unmarshal(v, &enabled) == nil
}

// localSettingsTracked mirrors the settings loader's index-level ownership
// check without importing settings (which would create a package cycle).
// Any verification error is handled by the caller as "not configured", the
// fail-toward-invisibility direction for this routing layer.
func localSettingsTracked(ctx context.Context, root string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "--error-unmatch", "--", filepath.ToSlash(filepath.Join(EntireDir, settingsLocalFileName)))
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("checking local settings ownership: %w", err)
}

// userGlobalTierEnabled reports whether the user-global settings file enables
// global mode. This is a minimal mirror of settings.LoadUserSettings +
// GlobalModeActive's enabled bit — paths cannot import settings (settings
// imports paths), and the full gate (strict decoding, exclude lists) stays in
// settings where hooks consult it.
//
// Divergence from the strict gate is deliberately one-sided: lenient
// decoding and no exclude lists make this probe a strict superset of the
// strict gate, so divergence can only route runtime I/O to an empty
// .git-side location, never produce worktree writes.
func userGlobalTierEnabled() bool {
	configDir, err := userdirs.ResolveConfig()
	if err != nil {
		return false
	}
	// settings.json inside userdirs.ResolveConfig() = settings.UserSettingsFileName.
	data, err := os.ReadFile(filepath.Join(configDir, "settings.json")) //nolint:gosec // configDir is resolved by the userdirs trust boundary
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
// directory. It cannot reuse session.GetGitCommonDir — importing session (or
// strategy) here would cycle back into paths via logging. session's copy now
// absolutizes the same way (its filepath.Join(".", dir) bug was fixed to
// filepath.Abs); the two remain separate only for that import-cycle reason
// plus cache shape: session caches per cwd, while here invisibleRuntimeBase
// already caches the final routing decision per worktree root.
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
