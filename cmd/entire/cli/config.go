package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
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
//
// Fails when `.entire` is not a real directory. The root pre-run already made
// that check for anything that reaches a command's RunE, so for those callers
// this is a second Lstat — but the pre-run does not cover everything. External
// plugins are dispatched from main.go before cobra runs at all, and commands
// exempt from the pre-run still reach settings through the post-run telemetry
// path. Settings are read FROM the directory in question, which makes this the
// one operation those callers have in common and so the place to guarantee the
// check happens at least once.
//
// Outside a repository there is nothing to validate and the check is skipped,
// so settings still resolve to defaults there.
func LoadEntireSettings(ctx context.Context) (*settings.EntireSettings, error) {
	if err := paths.RequireEntireDir(ctx); err != nil {
		return nil, fmt.Errorf("refusing to load settings: %w", err)
	}
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
// worktree. Nothing is created on disk here — the directory and file arrive
// with the first line actually written. In a globally tracked repo AbsPath
// routes those writes under the git common dir, preserving the invisible
// worktree contract.
func newLogger(ctx context.Context) (*logging.Logger, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve log directory: %w", err)
	}
	// The log sink lives under .entire, so it is a write through that path like
	// any other. The root pre-run has already refused a guarded command by the
	// time we get here; this covers the callers that build a logger outside it
	// (`enable`, the hook handlers) and the exempt commands, which run but must
	// not create .entire/logs through a symlink someone else controls.
	if err := paths.ValidateEntireDirAt(worktreeRoot); err != nil {
		return nil, fmt.Errorf("refusing to open log sink: %w", err)
	}
	l, err := logging.New(logging.Config{
		// The routed base is resolved when the opener is built (see
		// entiredir.Opener): in a globally tracked repo the log sink lives
		// under the git common dir, so logging never creates a worktree
		// .entire directory, and any routing failure means no logger at all.
		Root:  entiredir.Opener(ctx),
		Dir:   logging.LogsName,
		Level: resolveLogLevel(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("open log sink: %w", err)
	}
	return l, nil
}

// agentHookState is the result of one hooks-installed sweep. Detecting is a
// subprocess per external plugin, so a caller needing both answers takes them
// from a single sweep rather than probing twice.
type agentHookState struct {
	// installed are the agents that reported hooks installed.
	installed []types.AgentName
	// unchecked are the agents that could not answer. Their hooks may or may not
	// be on disk, which is not the same as cleanly reporting none.
	unchecked []uncheckedAgent
}

// uncheckedAgent is an agent that could not be asked, with the reason. The
// reason travels with it because the sweep is the only place it exists: asking
// an external plugin again would cost another subprocess.
type uncheckedAgent struct {
	name types.AgentName
	err  error
	// external distinguishes the two remedies offered to the user. Neither kind
	// is uninstalled blind — a check that failed is not a licence to mutate the
	// agent's config — but a plugin owns its own hooks, so its warning can hand
	// out the exact binary invocation that removes them; a built-in's config is
	// ours to read, so the only honest remedy is to say what broke and let a
	// re-run retry it.
	external bool
}

// uncheckedNames returns the unchecked agents' registry names, for display.
func (s agentHookState) uncheckedNames() []types.AgentName {
	names := make([]types.AgentName, 0, len(s.unchecked))
	for _, u := range s.unchecked {
		names = append(names, u.name)
	}
	return names
}

// getAgentHookState probes every hook-supporting agent exactly once, keeping a
// plugin that could not answer distinct from one reporting no hooks.
//
// A binary that fails `info` is never registered, so "unchecked" only ever
// covers a plugin that introduced itself and then failed to answer this.
func getAgentHookState(ctx context.Context) agentHookState {
	var state agentHookState
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		hs, ok := agent.AsHookSupport(ag)
		if !ok {
			continue
		}
		installed, err := hs.AreHooksInstalled(ctx)
		switch {
		case installed:
			state.installed = append(state.installed, name)
		case err == nil:
			// Cleanly reported no hooks.
		case ctx.Err() != nil:
			// The context died, not the agent. Blaming every plugin on $PATH for
			// our own cancellation would turn one Ctrl-C into a page of diagnoses.
			logging.Debug(ctx, "hooks-installed check abandoned: context ended",
				"agent", string(name))
		default:
			// Built-in or plugin: if we have a reason, the caller can say so. A
			// broken .cursor/hooks.json is as worth reporting as a plugin that
			// crashed — what differs is the remedy, not whether to mention it.
			state.unchecked = append(state.unchecked, uncheckedAgent{
				name:     name,
				err:      err,
				external: external.IsExternal(ag),
			})
		}
	}
	return state
}

// GetAgentsWithHooksInstalled returns names of agents that have hooks installed.
// An agent that could not be asked is absent; callers that must act on that
// difference use getAgentHookState.
func GetAgentsWithHooksInstalled(ctx context.Context) []types.AgentName {
	return getAgentHookState(ctx).installed
}

// InstalledAgentDisplayNames returns user-facing display names for agents with hooks installed.
func InstalledAgentDisplayNames(ctx context.Context) []string {
	return agentDisplayNames(GetAgentsWithHooksInstalled(ctx))
}

// OutdatedHookAgents returns agents whose Entire hook config has drifted from
// what the CLI would write today, for `entire status` and `entire doctor` to
// surface. Agents that don't implement agent.HookFreshness are skipped: absence
// of a drift check reads as "nothing to report", never as a warning.
//
// Every freshness implementation is asked directly so it can report a stale
// artifact that no longer qualifies as an active installation. For generated-
// file agents (Pi, OpenCode), the committed file *is* the installation, so a
// repo that ships one gets drift warnings even where nobody ran
// `entire agent add`.
func OutdatedHookAgents(ctx context.Context) []types.AgentName {
	var outdated []types.AgentName
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		if _, ownsDiagnostics := agent.AsEffectiveHookDiagnostics(ag); ownsDiagnostics {
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
		displayNames = append(displayNames, agentDisplayName(name))
	}
	return displayNames
}

// agentDisplayName returns one agent's user-facing name, falling back to the
// registry name when it cannot be looked up. Prose names an agent this way;
// only a command line the user is meant to run keeps the registry name, which
// is what the binary is called.
//
// The fallback matters because these names are how the user learns what a
// command is about to touch, or has left behind. Dropping an unresolvable name
// would list one fewer agent than will be acted on, and an empty one renders as
// a gap in the sentence.
func agentDisplayName(name types.AgentName) string {
	ag, err := agent.Get(name)
	if err != nil {
		return string(name)
	}
	return string(ag.Type())
}

// JoinAgentNames joins agent names into a comma-separated string.
func JoinAgentNames(names []types.AgentName) string {
	strs := make([]string, len(names))
	for i, n := range names {
		strs[i] = string(n)
	}
	return strings.Join(strs, ",")
}
