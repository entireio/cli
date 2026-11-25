package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
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
	// once the lock is taken (nil: always), so `entire disable --global`,
	// which turns the flag off before removing, cannot be undone by a command
	// that read the flag just before. The result always names the file; the
	// error is ErrUserHooksNotInUse when the agent's config directory is
	// absent.
	EnsureUserHooks(ctx context.Context, wanted func(context.Context) bool) (UserHookInstall, error)

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

// UserHookInstall is one EnsureUserHooks outcome.
type UserHookInstall struct {
	Path string
	// Changed is how many entries the install changed; 0 when already current.
	Changed int
	// Updated: the install replaced Entire hooks already there (an older
	// render, or one the user edited) rather than adding them.
	Updated bool
}

// userHookState is Entire's own record, in its user config directory, of what
// it wrote into agents' user-level config: the file and directories it
// created (removal deletes only those) and the hook version of a file that
// cannot carry one. Its lock serializes every user-level write across Entire
// processes. Nothing Entire keeps for itself goes in an agent's directory,
// and once no record is left the state and lock files go too.
type userHookState struct {
	// open replaces the Entire user config directory (tests).
	open func() (*os.Root, error)
}

const (
	userHookStateFile = "user-hooks.json"
	userHookLockFile  = "user-hooks.lock"
)

// userHookRecord is what Entire recorded about one user-level file, keyed by
// the file's path.
type userHookRecord struct {
	CreatedFile bool     `json:"created_file,omitempty"`
	CreatedDirs []string `json:"created_dirs,omitempty"`
	Version     int      `json:"version,omitempty"`
}

func (s userHookState) root(create bool) (*os.Root, error) {
	switch {
	case s.open != nil:
		return s.open()
	case create:
		return userdirs.ConfigRoot() //nolint:wrapcheck // names the directory
	default:
		return userdirs.ConfigRootForRead() //nolint:wrapcheck // names the directory
	}
}

func loadUserHookRecords(root *os.Root) map[string]userHookRecord {
	records := map[string]userHookRecord{}
	data, err := osroot.ReadFileNoFollow(root, userHookStateFile)
	if err == nil && json.Unmarshal(data, &records) != nil {
		records = map[string]userHookRecord{}
	}
	return records
}

// record returns path's record without locking, for reports.
func (s userHookState) record(path string) userHookRecord {
	root, err := s.root(false)
	if err != nil {
		return userHookRecord{}
	}
	return loadUserHookRecords(root)[path]
}

// ensure runs the shared shape of every user-level install. apply computes
// the change; with rec nil it must not write and returns how many entries it
// would change. Every Entire command runs this while global enable is on, so
// the usual case, hooks already current, is settled by that read alone: no
// lock, no write. Otherwise the write runs under the lock, only while wanted
// still holds.
func (s userHookState) ensure(ctx context.Context, path string, wanted func(context.Context) bool,
	apply func(rec *userHookRecord) (UserHookInstall, error),
) (UserHookInstall, error) {
	result, err := apply(nil)
	if err != nil || result.Changed == 0 {
		result.Changed = 0
		return result, err
	}
	err = s.locked(ctx, path, func(rec *userHookRecord) error {
		if wanted != nil && !wanted(ctx) {
			result.Changed = 0
			return nil
		}
		var applyErr error
		result, applyErr = apply(rec)
		return applyErr
	})
	return result, err
}

// locked runs fn on path's record under the lock and saves what fn left: an
// empty record is dropped, and with no record left the state and lock files
// are deleted (a process waiting on the lock retries on a new file).
func (s userHookState) locked(ctx context.Context, path string, fn func(rec *userHookRecord) error) error {
	root, err := s.root(true)
	if err != nil {
		return fmt.Errorf("open user config directory: %w", err)
	}
	release, err := flock.AcquireContextIn(ctx, root, userHookLockFile)
	if err != nil {
		return fmt.Errorf("lock %s: %w", userHookLockFile, err)
	}
	defer release()
	records := loadUserHookRecords(root)
	rec := records[path]
	fnErr := fn(&rec)
	delete(records, path)
	if rec.CreatedFile || len(rec.CreatedDirs) > 0 || rec.Version != 0 {
		records[path] = rec
	}
	if len(records) == 0 {
		_ = root.Remove(userHookStateFile) //nolint:errcheck // absent is fine
		_ = root.Remove(userHookLockFile)  //nolint:errcheck // still held; Windows keeps it
		return fnErr
	}
	data, err := json.Marshal(records)
	if err == nil {
		err = jsonutil.WriteFileAtomicIn(root, userHookStateFile, data, 0o600)
	}
	if err != nil && fnErr == nil {
		return fmt.Errorf("record user-level hooks: %w", err)
	}
	return fnErr
}

// removeDirsIfEmpty removes dirs, deepest first, each only while empty and
// never a symlink: a linked directory is the user's arrangement (dotfiles),
// and removing the link would drop it whatever it holds.
func removeDirsIfEmpty(dirs []string) {
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			continue
		}
		_ = os.Remove(dir) // fails, as intended, while anything is in it
	}
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
