package claudecode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// EnsureUserHooks installs Entire's hooks into the user-level Claude Code
// settings ($CLAUDE_CONFIG_DIR/settings.json, else ~/.claude/settings.json),
// which Claude Code reads in every project. It is how a machine-wide
// `preferences.enabled` reaches repositories that were never enabled: without
// a hook, Claude never calls Entire there.
//
// The merge is the one `entire enable` uses for the project file, so the
// commands are byte-identical and Claude Code runs a hook present in both
// files once. Returns the number of hooks added (0 when already present) and
// the file's path.
func EnsureUserHooks(_ context.Context) (int, string, error) {
	path, err := userClaudeSettingsPath()
	if err != nil {
		return 0, "", err
	}
	// A file that exists but cannot be read must not be mistaken for an
	// absent one: the install merge would then replace everything in it.
	if _, err := os.ReadFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) { //nolint:gosec // the user's own Claude config
		return 0, path, fmt.Errorf("read %s: %w", path, err)
	}
	n, err := installHooksIn(userClaudeSettings{path: path}, false)
	return n, path, err
}

// userClaudeSettings is the user's own config file. Unlike a worktree file, a
// symlink here is the user's arrangement (a dotfiles checkout), so the write
// goes through it rather than replacing it — the same rule usersettings
// applies to ~/.config/entire/settings.json.
type userClaudeSettings struct{ path string }

func (u userClaudeSettings) Path() string { return u.path }

func (u userClaudeSettings) Read() ([]byte, error) {
	return os.ReadFile(u.path) //nolint:wrapcheck // callers classify not-exist
}

func (u userClaudeSettings) Write(data []byte, perm os.FileMode) error {
	target := u.path
	if resolved, err := filepath.EvalSymlinks(u.path); err == nil {
		target = resolved
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}
	if err := jsonutil.WriteFileAtomic(target, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", u.path, err)
	}
	return nil
}
