package agent

import (
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// HookConfigFile is an agent's hook-configuration file inside the worktree —
// .claude/settings.json, .cursor/hooks.json, .gemini/settings.json,
// .github/hooks/entire.json, .factory/settings.json, .codex/hooks.json,
// .opencode/plugins/entire.ts, .pi/extensions/entire/index.ts.
//
// Eight agents each carried the same four lines: filepath.Join a repo root with
// a fixed relative path, then os.ReadFile / os.MkdirAll / os.WriteFile /
// os.Remove on the result, each with a //nolint:gosec saying the path came from
// the repo root. That justification is about where the path was BUILT and says
// nothing about where it RESOLVES, which is the part that matters: `.claude` and
// its siblings live in the working tree, and a working tree arrives by clone.
// A repository carrying a checked-in symlink at `.claude` therefore had
// `entire enable` create directories and write JSON through it, to wherever it
// pointed — outside the repository included.
//
// Anchoring on worktreedir fixes that at the root rather than at each call site:
// the base is the worktree root, the agent's fixed subpath is a NAME inside it,
// and directories are created with MkdirAllNoSymlink so a symlinked component is
// refused by name instead of silently followed. It also collapses the eight
// copies into one, which is what keeps the next agent integration from
// reintroducing the pattern. Pi was the one that got left behind on the first
// pass and had to be migrated afterwards, which is the argument for routing a
// new agent through here rather than for trusting that someone will notice.
//
// Every symlink component is refused, including the file itself. os.Root blocks
// a link that escapes the worktree but follows one pointing elsewhere inside
// it; accepting the leaf would let `.claude/settings.json -> ../victim.json`
// redirect both the merge read and the subsequent write.
//
// An earlier revision of this type deliberately allowed a symlinked leaf, on the
// grounds that pointing `.claude/settings.json` at a dotfile repo is a real
// setup Entire has no business breaking. That was reconsidered, and the reason
// is worth keeping: the merge READ pulls the target's contents into what Entire
// then writes, and the write is a rename, which replaces the user's link with a
// regular file rather than following it. Both happen silently. Refusing is the
// legible version of the same outcome.
//
// The setup still works, because the thing that makes a link dangerous here is
// that it arrived with the checkout. A developer who wants one keeps it out of
// the repository — `git rm --cached .claude/settings.json` and a .gitignore
// entry — and manages it locally, which is what a dotfile workflow does anyway.
type HookConfigFile struct {
	root *os.Root
	name string
	path string
}

// HookConfigLocator is implemented by an agent whose Entire hook configuration
// is a file inside the worktree: it returns the same worktree-relative,
// slash-separated path the agent passes to OpenHookConfig.
//
// It exists for the callers that must reason about that path WITHOUT doing I/O
// on it. Doctor's symlink diagnosis is the first: the directories BETWEEN an
// agent's own directory and its config file — `.pi/extensions`,
// `.pi/extensions/entire`, `.opencode/plugins` — are created by Entire and
// appear in no other registry, because ProtectedDirs names what the AGENT owns
// (`.pi`, `.opencode`) and stops there. Deriving them from a hand-kept list in
// the caller is how two of them came to be unchecked in the first place.
//
// Optional: an agent whose hooks are not a worktree file (an external plugin
// declaring them in its manifest) does not implement it. See
// TestAllHookConfigRelPaths_CoversEveryWorktreeConfigAgent for the ones that
// must.
type HookConfigLocator interface {
	// HookConfigRelPath returns the config file's path relative to the worktree
	// root, slash-separated, exactly as passed to OpenHookConfig.
	HookConfigRelPath() string
}

// OpenHookConfig returns the hook-config file at relPath inside worktreeRoot.
//
// relPath is slash-separated and relative to the worktree root
// (".cursor/hooks.json"). The directory does not need to exist: Read reports a
// missing file the way os.ReadFile does, and Write creates the parents.
func OpenHookConfig(worktreeRoot, relPath string) (*HookConfigFile, error) {
	name, err := worktreedir.Name(worktreeRoot, relPath)
	if err != nil {
		return nil, fmt.Errorf("resolve hook config path: %w", err)
	}
	root, err := worktreedir.OpenAt(worktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("open worktree root: %w", err)
	}
	return &HookConfigFile{
		root: root,
		name: name,
		path: filepath.Join(worktreeRoot, filepath.FromSlash(name)),
	}, nil
}

// Path is the file's absolute path, for messages and for the agent config that
// has to name it. It is not an invitation to do I/O on the result.
func (f *HookConfigFile) Path() string { return f.path }

// Read returns the file's contents. A missing file is reported unwrapped so
// callers can classify it with os.IsNotExist, which is how every agent's
// install path decides between "merge into existing" and "write fresh".
func (f *HookConfigFile) Read() ([]byte, error) {
	return osroot.ReadFileNoFollow(f.root, f.name) //nolint:wrapcheck // see doc comment
}

// Exists reports whether the file is present at a path Entire will read.
//
// A symlinked parent directory counts as absent rather than as an error, since
// the signature has nowhere to put one. That is the useful answer: every caller
// uses this to choose between merging into an existing file and writing a fresh
// one, and Write refuses the same path with a message that names the link.
func (f *HookConfigFile) Exists() bool {
	info, err := osroot.LstatNoSymlinks(f.root, f.name)
	return err == nil && info.Mode()&os.ModeSymlink == 0
}

// Write writes data, creating parent directories. A parent that already exists
// as a symlink is refused with osroot.ErrSymlinkedPath rather than followed.
func (f *HookConfigFile) Write(data []byte, perm os.FileMode) error {
	if dir := path.Dir(f.name); dir != "." {
		if err := osroot.MkdirAllNoSymlink(f.root, dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(f.path), err)
		}
	}
	if info, err := osroot.LstatNoSymlinks(f.root, f.name); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("write %s: %w", f.path, osroot.ErrSymlinkedPath)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	if err := jsonutil.WriteFileAtomicIn(f.root, f.name, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	return nil
}

// Remove deletes the file. A file that is not there is not an error; a
// symlinked parent directory is, because the file this would delete is at the
// far end of a link Entire did not create.
func (f *HookConfigFile) Remove() error {
	if err := osroot.RemoveNoSymlinks(f.root, f.name); err != nil {
		return fmt.Errorf("remove %s: %w", f.path, err)
	}
	return nil
}

// RemoveDir deletes the directory the file lives in, and everything in it,
// refusing a symlink at any component along the way.
//
// One agent needs this, and it is not a general uninstall: Pi's whole
// integration is `.pi/extensions/entire/index.ts`, a directory that exists only
// to hold that one file, and pi discovers extensions BY DIRECTORY — so removing
// the file alone leaves an empty `.pi/extensions/entire` for pi to find and
// nothing to load from it. Every other agent writes into a directory the agent
// itself owns (`.claude`, `.codex`, `.cursor`), where Remove of the file is the
// correct uninstall and taking the parent would delete the user's own config
// with it.
//
// Enforced rather than described, because "every other agent must not call
// this" was a comment and nothing else, and the call it guards is a recursive
// delete. The precondition is stated positively: the directory has to be one
// ENTIRE named, which is the only kind it created to hold a generated file.
//
// A blocklist of the agents' own directories was the obvious alternative and is
// not sound. AllProtectedDirs() holds `.opencode` and `.github/hooks` but not
// `.opencode/plugins`, `.pi/extensions` or `.github`, so deriving the target
// with path.Dir and checking it against that list still permits
// RemoveAll(".opencode/plugins") — the user's other OpenCode plugins — for any
// future agent whose config sits one level deeper than its root. Every agent
// root and every shared intermediate fails the name test instead, and pi's
// `.pi/extensions/entire` passes it because Entire is what created it.
func (f *HookConfigFile) RemoveDir() error {
	dir := path.Dir(f.name)
	if dir == "." {
		return fmt.Errorf("remove %s: refusing to remove the worktree root", filepath.Dir(f.path))
	}
	if path.Base(dir) != entireOwnedDirName {
		return fmt.Errorf("remove %s: refusing to remove %q, which Entire did not create; "+
			"RemoveDir is only for a directory named %q that holds one generated file",
			filepath.Dir(f.path), path.Base(dir), entireOwnedDirName)
	}
	if err := osroot.RemoveAllNoSymlinks(f.root, dir); err != nil {
		return fmt.Errorf("remove %s: %w", filepath.Dir(f.path), err)
	}
	return nil
}

// entireOwnedDirName is the directory name Entire uses when it has to create a
// directory of its own inside a tree an agent owns (`.pi/extensions/entire`).
// RemoveDir keys its refusal on it.
const entireOwnedDirName = "entire"

// Root exposes the underlying root and the file's name inside it, for the
// callers that need a descriptor rather than the bytes. Both are Codex, which
// bounds .codex/hooks.json on its stat size before reading any of it: Read is
// an unbounded io.ReadAll, so it cannot express a limit, and the file arrives
// with the checkout. Everything else must use Read/Write/Remove.
//
// The root is owned by the shared registry, so do not close it. Callers must go
// through the osroot no-follow primitives rather than resolving name with
// Root.Open, which is what Read does for them.
func (f *HookConfigFile) Root() (*os.Root, string) { return f.root, f.name }

// GeneratedState reports whether a generated hook file is absent, current or
// outdated, for the agents whose whole integration is one file Entire writes
// (Pi, OpenCode). marker is the Entire-managed sentinel that identifies the file
// as ours rather than the user's; render is what the current template would
// write, compared against the file's contents to tell current from outdated.
func (f *HookConfigFile) GeneratedState(marker, render string) HookConfigState {
	data, err := f.Read()
	if err != nil {
		return HooksAbsent
	}
	return generatedStateFromContent(string(data), marker, render)
}
