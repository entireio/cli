package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

var _ agent.UserHookInstaller = (*ClaudeCodeAgent)(nil)

// userHooksVersion is the Entire hook version of the user-level hooks. Bump it
// when what the user-level install writes changes. It is recorded in an
// Entire-owned side file, never in the hook commands: Claude Code runs a
// command present in both its user and project settings once only when the
// two are identical.
const userHooksVersion = 1

// userSettingsFile is the user-level Claude Code settings
// ($CLAUDE_CONFIG_DIR/settings.json, else ~/.claude/settings.json), which
// Claude Code reads in every project. The merge is the one `entire enable`
// uses for the project file, so the commands are byte-identical and Claude
// Code runs a hook present in both files once.
func userSettingsFile() (agent.MergedUserFile, error) {
	path, err := userClaudeSettingsPath()
	if err != nil {
		return agent.MergedUserFile{}, err
	}
	return agent.MergedUserFile{
		Path:      path,
		Perm:      0o600,
		HasEntire: settingsHaveEntireHooks,
		Merge: func(data []byte, exists bool) ([]byte, int, error) {
			f := &memSettingsFile{path: path, data: data, exists: exists}
			n, err := installHooksIn(f, false)
			if err != nil || f.out == nil {
				return nil, 0, err
			}
			// A write that adds nothing (a stale hook dropped, a timeout set)
			// is still a change.
			return f.out, max(n, 1), nil
		},
		Strip: func(data []byte) ([]byte, bool, error) {
			f := &memSettingsFile{path: path, data: data, exists: true}
			if err := uninstallHooksIn(f); err != nil || f.out == nil {
				return nil, false, err
			}
			return f.out, !bytes.Equal(f.out, data), nil
		},
	}, nil
}

// EnsureUserHooks implements agent.UserHookInstaller. It also records the
// version side file after any install, and once for hooks an earlier CLI
// installed without one.
func (c *ClaudeCodeAgent) EnsureUserHooks(ctx context.Context, wanted func(context.Context) bool) (int, string, error) {
	file, err := userSettingsFile()
	if err != nil {
		return 0, "", err
	}
	n, err := file.Ensure(ctx, wanted)
	if err == nil && (n > 0 || agent.ReadUserHookStamp(agent.AgentNameClaudeCode) != userHooksVersion) {
		if stampErr := agent.WriteUserHookStamp(agent.AgentNameClaudeCode, userHooksVersion); stampErr != nil {
			logging.Debug(ctx, "claude: could not record the user-level hook version",
				slog.String("error", stampErr.Error()))
		}
	}
	return n, file.Path, err //nolint:wrapcheck // names the file or its lock
}

// RemoveUserHooks implements agent.UserHookInstaller, for `entire disable
// --global`.
func (c *ClaudeCodeAgent) RemoveUserHooks(ctx context.Context) (bool, string, error) {
	file, err := userSettingsFile()
	if err != nil {
		return false, "", err
	}
	removed, err := file.Remove(ctx, func() {
		if err := agent.RemoveUserHookStamp(agent.AgentNameClaudeCode); err != nil {
			logging.Debug(ctx, "claude: could not remove the user-level hook version",
				slog.String("error", err.Error()))
		}
	})
	return removed, file.Path, err //nolint:wrapcheck // names the file or its lock
}

// ProjectHooksSupersede implements agent.UserHookInstaller. Claude Code runs a
// hook command present in both its user and project settings once, so its
// user-level hooks never need to step aside.
func (c *ClaudeCodeAgent) ProjectHooksSupersede(context.Context) bool { return false }

// InspectUserHooks implements agent.UserHookInstaller. The version comes from
// the side file; hooks older than this CLI's are outdated.
func (c *ClaudeCodeAgent) InspectUserHooks(context.Context) agent.UserHookReport {
	file, err := userSettingsFile()
	if err != nil {
		return agent.UserHookReport{State: agent.UserHooksUnavailable, Err: err}
	}
	report := file.Inspect()
	report.Version = agent.ReadUserHookStamp(agent.AgentNameClaudeCode)
	if report.State == agent.UserHooksCurrent && report.Version < userHooksVersion {
		report.State = agent.UserHooksOutdated
	}
	return report
}

// settingsHaveEntireHooks reports whether any hook type in a Claude settings
// file runs an Entire command. A hook type whose shape is not Claude's
// matcher list is not Entire's.
func settingsHaveEntireHooks(data []byte) bool {
	var settings struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return true // let the merge or strip report the parse error
	}
	for _, raw := range settings.Hooks {
		var matchers []ClaudeHookMatcher
		if json.Unmarshal(raw, &matchers) == nil && hasEntireHook(matchers) {
			return true
		}
	}
	return false
}

// memSettingsFile runs the shared settings merge on content in memory;
// agent.MergedUserFile does the reading and writing.
type memSettingsFile struct {
	path   string
	data   []byte
	exists bool
	out    []byte
}

func (m *memSettingsFile) Path() string { return m.path }

func (m *memSettingsFile) Read() ([]byte, error) {
	if !m.exists {
		return nil, fs.ErrNotExist
	}
	return m.data, nil
}

func (m *memSettingsFile) Write(data []byte, _ os.FileMode) error {
	m.out = data
	return nil
}
