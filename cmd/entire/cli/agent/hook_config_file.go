package agent

import (
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// HookConfigFile is an agent's hook-configuration file inside the worktree —
// .claude/settings.json, .cursor/hooks.json, .gemini/settings.json,
// .github/hooks/entire.json, .factory/settings.json, .codex/hooks.json,
// .opencode/plugin/entire.ts.
//
// Seven agents each carried the same four lines: filepath.Join a repo root with
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
// refused by name instead of silently followed. It also collapses the seven
// copies into one, which is what keeps the next agent integration from
// reintroducing the pattern.
//
// It deliberately does NOT refuse a symlinked file at the leaf. A developer
// pointing `.claude/settings.json` at a dotfile repo is a real setup and Entire
// has no business breaking it; what must not happen is Entire CREATING the path
// through a link it did not put there.
type HookConfigFile struct {
	root *os.Root
	name string
	path string
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
	return osroot.ReadFile(f.root, f.name) //nolint:wrapcheck // see doc comment
}

// Exists reports whether the file is present.
func (f *HookConfigFile) Exists() bool {
	_, err := f.root.Lstat(f.name)
	return err == nil
}

// Write writes data, creating parent directories. A parent that already exists
// as a symlink is refused with osroot.ErrSymlinkedPath rather than followed.
func (f *HookConfigFile) Write(data []byte, perm os.FileMode) error {
	if dir := path.Dir(f.name); dir != "." {
		if err := osroot.MkdirAllNoSymlink(f.root, dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", path.Dir(f.path), err)
		}
	}
	if err := osroot.WriteFile(f.root, f.name, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	return nil
}

// Remove deletes the file. A file that is not there is not an error.
func (f *HookConfigFile) Remove() error {
	if err := osroot.Remove(f.root, f.name); err != nil {
		return fmt.Errorf("remove %s: %w", f.path, err)
	}
	return nil
}

// Root exposes the underlying root and the file's name inside it, for the one
// caller that needs the primitives rather than Read: Codex re-Stats the file
// before and after opening it and compares os.SameFile, which Read cannot
// express. Everything else must use Read/Write/Remove.
//
// The root is owned by the shared registry — do not close it.
func (f *HookConfigFile) Root() (*os.Root, string) { return f.root, f.name }

// GeneratedState is GeneratedHookFileState for a file read through this root.
// See that function for what marker and render mean.
func (f *HookConfigFile) GeneratedState(marker, render string) HookConfigState {
	data, err := f.Read()
	if err != nil {
		return HooksAbsent
	}
	return generatedStateFromContent(string(data), marker, render)
}
