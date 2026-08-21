package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/spf13/cobra"

	// Import agents to register them
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/codex"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
)

// Package-level aliases to avoid shadowing the settings package with local variables named "settings".
const (
	EntireSettingsFile      = settings.EntireSettingsFile
	EntireSettingsLocalFile = settings.EntireSettingsLocalFile
)

// EntireSettings is an alias for settings.EntireSettings.
type EntireSettings = settings.EntireSettings

// LoadEntireSettings loads the Entire settings from .entire/settings.json,
// then applies any overrides from .entire/settings.local.json if it exists.
// Returns default settings if neither file exists.
// Works correctly from any subdirectory within the repository.
func LoadEntireSettings(ctx context.Context) (*settings.EntireSettings, error) {
	s, err := settings.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}
	return s, nil
}

// SaveEntireSettings saves the Entire settings to .entire/settings.json.
func SaveEntireSettings(ctx context.Context, s *settings.EntireSettings) error {
	if err := settings.Save(ctx, s); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	return nil
}

// SaveEntireSettingsLocal saves the Entire settings to .entire/settings.local.json.
func SaveEntireSettingsLocal(ctx context.Context, s *settings.EntireSettings) error {
	if err := settings.SaveLocal(ctx, s); err != nil {
		return fmt.Errorf("saving local settings: %w", err)
	}
	return nil
}

// IsEnabled returns whether Entire is currently enabled.
// Returns true by default if settings cannot be loaded.
func IsEnabled(ctx context.Context) (bool, error) {
	s, err := settings.Load(ctx)
	if err != nil {
		return true, err //nolint:wrapcheck // already present in codebase
	}
	return s.Enabled, nil
}

// GetStrategy returns the manual-commit strategy instance with blob fetching
// enabled so that checkpoint reads work after treeless fetches.
func GetStrategy(_ context.Context) *strategy.ManualCommitStrategy {
	s := strategy.NewManualCommitStrategy()
	s.SetBlobFetcher(FetchBlobsByHash)
	return s
}

// resolveLogLevel resolves the level for a new logger: the environment first,
// then repo settings. An unreadable settings file leaves the default. An
// unrecognized name warns on stderr rather than logging, because there is no
// logger yet to warn through.
func resolveLogLevel(ctx context.Context) slog.Level {
	name := os.Getenv(logging.LogLevelEnvVar)
	if name == "" {
		if s, err := settings.Load(ctx); err == nil {
			name = s.LogLevel
		}
	}
	level, ok := logging.ParseLevel(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "[entire] Warning: invalid log level %q, defaulting to INFO\n", name)
	}
	return level
}

// ensureLogger attaches a logger to cmd's context unless one is already there,
// and is the only place that installs one. Two loggers on the same file means
// two 8KB buffers, and whichever lands in the context second orphans the first —
// nothing flushes it. Best-effort: a command must not die because a log file
// could not be opened.
func ensureLogger(cmd *cobra.Command) {
	ctx := cmd.Context()
	if logging.LoggerFromContext(ctx) != nil {
		return
	}
	l, err := newLogger(ctx)
	if err != nil {
		return
	}
	cmd.SetContext(logging.WithLogger(ctx, l))
}

// newLogger builds the logger for .entire/logs/entire.log in the current
// worktree. Nothing is created on disk here — the directory and file arrive with
// the first line actually written — so the caller's remaining obligation is
// narrower than it once was but has not gone away: `enable` still calls this
// only after every check that can still reject the invocation, so a rejected
// enable that goes on to log leaves an untouched repo untouched.
func newLogger(ctx context.Context) (*logging.Logger, error) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	l, err := logging.New(logging.Config{
		Dir:   filepath.Join(root, logging.LogsDir),
		Level: resolveLogLevel(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("open log sink: %w", err)
	}
	return l, nil
}

// GetAgentsWithHooksInstalled returns names of agents that have hooks installed.
func GetAgentsWithHooksInstalled(ctx context.Context) []types.AgentName {
	var installed []types.AgentName
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		if hs, ok := agent.AsHookSupport(ag); ok && hs.AreHooksInstalled(ctx) {
			installed = append(installed, name)
		}
	}
	return installed
}

// InstalledAgentDisplayNames returns user-facing display names for agents with hooks installed.
func InstalledAgentDisplayNames(ctx context.Context) []string {
	return agentDisplayNames(GetAgentsWithHooksInstalled(ctx))
}

// OutdatedHookAgents returns installed agents whose Entire hook config has
// drifted from what the CLI would write today, for `entire status` and
// `entire doctor` to surface. Agents that don't implement agent.HookFreshness
// are skipped: absence of a drift check reads as "nothing to report", never as
// a warning.
//
// Scoped to agents AreHooksInstalled reports as installed here. Note what that
// means for generated-file agents (Pi, OpenCode): the committed file *is* the
// installation, so a repo that ships one gets drift warnings even where nobody
// ran `entire agent add`. That is the intent — such a repo is relying on the
// committed file to work — but it does mean this is not scoped to people who
// opted in on this machine.
func OutdatedHookAgents(ctx context.Context) []types.AgentName {
	var outdated []types.AgentName
	for _, name := range GetAgentsWithHooksInstalled(ctx) {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		if hf, ok := agent.AsHookFreshness(ag); ok && hf.CheckHookConfig(ctx) == agent.HooksOutdated {
			outdated = append(outdated, name)
		}
	}
	return outdated
}

// OutdatedHookAgentDisplayNames returns user-facing display names for agents
// whose hook config is out of date.
func OutdatedHookAgentDisplayNames(ctx context.Context) []string {
	return agentDisplayNames(OutdatedHookAgents(ctx))
}

// agentDisplayNames maps agent names to their user-facing display names,
// skipping names that aren't registered.
func agentDisplayNames(names []types.AgentName) []string {
	displayNames := make([]string, 0, len(names))
	for _, name := range names {
		if ag, err := agent.Get(name); err == nil {
			displayNames = append(displayNames, string(ag.Type()))
		}
	}
	return displayNames
}

// JoinAgentNames joins agent names into a comma-separated string.
func JoinAgentNames(names []types.AgentName) string {
	strs := make([]string, len(names))
	for i, n := range names {
		strs[i] = string(n)
	}
	return strings.Join(strs, ",")
}
