package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// ErrUserHooksNotInUse is an agent's user-level install finding no config
// directory for the agent: it is not in use on this machine, so nothing was
// installed, and nothing needed to be. The directory is never created for it.
var ErrUserHooksNotInUse = errors.New("agent config directory not found")

// userHooksNotInUseError is ErrUserHooksNotInUse naming the directory that
// was looked for.
type userHooksNotInUseError struct{ dir string }

func (e userHooksNotInUseError) Error() string        { return e.dir + " not found" }
func (e userHooksNotInUseError) Is(target error) bool { return target == ErrUserHooksNotInUse }

// UserHooksMissingDir returns the agent config directory an
// ErrUserHooksNotInUse error looked for, or "".
func UserHooksMissingDir(err error) string {
	var notInUse userHooksNotInUseError
	if errors.As(err, &notInUse) {
		return notInUse.dir
	}
	return ""
}

// UserHookState is what InspectUserHooks found in an agent's user-level
// config. Report-only: nothing acts on it but doctor and status.
type UserHookState uint8

const (
	// UserHooksNotInUse: the agent's config directory does not exist.
	UserHooksNotInUse UserHookState = iota
	// UserHooksAbsent: the agent is in use, without Entire's user-level hooks.
	UserHooksAbsent
	// UserHooksCurrent: Entire's user-level hooks match this CLI.
	UserHooksCurrent
	// UserHooksOutdated: Entire's user-level hooks are from another CLI; the
	// next foreground command with global enable on rewrites them.
	UserHooksOutdated
	// UserHooksUnavailable: the config could not be read or parsed.
	UserHooksUnavailable
)

// UserHookReport is one agent's user-level hook health.
type UserHookReport struct {
	Path  string
	State UserHookState
	// Version is the Entire hook version stamped on the installed entry point
	// (0 when absent or unstamped).
	Version int
	// Notes are agent-specific findings, such as Codex hooks awaiting
	// approval in /hooks.
	Notes []string
	Err   error
}

// UserHookInstaller is implemented by agents whose Entire hooks can also live
// in the agent's user-level config, where the agent loads them in every
// project. It is how `global_enable` reaches repositories that were never
// enabled: without a hook the agent never calls Entire there.
//
// A repository's own Entire install for the agent overrides the user-level
// one. Agents that load both copies (Codex, Pi, OpenCode) make the user-level
// entry point step aside where a project install exists; Claude Code runs an
// identical hook command once itself.
//
// Built-in only: external agents own their hook config.
type UserHookInstaller interface {
	Agent

	// EnsureUserHooks installs or upgrades Entire's user-level hooks, keeping
	// everything else in the file. It installs only while wanted still holds
	// once the file's lock is taken (nil: always), so `entire disable
	// --global`, which turns the flag off before removing, cannot be undone
	// by a command that read the flag just before. Returns how many entries
	// changed (0 when already current) and the file's path, or
	// ErrUserHooksNotInUse when the agent's config directory is absent.
	EnsureUserHooks(ctx context.Context, wanted func(context.Context) bool) (int, string, error)

	// RemoveUserHooks takes Entire's user-level hooks out, keeping everything
	// else, and reports whether there were any.
	RemoveUserHooks(ctx context.Context) (bool, string, error)

	// InspectUserHooks reports the user-level hooks' health without writing.
	InspectUserHooks(ctx context.Context) UserHookReport

	// ProjectHooksSupersede reports whether the current checkout has its own
	// Entire install for this agent that the agent will run, so the
	// user-level entry point must do nothing. Checked fresh on every event by
	// `entire hooks <agent> --scope=user`.
	ProjectHooksSupersede(ctx context.Context) bool
}

// AsUserHookInstaller returns the agent as UserHookInstaller if it implements
// it. Built-in only, like HookFreshness.
func AsUserHookInstaller(ag Agent) (UserHookInstaller, bool) {
	return builtinCapability[UserHookInstaller](ag)
}

// EnsureUserHookFile runs the shared shape of every user-level install. apply
// computes the change; with write false it must not write and returns how
// many entries it would change. Every Entire command runs this while global
// enable is on, so the usual case, hooks already current, is settled by that
// read alone: no lock, no write. Otherwise the read-merge-write is serialized
// on lockPath across Entire processes, and redone under the lock only while
// wanted still holds.
func EnsureUserHookFile(ctx context.Context, lockPath string, wanted func(context.Context) bool, apply func(write bool) (int, error)) (int, error) {
	n, err := apply(false)
	if err != nil || n == 0 {
		return 0, err
	}
	release, err := flock.AcquireContext(ctx, lockPath)
	if err != nil {
		return 0, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	defer release()
	if wanted != nil && !wanted(ctx) {
		return 0, nil
	}
	return apply(true)
}

// LockUserHookFile takes the lock EnsureUserHookFile uses, for removal.
func LockUserHookFile(ctx context.Context, lockPath string) (func(), error) {
	release, err := flock.AcquireContext(ctx, lockPath)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	return release, nil
}

// userHookStampName is the Entire-owned side file, in the user config root,
// recording the Entire hook version of an agent's user-level hooks. Used by
// agents that cannot stamp their own entry point: Claude Code runs a hook
// present in both its user and project settings once only when the two
// commands are byte-identical, so a version in the command would make the
// user-level copy fire beside every project copy.
func userHookStampName(name types.AgentName) string {
	return "user_hooks_" + string(name) + ".json"
}

type userHookStamp struct {
	Version int `json:"version"`
}

// WriteUserHookStamp records version for name's user-level hooks.
func WriteUserHookStamp(name types.AgentName, version int) error {
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return fmt.Errorf("open user config directory: %w", err)
	}
	data, err := jsonutil.MarshalIndentWithNewline(userHookStamp{Version: version}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal user hook stamp: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, userHookStampName(name), data, 0o600); err != nil {
		return fmt.Errorf("write user hook stamp: %w", err)
	}
	return nil
}

// ReadUserHookStamp returns the recorded version for name's user-level hooks,
// or 0 when there is none.
func ReadUserHookStamp(name types.AgentName) int {
	root, err := userdirs.ConfigRootForRead()
	if err != nil {
		return 0
	}
	f, err := root.Open(userHookStampName(name))
	if err != nil {
		return 0
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return 0
	}
	var stamp userHookStamp
	if json.Unmarshal(data, &stamp) != nil {
		return 0
	}
	return stamp.Version
}

// RemoveUserHookStamp deletes name's stamp; a missing one is not an error.
func RemoveUserHookStamp(name types.AgentName) error {
	root, err := userdirs.ConfigRootForRead()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open user config directory: %w", err)
	}
	if err := root.Remove(userHookStampName(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove user hook stamp: %w", err)
	}
	return nil
}

// UserConfigFile is a file in an agent's user-level config. Unlike a
// worktree file, a symlink here is the user's own arrangement (a dotfiles
// checkout), so a write goes through it rather than replacing it; a link that
// does not resolve is refused rather than replaced by a regular file. See
// "User-level agent config" in docs/development/filesystem-safety.md.
type UserConfigFile struct{ Path string }

// Read returns the file's content. Callers classify fs.ErrNotExist.
func (u UserConfigFile) Read() ([]byte, error) {
	return os.ReadFile(u.Path) //nolint:wrapcheck // callers classify not-exist
}

// Write replaces the file's content atomically, through a symlink, keeping
// the mode the user gave an existing file. The parent directory must exist.
func (u UserConfigFile) Write(data []byte, perm os.FileMode) error {
	target, err := u.resolve()
	if err != nil {
		return err
	}
	if info, err := os.Stat(target); err == nil {
		perm = info.Mode().Perm()
	}
	if err := jsonutil.WriteFileAtomic(target, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", u.Path, err)
	}
	return nil
}

// Remove deletes the file, or the file a symlink there points at. A missing
// file is not an error.
func (u UserConfigFile) Remove() error {
	target, err := u.resolve()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", u.Path, err)
	}
	return nil
}

// resolve returns the path to write: the file itself, or the target of a
// symlink there, refusing a link that does not resolve.
func (u UserConfigFile) resolve() (string, error) {
	info, err := os.Lstat(u.Path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && info.Mode()&os.ModeSymlink == 0) {
		return u.Path, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", u.Path, err)
	}
	resolved, err := filepath.EvalSymlinks(u.Path)
	if err != nil {
		return "", fmt.Errorf("resolve symlink %s: %w", u.Path, err)
	}
	return resolved, nil
}
