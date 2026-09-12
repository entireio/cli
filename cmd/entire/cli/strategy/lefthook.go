package strategy

// Lefthook integration.
//
// Lefthook owns .git/hooks/* and regenerates every hook from its config on
// each run, gated by a checksum of that config. So Entire cannot keep a
// wrapper there: any lefthook.yml edit, or the `lefthook install -f` that its
// npm postinstall runs, silently takes the file back. Worse, the reclaim
// happens at the top of whatever hook fires next — typically the pre-commit of
// the very commit being made — so repairing at turn start loses the race.
//
// The fix is to stop wanting the file. Entire writes a config of its own and
// registers it with one `extends` entry in Lefthook's local config; Lefthook
// then invokes Entire's scripts itself. Lefthook resolves `extends` at run
// time, so the artifacts take effect immediately — no `lefthook install`, no
// user action — and Lefthook's own config is never touched.
//
// Verified against lefthook 2.1.10: a config reached via `extends` can declare
// `scripts`, those scripts receive git's hook arguments (pre-push gets the
// remote and its URL), and the whole arrangement survives both
// `lefthook install -f` and a config edit.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
	"gopkg.in/yaml.v3"
)

const (
	// entireLefthookConfig is Entire's own Lefthook config. Entire owns this
	// file outright, which is what keeps it out of the user's.
	entireLefthookConfig = "entire-lefthook.yml"
	// lefthookScriptDir is Lefthook's clone-local script directory, its
	// default for source_dir_local.
	lefthookScriptDir = ".lefthook-local"
	lefthookScript    = "entire.sh"
	// lefthookOwnedMarker identifies a file as Entire's. Removal and
	// overwriting both require it: the path is Entire's choice, but the
	// repository is the user's.
	lefthookOwnedMarker = "entire-cli-owned:lefthook:v1"
)

// ErrLefthookLocalConfigUnwritable reports a local config Entire will not
// edit. Lefthook reads exactly one local config and lefthook-local.yml takes
// precedence over lefthook-local.toml, so creating the former beside a user's
// latter would silently stop their hooks running. Entire declines instead and
// leaves its native hooks in place.
var ErrLefthookLocalConfigUnwritable = errors.New("lefthook local config is not YAML")

// ErrLefthookArtifactBlocked reports a path Entire cannot claim because an
// earlier backup is already sitting beside it. Burying that backup would
// destroy whatever it holds, so the integration stops and says so instead —
// unlike ErrLefthookLocalConfigUnwritable, which is a deliberate decision not
// to touch the repo and is handled silently.
var ErrLefthookArtifactBlocked = errors.New("lefthook artifact path is blocked by an existing backup")

// ErrLefthookLocalConfigTracked reports a local config the repository carries.
// Lefthook documents lefthook-local.* as personal and gitignored; a tracked
// one is the team's file, so Entire declines to add its entry rather than
// modify something shared. Like ErrLefthookLocalConfigUnwritable this is a
// decision, not a failure, and the native hooks stay.
var ErrLefthookLocalConfigTracked = errors.New("lefthook local config is tracked by git")

// lefthookLocalConfigNames are the local config names Lefthook reads, in its
// own precedence order. Writing into anything but the first match present
// would be a silent no-op.
var lefthookLocalConfigNames = []string{
	"lefthook-local.yml", "lefthook-local.yaml",
	"lefthook-local.json", "lefthook-local.toml",
	".lefthook-local.yml", ".lefthook-local.yaml",
	".lefthook-local.json", ".lefthook-local.toml",
}

// LefthookManaged reports whether this repository is managed by Lefthook.
//
// Only a main config counts. A lone lefthook-local.* is deliberately not
// evidence: Entire creates lefthook-local.yml itself, so trusting it would
// leave a repo classified as Lefthook-managed after the user removed Lefthook
// — reporting "via Lefthook" in status while nothing ran Entire at all.
func LefthookManaged(repoRoot string) bool {
	for _, prefix := range []string{"", "."} {
		for _, ext := range []string{"yml", "yaml", "json", "toml"} {
			if _, err := os.Stat(filepath.Join(repoRoot, prefix+"lefthook."+ext)); err == nil {
				return true
			}
		}
	}
	return false
}

// EnsureLefthookIntegration registers Entire with Lefthook, returning the
// number of artifacts written.
//
// The hook-command prefix is read from this repository's settings rather than
// taken as an argument, for the reason ReinstallGitHooks exists: a caller that
// passes the wrong value does not fail, it silently drops
// absolute_git_hook_path — and here that means rewriting a working
// absolute-path install down to a bare `entire`, breaking delivery for the GUI
// git clients that setting exists to serve. `entire doctor` did exactly that.
//
// There is deliberately no rollback. Every write is atomic and content-
// idempotent, and the entry that activates the integration is written last, so
// a failure part way through leaves inert files that the next install
// overwrites. EnsureSetup runs at every turn start, so "the next install" is a
// guarantee rather than a hope.
//
// That argument only holds for a failure that eventually stops happening, so
// two things come before the writes: the non-YAML local config is refused up
// front, because it is a decision not to touch the repo rather than a failure
// that will pass, and the exclude entries go in while the paths are still
// empty, so a partial write is never left unignored.
func EnsureLefthookIntegration(ctx context.Context) (int, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve worktree root: %w", err)
	}
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return 0, fmt.Errorf("open worktree: %w", err)
	}
	// Refuse BEFORE writing anything. Declining to shadow a non-YAML local
	// config has to mean leaving the repo untouched: the extends entry is what
	// makes the other artifacts do anything, so writing them first left six
	// untracked files in the worktree that this same function had not reached
	// the point of excluding — and because ensureLefthookIntegrationIfManaged
	// treats this refusal as a decision rather than a failure, that recurred
	// on every turn instead of resolving itself.
	if name, existing, err := findLocalConfig(root); err != nil {
		return 0, err
	} else if existing != nil && !extendsEntryPresent(root) && lefthookLocalConfigTracked(ctx, repoRoot, name) {
		// Only when the entry would actually be ADDED: a tracked config that
		// already carries it (committed by the team, or added by hand) is
		// working as intended, and nothing here writes to it.
		return 0, fmt.Errorf("%w: %s", ErrLefthookLocalConfigTracked, name)
	}
	cmdPrefix, err := hookCmdPrefix(hookSettingsFromConfig(ctx))
	if err != nil {
		return 0, err
	}

	// Before the writes, not after: a write that fails part way through (a
	// blocked artifact path, a full disk) otherwise leaves files in the
	// worktree that nothing has excluded yet, and the entries name paths
	// rather than existing files, so writing them early costs nothing.
	if err := excludeArtifacts(ctx, repoRoot); err != nil {
		return 0, err
	}

	written := 0
	for _, spec := range buildHookSpecs(cmdPrefix) {
		name := lefthookScriptPath(spec.name)
		if err := displaceUnowned(root, name, 0o755); err != nil {
			return 0, err
		}
		if err := osroot.MkdirAllNoSymlink(root, filepath.ToSlash(filepath.Dir(name)), 0o755); err != nil {
			return 0, fmt.Errorf("create %s: %w", filepath.Dir(name), err)
		}
		changed, writeErr := writeHookFile(root, name, renderLefthookScript(spec))
		if writeErr != nil {
			return 0, writeErr
		}
		if changed {
			written++
		}
	}

	if err := displaceUnowned(root, entireLefthookConfig, 0o644); err != nil {
		return 0, err
	}
	changed, err := writeOwnedConfig(root)
	if err != nil {
		return 0, err
	}
	if changed {
		written++
	}

	// Last: this is what makes Lefthook load any of the above.
	if _, err := ensureExtendsEntry(root); err != nil {
		return 0, err
	}
	if err := reconcileHookFiles(ctx); err != nil {
		return 0, err
	}
	return written, nil
}

// artifactsExcluded reports whether the exclude block is in place, which the
// install treats as part of being current.
func artifactsExcluded(ctx context.Context, repoRoot string) bool {
	root, err := openGitCommonDir(ctx, repoRoot)
	if err != nil {
		return true // cannot tell; do not churn the install on it
	}
	data, err := osroot.ReadFileNoFollow(root, gitExcludePath)
	return err == nil && strings.Contains(string(data), lefthookExcludeBlock())
}

// lefthookExcludeBlock is the exact text excludeArtifacts writes, so
// artifactsExcluded tests for what an install would produce and any future
// change to the entries repairs itself on the next turn.
func lefthookExcludeBlock() string {
	// lefthook-local.yml is included because Entire creates it when absent and
	// Lefthook documents it as a personal, uncommitted file. An exclude entry
	// only affects untracked paths, so a repository that does commit it is
	// unaffected by this.
	return excludeBlockBegin +
		"/" + entireLefthookConfig + "\n" +
		"/" + lefthookScriptDir + "/\n" +
		"/" + lefthookLocalConfigNames[0] + "\n" +
		excludeBlockEnd
}

const (
	// LefthookManagerName is how Lefthook is named in user-facing output and
	// in HookDelivery.Manager; lefthookExtendsKey is Lefthook's own config key
	// for pulling in another config file at run time.
	LefthookManagerName = "Lefthook"
	lefthookExtendsKey  = "extends"

	gitExcludePath    = "info/exclude"
	excludeBlockBegin = "# " + lefthookOwnedMarker + " begin\n"
	excludeBlockEnd   = "# " + lefthookOwnedMarker + " end\n"
)

// excludeArtifacts adds Entire's generated files to the clone-local
// .git/info/exclude. They are generated and per-clone, so they do not belong
// in the user's .gitignore — and left unignored they make a dirty worktree the
// normal state in every Lefthook repo, which eventually gets Entire's
// integration files committed by an agent that sees them in git status.
func excludeArtifacts(ctx context.Context, repoRoot string) error {
	return rewriteExcludeBlock(ctx, repoRoot, lefthookExcludeBlock())
}

// rewriteExcludeBlock replaces Entire's marked block in .git/info/exclude with
// block, which is empty on uninstall. Failing to open the git dir is not an
// error: ignoring these files is a convenience, not part of the integration.
func rewriteExcludeBlock(ctx context.Context, repoRoot, block string) error {
	root, err := openGitCommonDir(ctx, repoRoot)
	if err != nil {
		return nil //nolint:nilerr // best effort: ignoring is a convenience
	}
	existing, err := osroot.ReadFileNoFollow(root, gitExcludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read git exclude: %w", err)
	}
	body := string(existing)
	if block != "" && strings.Contains(body, block) {
		return nil
	}
	// Replace a previous block rather than appending a second one.
	if i := strings.Index(body, excludeBlockBegin); i >= 0 {
		if j := strings.Index(body[i:], excludeBlockEnd); j >= 0 {
			body = body[:i] + body[i+j+len(excludeBlockEnd):]
		} else {
			return nil // an unterminated block is not ours to guess at
		}
	} else if block == "" {
		return nil
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if err := osroot.MkdirAllNoSymlink(root, "info", 0o755); err != nil {
		return fmt.Errorf("create info dir: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, gitExcludePath, []byte(body+block), 0o644); err != nil {
		return fmt.Errorf("write git exclude: %w", err)
	}
	return nil
}

func openGitCommonDir(ctx context.Context, repoRoot string) (*os.Root, error) {
	commonDir, err := gitCommonDirFor(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	root, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return nil, fmt.Errorf("open git common dir: %w", err)
	}
	return root, nil
}

// gitCommonDirCache memoizes the git common dir per worktree root. Resolving
// it is a `git rev-parse` subprocess — measured at ~10ms, against ~0.3µs for
// the root open it feeds — and the exclude check that needs it is part of the
// currency test, which a single turn-start runs two or three times. A
// worktree's common dir does not move while the process lives. Only the path
// is cached; the root itself is already memoized by osroot.Shared, which is
// also what keeps a handle from outliving a deleted directory here.
var (
	gitCommonDirMu    sync.RWMutex
	gitCommonDirCache = map[string]string{}
)

func gitCommonDirFor(ctx context.Context, repoRoot string) (string, error) {
	gitCommonDirMu.RLock()
	cached, ok := gitCommonDirCache[repoRoot]
	gitCommonDirMu.RUnlock()
	if ok {
		return cached, nil
	}
	commonDir, err := gitdir.CommonDirForWorktree(ctx, repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	gitCommonDirMu.Lock()
	gitCommonDirCache[repoRoot] = commonDir
	gitCommonDirMu.Unlock()
	return commonDir, nil
}

// clearGitCommonDirCache exists for tests that reuse a worktree root path.
func clearGitCommonDirCache() {
	gitCommonDirMu.Lock()
	gitCommonDirCache = map[string]string{}
	gitCommonDirMu.Unlock()
}

// LefthookIntegrationCurrent reports whether Entire's artifacts are present
// and current: its config, the extends entry pointing at it, and every script.
// The prefix comes from settings; see EnsureLefthookIntegration.
func LefthookIntegrationCurrent(ctx context.Context) (bool, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return false, fmt.Errorf("resolve worktree root: %w", err)
	}
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return false, fmt.Errorf("open worktree: %w", err)
	}
	want, err := renderOwnedConfig()
	if err != nil {
		return false, err
	}
	data, err := osroot.ReadFileNoFollow(root, entireLefthookConfig)
	if err != nil || string(data) != want {
		return false, nil //nolint:nilerr // a missing or stale config is "not current", not a failure
	}
	if !extendsEntryPresent(root) {
		return false, nil
	}
	cmdPrefix, err := hookCmdPrefix(hookSettingsFromConfig(ctx))
	if err != nil {
		return false, err
	}
	for _, spec := range buildHookSpecs(cmdPrefix) {
		content, err := osroot.ReadFileNoFollow(root, lefthookScriptPath(spec.name))
		if err != nil || string(content) != renderLefthookScript(spec) {
			return false, nil //nolint:nilerr // same: stale, not broken
		}
	}
	// The exclude entry counts as part of the install. Without it every
	// Lefthook worktree reads as dirty, and a repo that predates this check
	// needs a repair pass to pick it up.
	return artifactsExcluded(ctx, repoRoot), nil
}

// RemoveLefthookIntegration removes Entire's artifacts, returning the number
// removed. Files without Entire's marker are left alone.
func RemoveLefthookIntegration(ctx context.Context) (int, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve worktree root: %w", err)
	}
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return 0, fmt.Errorf("open worktree: %w", err)
	}
	// The extends entry goes first, mirroring the install, which writes it
	// last: at no point should Lefthook's config name a file that is not
	// there. Lefthook 2.1.10 tolerates a dangling extends (run, install,
	// validate and dump all succeed), so this is not repairing a break — it is
	// declining to depend on another tool's tolerance, and it fails in the
	// better direction, since giving up here leaves the integration whole
	// rather than half-deleted.
	removed := 0
	dropped, err := removeExtendsEntry(root)
	if err != nil {
		return removed, err
	}
	if dropped {
		removed++
	}
	for _, hook := range gitHookNames {
		name := lefthookScriptPath(hook)
		owned, err := fileIsOwned(root, name)
		if err != nil || !owned {
			continue // unreadable or not ours: leave it
		}
		if err := osroot.RemoveNoSymlinks(root, name); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("remove %s: %w", name, err)
		}
		removed++
	}
	if owned, ownErr := fileIsOwned(root, entireLefthookConfig); ownErr == nil && owned {
		if err := osroot.RemoveNoSymlinks(root, entireLefthookConfig); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("remove %s: %w", entireLefthookConfig, err)
		}
		removed++
	}
	if err := rewriteExcludeBlock(ctx, repoRoot, ""); err != nil {
		return removed, err
	}
	// Prune the directories the scripts lived in. RemoveNoSymlinks fails on a
	// non-empty directory, which is exactly the guard wanted: anything the
	// user put there keeps its parent alive.
	for _, hook := range gitHookNames {
		//nolint:errcheck // a non-empty directory is meant to survive
		_ = osroot.RemoveNoSymlinks(root, filepath.ToSlash(filepath.Join(lefthookScriptDir, hook)))
	}
	//nolint:errcheck // same
	_ = osroot.RemoveNoSymlinks(root, lefthookScriptDir)
	return removed, nil
}

func lefthookScriptPath(hook string) string {
	return filepath.ToSlash(filepath.Join(lefthookScriptDir, hook, lefthookScript))
}

// renderLefthookScript is the native hook body with Entire's marker swapped
// for the Lefthook-owned one, so ownership is checkable and the two kinds of
// artifact are never confused for each other.
func renderLefthookScript(spec hookSpec) string {
	return strings.Replace(spec.content,
		"# "+entireHookMarker+"\n", "# "+lefthookOwnedMarker+"\n", 1)
}

func renderOwnedConfig() (string, error) {
	hooks := map[string]any{}
	for _, hook := range gitHookNames {
		hooks[hook] = map[string]any{
			"scripts": map[string]any{lefthookScript: map[string]any{"runner": "bash"}},
		}
	}
	hooks["source_dir_local"] = lefthookScriptDir
	out, err := yaml.Marshal(hooks)
	if err != nil {
		return "", fmt.Errorf("render %s: %w", entireLefthookConfig, err)
	}
	return "# " + lefthookOwnedMarker + "\n" +
		"# Managed by Entire. Remove the extends entry in your Lefthook local\n" +
		"# config to detach; edits here are overwritten.\n" + string(out), nil
}

func writeOwnedConfig(root *os.Root) (bool, error) {
	content, err := renderOwnedConfig()
	if err != nil {
		return false, err
	}
	existing, readErr := osroot.ReadFileNoFollow(root, entireLefthookConfig)
	if readErr == nil && string(existing) == content {
		return false, nil
	}
	if err := jsonutil.WriteFileAtomicIn(root, entireLefthookConfig, []byte(content), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", entireLefthookConfig, err)
	}
	return true, nil
}

func fileIsOwned(root *os.Root, name string) (bool, error) {
	data, err := osroot.ReadFileNoFollow(root, name)
	if err != nil {
		return false, err //nolint:wrapcheck // callers only test the bool
	}
	return strings.Contains(string(data), lefthookOwnedMarker), nil
}

// displaceUnowned moves a file Entire did not write aside to <name>.pre-entire
// before Entire claims the path, and refuses rather than bury an earlier
// backup. Corruption destroys the marker, so without this a damaged script
// would read as a foreign file and could never be repaired.
func displaceUnowned(root *os.Root, name string, mode os.FileMode) error {
	data, err := osroot.ReadFileNoFollow(root, name)
	if err != nil || strings.Contains(string(data), lefthookOwnedMarker) {
		return nil //nolint:nilerr // absent or ours: nothing to displace
	}
	backup := name + GitHookBackupSuffix
	if _, err := osroot.ReadFileNoFollow(root, backup); err == nil {
		return fmt.Errorf("%w: %s already exists; remove it to let Entire reinstall %s",
			ErrLefthookArtifactBlocked, backup, name)
	}
	if err := jsonutil.WriteFileAtomicIn(root, backup, data, mode); err != nil {
		return fmt.Errorf("back up %s: %w", name, err)
	}
	return nil
}

// findLocalConfig returns the local config Lefthook actually reads, its
// contents, and whether Entire may write to it.
func findLocalConfig(root *os.Root) (name string, data []byte, err error) {
	for _, candidate := range lefthookLocalConfigNames {
		body, readErr := osroot.ReadFileNoFollow(root, candidate)
		if readErr != nil {
			continue
		}
		if !strings.HasSuffix(candidate, ".yml") && !strings.HasSuffix(candidate, ".yaml") {
			return candidate, body, fmt.Errorf("%w: %s", ErrLefthookLocalConfigUnwritable, candidate)
		}
		return candidate, body, nil
	}
	return lefthookLocalConfigNames[0], nil, nil
}

// ensureExtendsEntry adds one entry to the local config's `extends` sequence,
// preserving everything else including comments. Returns the config written.
func ensureExtendsEntry(root *os.Root) (string, error) {
	name, existing, err := findLocalConfig(root)
	if err != nil {
		return name, err
	}
	doc, seq, err := extendsSequence(existing)
	if err != nil {
		return name, fmt.Errorf("%s: %w", name, err)
	}
	for _, item := range seq.Content {
		if item.Value == entireLefthookConfig {
			return name, nil
		}
	}
	seq.Content = append(seq.Content, &yaml.Node{
		Kind: yaml.ScalarNode, Tag: "!!str", Value: entireLefthookConfig,
		LineComment: lefthookOwnedMarker,
	})
	out, err := encodeYAML(doc)
	if err != nil {
		return name, fmt.Errorf("%s: %w", name, err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, name, out, 0o644); err != nil {
		return name, fmt.Errorf("write %s: %w", name, err)
	}
	return name, nil
}

// extendsEntryPresent reports whether the local config already pulls in
// Entire's config. An unwritable, absent or unparseable config has no entry.
func extendsEntryPresent(root *os.Root) bool {
	_, existing, err := findLocalConfig(root)
	if err != nil || existing == nil {
		return false
	}
	_, seq, err := extendsSequence(existing)
	if err != nil {
		return false
	}
	for _, item := range seq.Content {
		if item.Value == entireLefthookConfig {
			return true
		}
	}
	return false
}

func removeExtendsEntry(root *os.Root) (bool, error) {
	name, existing, err := findLocalConfig(root)
	if err != nil || existing == nil {
		return false, nil //nolint:nilerr // nothing of ours to remove
	}
	doc, seq, err := extendsSequence(existing)
	if err != nil {
		return false, nil //nolint:nilerr // unparseable: not our entry
	}
	kept := make([]*yaml.Node, 0, len(seq.Content))
	changed := false
	for _, item := range seq.Content {
		if item.Value == entireLefthookConfig {
			changed = true
			continue
		}
		kept = append(kept, item)
	}
	if !changed {
		return false, nil
	}
	seq.Content = kept
	mapping := doc.Content[0]
	if len(kept) == 0 {
		for i := 0; i+1 < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == lefthookExtendsKey {
				mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
				break
			}
		}
	}
	// A file that held nothing but our entry is ours to delete.
	if len(mapping.Content) == 0 {
		if err := osroot.RemoveNoSymlinks(root, name); err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf("remove %s: %w", name, err)
		}
		return true, nil
	}
	out, err := encodeYAML(doc)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, name, out, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", name, err)
	}
	return true, nil
}

// extendsSequence returns the document and its `extends` sequence, creating
// the key when absent. Kept as yaml.Node rather than a struct because the
// user's comments and key order must survive the round trip.
func extendsSequence(existing []byte) (*yaml.Node, *yaml.Node, error) {
	doc := &yaml.Node{}
	if len(strings.TrimSpace(string(existing))) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	} else if err := yaml.Unmarshal(existing, doc); err != nil {
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("parse: top level must be a mapping")
	}
	mapping := doc.Content[0]
	found := -1
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != lefthookExtendsKey {
			continue
		}
		if found >= 0 {
			return nil, nil, errors.New("duplicate extends key")
		}
		found = i
	}
	if found >= 0 {
		seq := mapping.Content[found+1]
		if seq.Kind != yaml.SequenceNode {
			return nil, nil, errors.New("extends must be a sequence")
		}
		return doc, seq, nil
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: lefthookExtendsKey}, seq)
	return doc, seq, nil
}

func encodeYAML(doc *yaml.Node) ([]byte, error) {
	var out strings.Builder
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return []byte(out.String()), nil
}

// isLefthookLauncher reports whether a file is the hook Lefthook generates for
// this hook name.
//
// The test is deliberately structural rather than a search for the word
// "lefthook", because a false positive here is silent and permanent: a hook
// misread as Lefthook's gets restored over Entire's by reconcileHookFiles and
// then skipped forever by installSkipsHook, so capture stops for that hook and
// nothing ever repairs it. Lefthook's template defines a call_lefthook shell
// function and ends by dispatching THIS hook through it; a hand-written script
// that merely runs lefthook does neither, and a launcher for a different hook
// fails the second test.
func isLefthookLauncher(root *os.Root, name, hook string) bool {
	data, err := osroot.ReadFileNoFollow(root, name)
	if err != nil {
		return false
	}
	body := string(data)
	return strings.Contains(body, "call_lefthook()") &&
		strings.Contains(body, `call_lefthook run "`+hook+`"`)
}

// reconcileHookFiles hands .git/hooks/* back to Lefthook and clears the
// wreckage of the era when the two fought over those files.
//
// Once Entire is registered in Lefthook's config, Lefthook's own hook runs
// Entire. Entire owning the hook file as well means the hook runs Entire
// TWICE — its own copy, then Lefthook's launcher via the chain call — so where
// Lefthook's launcher is sitting in Entire's backup it is moved back over
// Entire's hook. installSkipsHook then leaves that path alone.
//
// The <hook>.old files are not merely untidy: Lefthook refuses to move a hook
// aside when its backup already exists, and reports "could not replace the
// hook: can't rename pre-push to pre-push.old - file already exists" on every
// sync until it is gone. That error outlives this fix unless cleared, and
// every repo the fight has already touched is in exactly that state.
//
// Nothing is touched unless its contents prove whose it was.
func reconcileHookFiles(ctx context.Context) error {
	// GetHooksDir rather than the by-path variant: both callers resolved their
	// worktree root from this same process's directory, and this one is
	// memoized. The by-path variant is another ~13ms `git rev-parse` per turn.
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return nil //nolint:nilerr // best effort: no hooks dir, nothing to do
	}
	root, err := hooksRootForRemoval(hooksDir)
	if err != nil {
		return nil //nolint:nilerr // same
	}
	for _, hook := range gitHookNames {
		backup := hook + GitHookBackupSuffix
		if isLefthookLauncher(root, backup, hook) && fileContains(root, hook, entireHookMarker) {
			if err := root.Rename(backup, hook); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("restore %s: %w", hook, err)
			}
			continue
		}
		// What Lefthook displaced, back when Entire owned the file.
		stale := hook + ".old"
		if !fileContains(root, stale, entireHookMarker) {
			continue
		}
		if err := osroot.RemoveNoSymlinks(root, stale); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale backup %s: %w", stale, err)
		}
	}
	return nil
}

func fileContains(root *os.Root, name, marker string) bool {
	data, err := osroot.ReadFileNoFollow(root, name)
	return err == nil && strings.Contains(string(data), marker)
}

// installSkipsHook reports whether InstallGitHook should leave a hook file
// alone because Lefthook owns it and already dispatches Entire from its own
// config. Writing Entire's hook over Lefthook's launcher would both run Entire
// twice and stop the user's own Lefthook commands until Lefthook's next sync.
//
// A hook Lefthook has not taken over is installed normally: in a repo where
// nobody has run `lefthook install` yet, Entire's own hooks are the only
// delivery there is.
func installSkipsHook(root *os.Root, hook string, lefthookDelivers bool) bool {
	return lefthookDelivers && isLefthookLauncher(root, hook, hook)
}

// uncoveredHookPaths names the hooks nothing would run Entire for.
//
// Registering with Lefthook is not enough on its own: git runs a hook only if
// the FILE exists, and Lefthook creates one per hook it knew about when it last
// installed. Entire's config reaches Lefthook through `extends` at run time, so
// a repo whose lefthook.yml declares only pre-commit has no file for the other
// four and nothing dispatches Entire there — while every artifact Entire owns
// is present and correct. Reporting that as delivering is the same false OK
// this integration exists to remove, so the check asks the question git asks:
// is there a file here, and does it lead to Entire?
func uncoveredHookPaths(ctx context.Context) []string {
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return gitHookNames
	}
	root, err := hooksRootForRemoval(hooksDir)
	if err != nil {
		return gitHookNames
	}
	var uncovered []string
	for _, hook := range gitHookNames {
		// Lefthook's launcher reaches Entire through the integration; Entire's
		// own hook reaches it directly. Anything else — absent, a symlink, or
		// another tool's script — does not.
		if isLefthookLauncher(root, hook, hook) || fileContains(root, hook, entireHookMarker) {
			continue
		}
		uncovered = append(uncovered, hook)
	}
	return uncovered
}

// lefthookDeliversHooks reports whether Lefthook will run Entire from its own
// configuration, which is what makes Entire's own hook files redundant.
func lefthookDeliversHooks(ctx context.Context) bool {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil || !LefthookManaged(repoRoot) {
		return false
	}
	current, err := LefthookIntegrationCurrent(ctx)
	return err == nil && current
}

// HookDelivery describes whether Entire's Git hooks will actually fire.
type HookDelivery struct {
	// OK is true when capture and delivery are wired up.
	OK bool
	// Manager names the hook manager that delivers Entire, if one does.
	Manager string
	// Reason explains a false OK, ready to show a user.
	Reason string
	// Declined names the Lefthook local config Entire will not write to, when
	// that is why Lefthook is not the one delivering. It is set whether or not
	// OK is true, because it is the answer to "why is Entire not in Lefthook's
	// config" in a repository whose hooks are working fine without it.
	Declined string
}

// CheckHookDelivery reports whether Entire's hooks will fire in this
// repository. This is the question `entire status` was answering wrongly in
// #2264: it reported a checkpoint destination while no hook existed to reach
// it. There is no network access and no mutation.
//
// A Lefthook repo is judged on the integration, not on .git/hooks/*: Lefthook
// owns those files and rewrites them constantly, so their contents say nothing
// about whether Entire will run.
func CheckHookDelivery(ctx context.Context) HookDelivery {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return HookDelivery{Reason: "Could not resolve the repository root."}
	}
	declined := ""
	if LefthookManaged(repoRoot) {
		current, checkErr := LefthookIntegrationCurrent(ctx)
		switch {
		case checkErr != nil:
			return HookDelivery{Manager: LefthookManagerName,
				Reason: fmt.Sprintf("Could not inspect Entire's Lefthook integration: %v", checkErr)}
		case current:
			if uncovered := uncoveredHookPaths(ctx); len(uncovered) > 0 {
				return HookDelivery{Manager: LefthookManagerName,
					Reason: fmt.Sprintf("Lefthook has no hook file for %s, so nothing runs Entire there.",
						strings.Join(uncovered, ", "))}
			}
			return HookDelivery{OK: true, Manager: LefthookManagerName}
		}
		// A declined integration is permanent, not a repair pending: Entire's
		// own hooks are the arrangement in such a repo, so the answer is the
		// native one. Saying "not registered with Lefthook" here would report
		// a working repository as broken, forever and with no fix to offer.
		if declined = declinedLefthookLocalConfig(ctx, repoRoot); declined == "" {
			return HookDelivery{Manager: LefthookManagerName,
				Reason: "Entire is not registered with Lefthook, so Lefthook's hooks do not run it."}
		}
	}
	switch CheckGitHookState(ctx) {
	case GitHooksCurrent:
		return HookDelivery{OK: true, Declined: declined}
	case GitHooksOutdated:
		return HookDelivery{Reason: "Entire's Git hooks are outdated.", Declined: declined}
	case GitHooksAbsent:
	}
	return HookDelivery{Reason: "Entire's Git hooks are not installed.", Declined: declined}
}

// declinedLefthookLocalConfig describes the local config Entire refuses to
// write to, as a phrase naming the file and why — or "" when nothing is being
// refused. A phrase rather than a name because the two reasons need different
// remedies and every consumer of HookDelivery.Declined is reporting it to a
// person.
func declinedLefthookLocalConfig(ctx context.Context, repoRoot string) string {
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return ""
	}
	name, existing, err := findLocalConfig(root)
	switch {
	case errors.Is(err, ErrLefthookLocalConfigUnwritable):
		return name + " is not YAML"
	case err != nil:
		return ""
	case existing != nil && !extendsEntryPresent(root) && lefthookLocalConfigTracked(ctx, repoRoot, name):
		return name + " is tracked by git"
	}
	return ""
}

// lefthookLocalConfigTracked reports whether the local config is carried by
// the repository rather than being the per-clone file Lefthook documents.
//
// A tracked one is shared with the team, so adding Entire's extends entry to
// it modifies their file, not the developer's — and a revert followed by a
// reinstall next turn is a fight Entire should not pick.
//
// settings.PathIsTracked rather than an index scan here: that probe carries
// two commits' worth of correctness an open-coded one does not, in particular
// comparing paths the way the filesystem would, because on a case-insensitive
// volume a differently-cased tracked path is the same file. It is consulted
// only when Entire is about to add the entry, so after a successful install it
// never runs again.
func lefthookLocalConfigTracked(ctx context.Context, repoRoot, name string) bool {
	tracked, err := settings.PathIsTracked(ctx, repoRoot, name)
	return err == nil && tracked
}
