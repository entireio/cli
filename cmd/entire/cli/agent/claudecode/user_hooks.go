package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// ErrClaudeNotInUse is EnsureUserHooks finding no Claude config directory:
// nothing was installed, and nothing needed to be.
var ErrClaudeNotInUse = agent.ErrUserHooksNotInUse

var _ agent.UserHookInstaller = (*ClaudeCodeAgent)(nil)

// userHooksVersion is the Entire hook version of the user-level hooks. Bump it
// when what installUserHooks writes changes. It is recorded in an Entire-owned
// side file, never in the hook commands: Claude Code runs a command present in
// both its user and project settings once only when the two are identical.
const userHooksVersion = 1

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
	return EnsureUserHooksWhile(ctx, nil)
}

// EnsureUserHooksWhile is EnsureUserHooks that installs only while wanted
// still holds once the lock is taken (nil: always). `entire disable --global`
// turns the flag off and then removes the hooks under the same lock, so a
// command that read the flag as on just before cannot put them back after.
func EnsureUserHooksWhile(ctx context.Context, wanted func(context.Context) bool) (int, string, error) {
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
	n, err := agent.EnsureUserHookFile(ctx, path+".lock", wanted, func(write bool) (int, error) {
		if !write {
			return dryRunInstallCount(cfg)
		}
		return installUserHooks(cfg)
	})
	// The stamp is written after any install, and once for hooks an earlier
	// CLI installed without one.
	if err == nil && (n > 0 || agent.ReadUserHookStamp(agent.AgentNameClaudeCode) != userHooksVersion) {
		if stampErr := agent.WriteUserHookStamp(agent.AgentNameClaudeCode, userHooksVersion); stampErr != nil {
			logging.Debug(ctx, "claude: could not record the user-level hook version",
				slog.String("error", stampErr.Error()))
		}
	}
	return n, path, err
}

// dryRunInstallCount reports 1 when the install merge would write cfg, else 0.
func dryRunInstallCount(cfg userClaudeSettings) (int, error) {
	_, err := installUserHooks(dryRunUserSettings{cfg})
	if errors.Is(err, errWriteNeeded) {
		return 1, nil
	}
	return 0, err
}

// EnsureUserHooks implements agent.UserHookInstaller.
func (c *ClaudeCodeAgent) EnsureUserHooks(ctx context.Context, wanted func(context.Context) bool) (int, string, error) {
	return EnsureUserHooksWhile(ctx, wanted)
}

// RemoveUserHooks implements agent.UserHookInstaller.
func (c *ClaudeCodeAgent) RemoveUserHooks(ctx context.Context) (bool, string, error) {
	return RemoveUserHooks(ctx)
}

// ProjectHooksSupersede implements agent.UserHookInstaller. Claude Code runs a
// hook command present in both its user and project settings once, so its
// user-level hooks never need to step aside.
func (c *ClaudeCodeAgent) ProjectHooksSupersede(context.Context) bool { return false }

// InspectUserHooks implements agent.UserHookInstaller.
func (c *ClaudeCodeAgent) InspectUserHooks(context.Context) agent.UserHookReport {
	path, err := userClaudeSettingsPath()
	if err != nil {
		return agent.UserHookReport{State: agent.UserHooksUnavailable, Err: err}
	}
	report := agent.UserHookReport{Path: path, Version: agent.ReadUserHookStamp(agent.AgentNameClaudeCode)}
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, fs.ErrNotExist) {
		report.State = agent.UserHooksNotInUse
		return report
	}
	cfg := userClaudeSettings{path: path}
	data, err := cfg.Read()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		report.State = agent.UserHooksAbsent
		return report
	case err != nil:
		report.State, report.Err = agent.UserHooksUnavailable, err
		return report
	case !settingsHaveEntireHooks(data):
		report.State = agent.UserHooksAbsent
		return report
	}
	n, err := dryRunInstallCount(cfg)
	switch {
	case err != nil:
		report.State, report.Err = agent.UserHooksUnavailable, err
	case n > 0 || report.Version < userHooksVersion:
		report.State = agent.UserHooksOutdated
	default:
		report.State = agent.UserHooksCurrent
	}
	return report
}

// RemoveUserHooks takes Entire's hooks out of the user-level Claude Code
// settings, keeping everything else, for `entire disable --global`. It
// reports whether there were any. A missing file or directory, or a file
// without Entire's hooks, is left as it is.
func RemoveUserHooks(ctx context.Context) (bool, string, error) {
	path, err := userClaudeSettingsPath()
	if err != nil {
		return false, "", err
	}
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, fs.ErrNotExist) {
		return false, path, nil
	}
	release, err := agent.LockUserHookFile(ctx, path+".lock")
	if err != nil {
		return false, path, err //nolint:wrapcheck // names the lock file
	}
	defer release()
	if err := agent.RemoveUserHookStamp(agent.AgentNameClaudeCode); err != nil {
		logging.Debug(ctx, "claude: could not remove the user-level hook version",
			slog.String("error", err.Error()))
	}
	cfg := userClaudeSettings{path: path}
	data, err := cfg.Read()
	if errors.Is(err, fs.ErrNotExist) {
		return false, path, nil
	}
	if err != nil {
		return false, path, fmt.Errorf("read %s: %w", path, err)
	}
	if !settingsHaveEntireHooks(data) {
		return false, path, nil
	}
	if err := uninstallHooksIn(cfg); err != nil {
		return false, path, err
	}
	return true, path, nil
}

// settingsHaveEntireHooks reports whether any hook type in a Claude settings
// file runs an Entire command. A hook type whose shape is not Claude's
// matcher list is not Entire's.
func settingsHaveEntireHooks(data []byte) bool {
	var settings struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return true // let the uninstall report the parse error
	}
	for _, raw := range settings.Hooks {
		var matchers []ClaudeHookMatcher
		if json.Unmarshal(raw, &matchers) == nil && hasEntireHook(matchers) {
			return true
		}
	}
	return false
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

// userClaudeSettings is the user's own Claude settings file; see
// agent.UserConfigFile for how it is written.
type userClaudeSettings struct{ path string }

func (u userClaudeSettings) Path() string { return u.path }

func (u userClaudeSettings) Read() ([]byte, error) {
	return agent.UserConfigFile{Path: u.path}.Read() //nolint:wrapcheck // callers classify not-exist
}

func (u userClaudeSettings) Write(data []byte, perm os.FileMode) error {
	return agent.UserConfigFile{Path: u.path}.Write(data, perm) //nolint:wrapcheck // names the file
}
