package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// HookDiscoveryState distinguishes a resolved Codex hook source from a layout
// whose behavior Entire cannot safely infer.
type HookDiscoveryState uint8

const (
	HookDiscoveryUnresolved HookDiscoveryState = iota
	HookDiscoveryResolved
)

// DiscoveredHooksPath identifies the read-only project hooks file Codex is
// expected to load. It is intentionally distinct from WorktreeHooksPath.
type DiscoveredHooksPath struct {
	path string
}

// Path returns the discovered hooks file's absolute path.
func (p DiscoveredHooksPath) Path() string {
	return p.path
}

// HookDiscovery describes the hooks file Codex is expected to discover. It is
// diagnostic-only and carries no write target, migration path, or lock path.
type HookDiscovery struct {
	State           HookDiscoveryState
	DiscoveredHooks DiscoveredHooksPath
	Diagnostic      error
	worktreeRoot    string
}

// ProjectLayerExists reports whether the current checkout has a valid local
// .codex directory Codex can use to construct its project config layer.
func (d HookDiscovery) ProjectLayerExists() bool {
	return d.worktreeRoot != "" && projectLayerExists(filepath.Join(d.worktreeRoot, ".codex"))
}

// UnresolvedHookDiscoveryError explains why Entire will not guess which hook
// file Codex loads for a Git layout.
type UnresolvedHookDiscoveryError struct {
	Reason string
}

func (e *UnresolvedHookDiscoveryError) Error() string {
	return "Codex hook discovery is unresolved: " + e.Reason
}

// ResolveHookDiscovery performs read-only discovery of the hook file Codex is
// expected to load for the current checkout.
func ResolveHookDiscovery(ctx context.Context) HookDiscovery {
	worktreeRoot, err := resolveWorktreeRoot(ctx)
	if err != nil {
		return unresolvedHookDiscoveryAt("", "worktree root could not be resolved: "+err.Error())
	}
	return resolveHookDiscovery(worktreeRoot)
}

func resolveHookDiscovery(worktreeRoot string) HookDiscovery {
	if canonicalRoot, err := canonicalPath(worktreeRoot); err == nil {
		worktreeRoot = canonicalRoot
	}
	discovery := HookDiscovery{
		State:        HookDiscoveryResolved,
		worktreeRoot: worktreeRoot,
	}
	root := worktreeRoot
	metadata, err := gitrepo.ResolveWorktreeMetadata(worktreeRoot)
	if err != nil {
		return unresolvedHookDiscoveryAt(worktreeRoot, "Git layout could not be resolved: "+err.Error())
	}
	dotGitPath := metadata.GitDir
	commonGitPath := metadata.CommonDir

	if metadata.WorktreeID != "" &&
		!isSubmoduleGitDir(dotGitPath) &&
		linkedWorktreeRegistrationMatches(dotGitPath, worktreeRoot) {
		candidate := filepath.Dir(commonGitPath)
		if rootOwnsGitDir(candidate, commonGitPath) {
			root = candidate
		} else if !commonGitDirIsBare(commonGitPath) && !hasDotGitEntry(candidate) {
			// A separate Git directory has no .git entry at the storage parent;
			// Codex nevertheless uses that parent as its project root.
			root = candidate
		}
	}

	if isUserHookRoot(root) {
		return unresolvedHookDiscoveryAt(worktreeRoot, fmt.Sprintf("derived hook root %q is user-wide", root))
	}
	discovery.DiscoveredHooks = DiscoveredHooksPath{path: filepath.Join(root, ".codex", HooksFileName)}
	return discovery
}

func isSubmoduleGitDir(dotGitPath string) bool {
	return strings.Contains(filepath.ToSlash(dotGitPath), "/.git/modules/")
}

func linkedWorktreeRegistrationMatches(dotGitPath, worktreeRoot string) bool {
	data, err := os.ReadFile(filepath.Join(dotGitPath, "gitdir")) //nolint:gosec // path comes from Git metadata.
	if err != nil {
		return false
	}
	registered := strings.TrimSpace(string(data))
	if registered == "" {
		return false
	}
	if !filepath.IsAbs(registered) {
		registered = filepath.Join(dotGitPath, registered)
	}
	if filepath.Base(registered) != ".git" {
		return false
	}
	registeredRoot, err := canonicalPath(filepath.Dir(registered))
	return err == nil && registeredRoot == worktreeRoot
}

func rootOwnsGitDir(root, commonGitPath string) bool {
	metadata, err := gitrepo.ResolveWorktreeMetadata(root)
	if err != nil {
		return false
	}
	resolved, err := canonicalPath(metadata.GitDir)
	if err != nil {
		return false
	}
	common, err := canonicalPath(commonGitPath)
	return err == nil && resolved == common
}

func hasDotGitEntry(root string) bool {
	_, err := os.Lstat(filepath.Join(root, ".git"))
	return err == nil
}

func commonGitDirIsBare(commonGitPath string) bool {
	data, err := os.ReadFile(filepath.Join(commonGitPath, "config")) //nolint:gosec // path comes from Git metadata.
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "bare = true")
}

func unresolvedHookDiscoveryAt(worktreeRoot, reason string) HookDiscovery {
	return HookDiscovery{
		State:        HookDiscoveryUnresolved,
		worktreeRoot: worktreeRoot,
		Diagnostic:   &UnresolvedHookDiscoveryError{Reason: reason},
	}
}

// WorktreeProjectLayerExists reports whether the current checkout has a valid
// local .codex project directory.
func WorktreeProjectLayerExists(ctx context.Context) bool {
	hooks, err := ResolveWorktreeHooksPath(ctx)
	return err == nil && projectLayerExists(filepath.Dir(hooks.Path()))
}

func projectLayerExists(projectDir string) bool {
	return validateExistingProjectDir(projectDir) == nil
}

func resolveWorktreeRoot(ctx context.Context) (string, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot, err = os.Getwd() //nolint:forbidigo // Preserve hook setup in non-repository test and bootstrap directories.
		if err != nil {
			return "", fmt.Errorf("resolve current directory: %w", err)
		}
	}
	worktreeRoot, err = canonicalPath(worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	return worktreeRoot, nil
}

// WorktreeHooksPath identifies the hooks file owned by the current checkout.
// It is intentionally distinct from DiscoveredHooksPath.
type WorktreeHooksPath struct {
	path         string
	worktreeRoot string
}

// Path returns the current checkout's hooks file path.
func (p WorktreeHooksPath) Path() string {
	return p.path
}

// hookConfig returns the same file as a handle anchored on the WORKTREE ROOT,
// not on `.codex`.
//
// The distinction is the point. Reads here already used an os.Root, but opened
// it at filepath.Dir of the target — which puts `.codex` above the root, so a
// checked-in symlink there was resolved before containment began and the root
// only ever contained the final "hooks.json". Anchoring one level up makes
// `.codex` a NAME inside the worktree, which is what MkdirAllNoSymlink can
// refuse. It also covers the writes, which had no root at all.
func (p WorktreeHooksPath) hookConfig() (*agent.HookConfigFile, error) {
	return agent.OpenHookConfig(p.worktreeRoot, ".codex/"+HooksFileName) //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
}

// ResolveWorktreeHooksPath resolves the hooks file owned by the current
// checkout, independently of the file Codex discovers.
func ResolveWorktreeHooksPath(ctx context.Context) (WorktreeHooksPath, error) {
	worktreeRoot, err := resolveWorktreeRoot(ctx)
	if err != nil {
		return WorktreeHooksPath{}, err
	}
	return resolveWorktreeHooksPath(worktreeRoot)
}

func resolveWorktreeHooksPath(worktreeRoot string) (WorktreeHooksPath, error) {
	canonicalRoot, err := canonicalPath(worktreeRoot)
	if err != nil {
		return WorktreeHooksPath{}, fmt.Errorf("resolve worktree root: %w", err)
	}
	return WorktreeHooksPath{
		path:         filepath.Join(canonicalRoot, ".codex", HooksFileName),
		worktreeRoot: canonicalRoot,
	}, nil
}

func isUserHookRoot(hookRoot string) bool {
	home, err := os.UserHomeDir()
	if err == nil {
		canonicalHome, canonicalErr := canonicalPath(home)
		if canonicalErr == nil && hookRoot == canonicalHome {
			return true
		}
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		if home == "" {
			return false
		}
		codexHome = filepath.Join(home, ".codex")
	}
	canonicalCodexHome, err := canonicalPath(codexHome)
	if err != nil {
		return false
	}
	canonicalProjectDir, err := canonicalPath(filepath.Join(hookRoot, ".codex"))
	return err == nil && canonicalProjectDir == canonicalCodexHome
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make path absolute: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Clean(abs), nil
	}
	return "", fmt.Errorf("evaluate path symlinks: %w", err)
}
