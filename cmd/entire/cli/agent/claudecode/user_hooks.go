package claudecode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// ErrClaudeNotInUse is EnsureUserHooks finding no Claude config directory:
// nothing was installed, and nothing needed to be.
var ErrClaudeNotInUse = errors.New("no Claude Code config directory")

// EnsureUserHooks installs Entire's hooks into the user-level Claude Code
// settings ($CLAUDE_CONFIG_DIR/settings.json, else ~/.claude/settings.json),
// which Claude Code reads in every project. It is how `global_enable` (set by
// `entire enable --global`) reaches repositories that were never enabled:
// without a hook, Claude never calls Entire there.
//
// The merge is the one `entire enable` uses for the project file, so the
// commands are byte-identical and Claude Code runs a hook present in both
// files once. Returns the number of hooks added (0 when already present) and
// the file's path, or ErrClaudeNotInUse when there is no Claude config
// directory.
func EnsureUserHooks(ctx context.Context) (int, string, error) {
	path, err := userClaudeSettingsPath()
	if err != nil {
		return 0, "", err
	}
	// No Claude config directory means Claude Code is not in use here; do
	// not create one for it.
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, fs.ErrNotExist) {
		return 0, path, ErrClaudeNotInUse
	}
	cfg := userClaudeSettings{path: path}
	// Every command runs this while the flag is on, so the usual case, hooks
	// already in, is settled by a read alone: no lock, no write.
	if _, err := installUserHooks(dryRunUserSettings{cfg}); !errors.Is(err, errWriteNeeded) {
		return 0, path, err
	}
	// Entire processes run this concurrently, so the read-merge-write is
	// serialised, and repeated under the lock.
	release, err := flock.AcquireContext(ctx, path+".lock")
	if err != nil {
		return 0, path, fmt.Errorf("lock %s: %w", path, err)
	}
	defer release()
	n, err := installUserHooks(cfg)
	return n, path, err
}

// installUserHooks merges Entire's hooks into cfg. A file that exists but
// cannot be read must not be mistaken for an absent one: the install merge
// would then replace everything in it.
func installUserHooks(cfg claudeSettingsFile) (int, error) {
	if _, err := cfg.Read(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("read %s: %w", cfg.Path(), err)
	}
	return installHooksIn(cfg, false)
}

// errWriteNeeded is dryRunUserSettings refusing the write the merge asked for.
var errWriteNeeded = errors.New("user-level Claude settings need Entire's hooks")

// dryRunUserSettings runs the install merge without writing: Write reports
// that one is needed.
type dryRunUserSettings struct{ userClaudeSettings }

func (dryRunUserSettings) Write([]byte, os.FileMode) error { return errWriteNeeded }

// userClaudeSettings is the user's own config file. Unlike a worktree file, a
// symlink here is the user's arrangement (a dotfiles checkout), so the write
// goes through it rather than replacing it; see "User-level Claude Code
// settings" in docs/development/filesystem-safety.md. A link that does not
// resolve is refused.
type userClaudeSettings struct{ path string }

func (u userClaudeSettings) Path() string { return u.path }

func (u userClaudeSettings) Read() ([]byte, error) {
	return os.ReadFile(u.path) //nolint:wrapcheck // callers classify not-exist
}

func (u userClaudeSettings) Write(data []byte, perm os.FileMode) error {
	target := u.path
	if info, err := os.Lstat(u.path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		// A link that does not resolve (a dotfiles checkout not there yet)
		// is refused rather than replaced by a regular file.
		resolved, err := filepath.EvalSymlinks(u.path)
		if err != nil {
			return fmt.Errorf("resolve symlink %s: %w", u.path, err)
		}
		target = resolved
	}
	// The user chose this file's mode; keep it rather than imposing the
	// project file's 0o600.
	if info, err := os.Stat(target); err == nil {
		perm = info.Mode().Perm()
	}
	if err := jsonutil.WriteFileAtomic(target, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", u.path, err)
	}
	return nil
}
