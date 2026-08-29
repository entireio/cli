package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/internal/gitpath"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

// localLayerTrackedReason explains why a tracked .entire/settings.local.json
// is ignored.
//
// The whole point of the local layer is that it is per-clone and per-developer:
// it is gitignored, it is where `entire enable --local` writes, and other code
// treats "present in the local file" as proof the developer chose it
// personally. A tracked local file breaks that premise — .gitignore does not
// apply to a path that is already tracked, so `git add -f
// .entire/settings.local.json` commits it and a fresh clone materializes it
// with whatever the committer put there. It then silently overrides project
// settings for everyone who clones, which is both a correctness surprise and,
// for settings naming a binary to execute, a way to run code on other people's
// machines through an ordinary pull request.
//
// Rejection ignores the layer rather than failing the load. Erroring would let
// one committed file brick every Entire command in the repo — including
// `status` and `doctor`, the tools needed to diagnose it.
const localLayerTrackedReason = "it is tracked in git, so it is not local to this clone — it arrives with the repository and overrides project settings for everyone"

// enforceOPFCommandTrust decides the effective
// redaction.openai_privacy_filter.command: the user settings file wins
// outright; failing that, a local file positively verified as this
// developer's own; failing that, the command is dropped.
//
// The command becomes argv[0] of an exec in the pre-push OPF rewrite (see
// redact.ConfigurePrivacyFilter -> newShellOut), so whoever controls the string
// controls what Entire runs on the developer's machine. .entire/settings.json
// is version-controlled, so honoring it from there would let an ordinary pull
// request carry a command pointing at a payload committed alongside it — and a
// JSON settings diff does not read as executable to a reviewer. The pre-push
// prompt is no defense: it never names the command, prompt_default "always"
// skips it, and non-TTY pushes auto-run.
//
// The user file (~/.config/entire/settings.json) is the supported location.
// It is the one root that is the developer's by construction — no clone,
// submodule, or symlink can put content there — so a command from it needs no
// ownership probe at all: no git index read, no HEAD read. Only the location
// rule below still applies to it.
//
// The local file is the deprecated location. localData is its raw bytes, nil
// when the file is absent or its layer was dropped as tracked. When the
// command comes from there, this re-verifies with the deep (index AND HEAD)
// check and requires localOwn: unlike the layer as a whole, an unverifiable
// repository fails CLOSED here, because the cost of being wrong is executing
// an attacker's binary rather than losing a preference. The honored command is
// tagged OPFCommandSourceLocal so the pre-push consumer can point at the new
// home.
//
// Rejection is a downgrade, never an error: Command resets to "" and
// ConfigurePrivacyFilter falls back to resolving "opf" on $PATH, where Go's
// exec.LookPath refuses a match from the current directory (exec.ErrDot). That
// protection covers $PATH lookups only: a command that itself contains a path
// separator never consults $PATH, so a `./…` or worktree-absolute command is
// rejected here on location, independent of ownership and of which file named
// it. If the fallback binary is missing, the pre-push rewrite fails closed
// rather than pushing content the user believed OPF had scanned.
func enforceOPFCommandTrust(ctx context.Context, s *EntireSettings, localSettingsPath string, localData []byte, userOPF *repopolicy.UserOPFConfig) {
	if s == nil || s.Redaction == nil || s.Redaction.OpenAIPrivacyFilter == nil {
		return
	}
	worktreeRoot := filepath.Dir(filepath.Dir(localSettingsPath))
	if userOPF != nil && userOPF.Command != "" {
		applyUserOPFCommand(s.Redaction.OpenAIPrivacyFilter, userOPF.Command, worktreeRoot)
		return
	}
	if s.Redaction.OpenAIPrivacyFilter.Command == "" {
		return
	}
	localVerified := classifyLocalSettingsDeep(ctx, localSettingsPath) == localOwn
	enforceOPFCommandTrustForVerifiedData(s, localData, localVerified, worktreeRoot)
}

// applyUserOPFCommand installs a command from the user settings file. It
// supersedes whatever the project or local file set (the project value was
// never honored; the local value is the deprecated location), subject only to
// the location rule — the user's own file can still name a binary that lives
// inside the repository, and that binary is repo-deliverable regardless of
// which file pointed at it.
func applyUserOPFCommand(opf *OPFSettings, command, worktreeRoot string) {
	if commandInsideWorktree(command, worktreeRoot) {
		opf.rejectedCommand = command
		opf.rejectionReason = "it points inside the repository worktree; install the binary outside the repo or use a $PATH name"
		opf.Command = ""
		opf.commandSource = ""
		return
	}
	opf.Command = command
	opf.commandSource = OPFCommandSourceUser
}

// enforceOPFCommandTrustForVerifiedData applies the ownership verdict and the
// location rule. worktreeRoot may be "" when unknown (the location rule is
// then skipped; ownership still decides).
func enforceOPFCommandTrustForVerifiedData(s *EntireSettings, localData []byte, localVerified bool, worktreeRoot string) {
	if s == nil || s.Redaction == nil || s.Redaction.OpenAIPrivacyFilter == nil {
		return
	}
	opf := s.Redaction.OpenAIPrivacyFilter
	if opf.Command == "" {
		return
	}

	var reason string
	switch {
	case !localSetsOPFCommand(localData):
		reason = "it did not come from .entire/settings.local.json"
	case !localVerified:
		reason = "the local settings file could not be verified as untracked"
	case commandInsideWorktree(opf.Command, worktreeRoot):
		// Ownership of the settings file is one gate; the binary's location
		// is the other. An executable that lives inside the repository is by
		// definition deliverable through it, whichever probe shape a clone
		// used to make the settings file look locally owned — so it is never
		// run, even from a genuinely untracked local file.
		reason = "it points inside the repository worktree; install the binary outside the repo or use a $PATH name"
	default:
		opf.commandSource = OPFCommandSourceLocal
		return
	}

	// Record rather than log, for two reasons. A log line in
	// .entire/logs/entire.log is not a signal the user will see, and the
	// consumer can put this on stderr where it belongs. And the loader is
	// reached while the cli package resolves the log level, so keeping it
	// log-free keeps it off any future re-entrancy path into the logger
	// (Init no longer calls out under its lock, but the loader should not
	// depend on that).
	opf.rejectedCommand = opf.Command
	opf.rejectionReason = reason
	opf.Command = ""
}

// commandInsideWorktree reports whether an OPF command names an executable
// under the repository worktree. A bare name (no separator) is a $PATH lookup
// and is left alone; a relative path resolves against the hook's working
// directory, which git sets to the worktree root; an absolute path is compared
// after symlink resolution on both sides.
func commandInsideWorktree(command, worktreeRoot string) bool {
	if worktreeRoot == "" || !strings.ContainsAny(command, `/\`) {
		return false
	}
	target := filepath.FromSlash(command)
	if !filepath.IsAbs(target) {
		target = filepath.Join(worktreeRoot, target)
	}
	target = canonicalizeBestEffort(target)
	root := canonicalizeBestEffort(worktreeRoot)
	return gitpath.Equivalent(target, root) || strings.HasPrefix(target, root+string(filepath.Separator))
}

// canonicalizeBestEffort resolves symlinks in the deepest existing ancestor of
// p and re-appends the rest, so a path whose final components do not exist yet
// (the binary need not be installed) still compares against the canonical
// worktree root — /var vs /private/var on macOS, for instance.
func canonicalizeBestEffort(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// localSetsOPFCommand reports whether the local override file explicitly sets
// redaction.openai_privacy_filter.command.
//
// Presence of the key is what matters, not the merged value: the project file
// and the local file may set the same string, and comparing effective values
// after the merge would misattribute that to the project layer. A malformed
// local file yields false, which is the safe direction — the merge itself
// fails separately with a parse error.
func localSetsOPFCommand(data []byte) bool {
	return localSetsOPFKey(data, "command")
}

// localSetsOPFKey reports whether the local override file explicitly sets
// redaction.openai_privacy_filter.<key>. Same presence-not-value contract as
// localSetsOPFCommand; nil data (absent or dropped layer) sets nothing.
func localSetsOPFKey(data []byte, key string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	return rawHasKey(raw, "redaction", "openai_privacy_filter", key)
}

// rawHasKey reports whether a nested key is present in a decoded JSON object,
// walking objects along the way. A missing or non-object link yields false.
//
// Presence is the question, not value: two layers may set the same string, so
// comparing effective values after a merge would misattribute provenance.
//
// The first key is a separate parameter rather than part of the variadic so
// that "no key at all" cannot be expressed. A plain `path ...string` accepted
// an empty call that indexed out of range at run time — a latent panic in a
// predicate that gates a security boundary. This shape makes the compiler
// reject it instead of relying on every caller to pass enough arguments.
func rawHasKey(parent map[string]json.RawMessage, first string, rest ...string) bool {
	key := first
	for _, next := range rest {
		parent = rawObject(parent, key)
		key = next
	}
	_, ok := parent[key]
	return ok
}

// rawObject returns parent[key] decoded as a JSON object, or nil when the key
// is absent or does not hold an object. Indexing the nil result is safe, so
// lookups can be chained without intermediate checks.
func rawObject(parent map[string]json.RawMessage, key string) map[string]json.RawMessage {
	v, ok := parent[key]
	if !ok {
		return nil
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(v, &out); err != nil {
		return nil
	}
	return out
}

// localTrust is the loader's verdict on .entire/settings.local.json.
//
// localUnverifiable is the zero value so a forgotten assignment fails in the
// safe direction. The two consumers use it with deliberately opposite
// policies: the layer as a whole is dropped only on localTracked (proof
// required, because losing every local setting over an unreadable repository
// is the worse failure), while the exec-bearing OPF command requires localOwn
// and treats localUnverifiable as hostile.
type localTrust int

const (
	localUnverifiable localTrust = iota // repository could not be read
	localTracked                        // proven present in the index (or HEAD, for a deep check)
	localOwn                            // verified not versioned, or no repository to clone from
)

// classifyLocalSettings checks the git index only.
//
// The index is what a delivered attack shows up in: a pull request that
// commits .entire/settings.local.json puts it in the index of every clone that
// checks the branch out. HEAD is deliberately NOT consulted here. The only
// state the index misses is "committed, then git rm --cached", and checkout
// does not produce a file that is absent from the index — so that state is one
// the local developer created, not one that arrived with the repository.
//
// Skipping HEAD is what keeps this affordable for the broad population. On a
// packed repository the HEAD commit+tree lookup costs more than the index
// parse, and on a reftable repository it re-enters the git CLI (gitrepo routes
// reference reads through a subprocess there), which is ~150x the cost of the
// index check.
func classifyLocalSettings(ctx context.Context, path string) localTrust {
	return classify(ctx, path, false)
}

// classifyLocalSettingsDeep also checks HEAD, catching content that is
// committed but no longer staged. Reserved for the OPF command, where being
// wrong means executing someone else's binary and the extra cost is paid only
// by the few users who set that field locally.
func classifyLocalSettingsDeep(ctx context.Context, path string) localTrust {
	return classify(ctx, path, true)
}

func classify(ctx context.Context, path string, deep bool) localTrust {
	switch versioned, err := localSettingsIsVersioned(ctx, path, deep); {
	case err != nil:
		return localUnverifiable
	case versioned:
		return localTracked
	default:
		return localOwn
	}
}

// probeKey memoizes the shallow and deep answers separately: the deep probe
// subsumes the shallow one, but they are asked by different callers and only
// the rare OPF path pays for HEAD.
type probeKey struct {
	path string
	deep bool
}

// versionedPaths memoizes localSettingsIsVersioned for the process lifetime.
//
// settings.Load has no caching of its own and runs ~5 times per hook, so an
// unmemoized probe multiplies one repository read into five. Whether a path is
// tracked cannot change inside a single short-lived hook process. Only
// successful determinations are cached; an error means "could not verify" and
// may be transient.
//
// Process-scoped, which is right for hooks but stale for a long-lived `entire
// mcp` server: a developer who untracks the file mid-session keeps the old
// verdict until restart. ClearVersionedPathCache exists for tests; see the
// context-scoped precedent in strategy/git_remote_cache.go if this ever needs
// to become request-scoped.
var (
	versionedPathsMu sync.Mutex
	versionedPaths   = map[probeKey]bool{}
)

// ClearVersionedPathCache drops the memoized trackedness verdicts. Tests that
// change a file's tracked state within one process must call it; every other
// process-wide cache in this codebase ships the same seam.
func ClearVersionedPathCache() {
	versionedPathsMu.Lock()
	defer versionedPathsMu.Unlock()
	clear(versionedPaths)
}

// localSettingsIsVersioned reports whether the local settings file is tracked
// in the index, and with deep set, whether it is also present in HEAD.
//
// An error means "could not determine" (unreadable repository, unreadable
// index) and callers must treat it as untrusted rather than as a negative.
func localSettingsIsVersioned(ctx context.Context, path string, deep bool) (bool, error) {
	key := probeKey{path: path, deep: deep}

	versionedPathsMu.Lock()
	cached, ok := versionedPaths[key]
	versionedPathsMu.Unlock()
	if ok {
		return cached, nil
	}

	versioned, err := probeLocalSettingsIsVersioned(ctx, path, deep)
	if err != nil {
		return false, err
	}

	versionedPathsMu.Lock()
	versionedPaths[key] = versioned
	versionedPathsMu.Unlock()
	return versioned, nil
}

// probeLocalSettingsIsVersioned answers through go-git rather than the git CLI.
// Beyond avoiding a subprocess, this removes a dependency on `git` being on
// $PATH for ordinary repositories — the population running GUI git clients,
// whose hooks do not inherit a shell profile, is precisely the one that needs
// an explicit OPF command and would otherwise fail verification and lose it.
// (Reftable repositories are the exception: gitrepo routes reference reads
// through the git CLI, so the deep check still needs the binary there.)
//
// path is always <worktreeRoot>/.entire/settings.local.json, so the worktree
// root is its grandparent and the repo-relative key is the known constant —
// no git call is needed to resolve either. That coupling is load-bearing: if
// paths.AbsPath ever fell back to a bare relative path from a subdirectory,
// the grandparent would name the wrong directory. It holds today because
// readConfined can only have succeeded on the relative form when the working
// directory IS the worktree root.
func probeLocalSettingsIsVersioned(ctx context.Context, path string, deep bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("verify %s: %w", EntireSettingsLocalFile, err)
	}

	// A symlink at `.entire` or at the file itself defeats the index probe:
	// the literal path is absent from the index while os.ReadFile follows the
	// link into whatever tracked content it points at (a committed
	// `.entire -> payload` ships an OPF command through a fresh clone). Treat
	// the file as not locally owned — the same downgrade as a tracked file.
	for _, candidate := range []string{filepath.Dir(path), path} {
		info, lstatErr := os.Lstat(candidate)
		if lstatErr != nil {
			if errors.Is(lstatErr, fs.ErrNotExist) {
				continue
			}
			return false, fmt.Errorf("inspect %s: %w", candidate, lstatErr)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return true, nil
		}
	}
	// A submodule (or nested repository) checked out AT `.entire` is a real
	// directory, so the symlink probe passes, while the superproject's index
	// and HEAD record only a gitlink named `.entire` — the file's own path
	// appears in neither. Its contents arrived by `git clone
	// --recurse-submodules` all the same. `.entire/.git` (a file for a
	// submodule, a directory for a nested clone) is the filesystem tell; the
	// index and HEAD scans below catch the gitlink itself.
	if _, statErr := os.Lstat(filepath.Join(filepath.Dir(path), ".git")); statErr == nil {
		return true, nil
	}

	repo, err := gitrepo.OpenPath(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		// No repository at all (no .git). A file cannot have arrived by
		// cloning when there is nothing to clone, so this is a definitive
		// "not versioned", not a verification failure. Treating it as a
		// failure would drop the local layer for every non-repo invocation.
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("open repository: %w", err)
	}
	defer func() { _ = repo.Close() }()

	// Index. Paths are stored slash-separated, which EntireSettingsLocalFile
	// already is, on every platform.
	idx, err := repo.Storer.Index()
	if err != nil {
		return false, fmt.Errorf("read index: %w", err)
	}
	// An entry NAMED `.entire` — a gitlink, or a blob standing where the
	// directory should be — means the directory itself is repository content,
	// so anything under it is too.
	entireDir := filepath.ToSlash(filepath.Dir(EntireSettingsLocalFile))
	for _, entry := range idx.Entries {
		if pathsEqualFold(entry.Name, EntireSettingsLocalFile) || pathsEqualFold(entry.Name, entireDir) {
			return true, nil
		}
	}
	if !deep {
		return false, nil
	}

	// HEAD. An unborn HEAD cannot contain the file, so that is a definitive
	// negative rather than a verification failure.
	head, err := repo.Head()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("resolve HEAD: %w", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return false, fmt.Errorf("read HEAD commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return false, fmt.Errorf("read HEAD tree: %w", err)
	}
	return treeHasPathFold(tree, strings.Split(EntireSettingsLocalFile, "/"))
}

// pathsEqualFold reports whether two repo-relative git paths can name the same
// working-tree file once the filesystem has had its say.
//
// The comparison must NOT be exact, because git records the committed name
// while readConfined opens the canonical one, and several filesystems map the
// two together:
//
//   - Case. On a case-insensitive volume (the macOS and Windows default) a pull
//     request can commit `.entire/Settings.Local.json`; checkout materializes a
//     file that readConfined opens through the canonical name.
//   - Trailing dots and spaces. Win32 strips them, so a committed
//     `.entire/settings.local.json.` also lands on disk under the canonical
//     name. This is the class core.protectNTFS defends, but that setting only
//     guards .git-family names, not ours.
//
// Either way the file would be read as settings while an exact lookup judged
// it untracked — precisely the bypass this gate exists to prevent, and for the
// trailing-dot case reachable through an ordinary pull request, since it
// defeats the index check rather than only the HEAD one.
//
// Normalizing unconditionally rather than probing the platform is deliberate:
// no variant of this filename is ever legitimate content, so treating one as
// tracked on Linux too costs at most an ignored local layer plus a warning,
// and it keeps the security-relevant path free of a capability check that
// could itself be wrong.
//
// Residual limits worth knowing. This covers Unicode simple case folding and
// the Win32 trailing-character rule, not every equivalence a filesystem might
// apply (NFC/NFD, for instance); the filename is pure ASCII, so those are not
// the realistic vectors. NTFS alternate data streams are a read-side suffix
// rather than a committable name, and 8.3 short names are generated aliases
// for long names rather than the reverse, so neither is a route in.
func pathsEqualFold(a, b string) bool {
	return gitpath.Equivalent(a, b)
}

// treeHasPathFold reports whether any fold-matching chain of tree entries
// spells parts. object.Tree.FindEntry matches exactly, so it cannot be used.
//
// It must try EVERY fold-matching entry at each level, not just the first.
// Git objects are case-sensitive, so one tree can legitimately hold both
// `.Entire` and `.entire`, and git sorts uppercase first — so a decoy that
// sorts earlier (a blob by that name, or a directory missing the child) would
// otherwise mask the real sibling and produce a false negative. That is the
// "any variant is tracked" semantics the index scan already has; the two sides
// of this predicate must not disagree.
func treeHasPathFold(tree *object.Tree, parts []string) (bool, error) {
	if len(parts) == 0 {
		return false, nil
	}
	part, rest := parts[0], parts[1:]

	for i := range tree.Entries {
		entry := &tree.Entries[i]
		if !pathsEqualFold(entry.Name, part) {
			continue
		}
		if len(rest) == 0 {
			return true, nil
		}
		// A gitlink where a directory is expected: the whole subtree is a
		// submodule's content. tree.Tree would report ErrDirectoryNotFound
		// (the commit object lives in the submodule's store) and the
		// decoy-sibling `continue` below would read that as "not tracked".
		if entry.Mode == filemode.Submodule {
			return true, nil
		}
		sub, err := tree.Tree(entry.Name)
		if err != nil {
			if errors.Is(err, object.ErrDirectoryNotFound) {
				continue // a blob where a directory was expected: try siblings
			}
			return false, fmt.Errorf("read %s in HEAD: %w", entry.Name, err)
		}
		found, err := treeHasPathFold(sub, rest)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}
