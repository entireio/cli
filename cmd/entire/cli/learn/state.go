package learn

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6"
)

// Stage is the routing decision for the rendered tour.
type Stage string

const (
	// StageNotGitRepo: cwd is not inside a git repository. Bail before discovery.
	StageNotGitRepo Stage = "not-git-repo"
	// StageSetup: in a git repo but Entire has never been enabled here.
	StageSetup Stage = "setup"
	// StageAgentInstall: enabled, but no agent hooks are installed yet.
	StageAgentInstall Stage = "agent-install"
	// StageFirstCapture: enabled, agent installed, but no committed checkpoints exist.
	StageFirstCapture Stage = "first-capture"
	// StageWorkflow: enabled, agent installed, repo has captured history.
	StageWorkflow Stage = "workflow"
)

// State captures everything `entire learn` needs to know about the user's
// repo to choose which tour to render.
type State struct {
	Stage           Stage    `json:"stage"`
	Enabled         bool     `json:"enabled"`
	InstalledAgents []string `json:"installed_agents"`
	HasHistory      bool     `json:"has_history"`
}

// SettingsLoader matches the cli package's LoadEntireSettings signature.
// Injecting it keeps the learn package free of a dependency on the cli
// package (which would create a cycle).
type SettingsLoader func(ctx context.Context) (enabled bool, isSetUp bool, err error)

// AgentInstallChecker matches the cli package's GetAgentsWithHooksInstalled.
// Same rationale: avoids a cli→learn→cli import cycle.
type AgentInstallChecker func(ctx context.Context) []types.AgentName

// ResolveState returns the routing stage and supporting state. It does not
// shell out — every signal comes from in-process Go calls, which is the
// whole point of moving the tour into the CLI.
func ResolveState(ctx context.Context, loadSettings SettingsLoader, listAgents AgentInstallChecker) (State, error) {
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		// Not being in a git repo isn't an error — it's a routing
		// signal for the caller. We return nil here intentionally so
		// translateLearnError can branch on the StageNotGitRepo stage
		// rather than on a propagated paths error.
		return State{Stage: StageNotGitRepo}, nil //nolint:nilerr // intentional: not-a-repo is a stage, not an error
	}

	enabled, isSetUp, err := loadSettings(ctx)
	if err != nil {
		return State{}, fmt.Errorf("load entire settings: %w", err)
	}
	if !isSetUp || !enabled {
		return State{Stage: StageSetup, Enabled: false}, nil
	}

	installed := listAgents(ctx)
	state := State{
		Enabled:         true,
		InstalledAgents: agentNamesAsStrings(installed),
	}
	if len(installed) == 0 {
		state.Stage = StageAgentInstall
		return state, nil
	}

	hasHistory, err := repoHasHistory(ctx)
	if err != nil {
		return State{}, err
	}
	state.HasHistory = hasHistory
	if hasHistory {
		state.Stage = StageWorkflow
	} else {
		state.Stage = StageFirstCapture
	}
	return state, nil
}

func agentNamesAsStrings(names []types.AgentName) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, string(n))
	}
	return out
}

// repoHasHistory returns true when at least one committed checkpoint
// exists anywhere in the repo. We don't restrict to the current branch:
// the skill's "no history on this branch" gate produced false negatives
// for users with prior work on other branches, and dispatch already
// learned that lesson the hard way.
func repoHasHistory(ctx context.Context) (bool, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return false, fmt.Errorf("worktree root: %w", err)
	}
	repo, err := git.PlainOpenWithOptions(repoRoot, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return false, fmt.Errorf("open repo: %w", err)
	}
	infos, err := checkpoint.NewGitStore(repo, checkpoint.ResolveCommittedRefs(ctx)).ListCommitted(ctx)
	if err != nil {
		return false, fmt.Errorf("list committed checkpoints: %w", err)
	}
	return len(infos) > 0, nil
}

// ErrNoTextGenerator is returned by ResolveTextGenerator when no
// TextGenerator-capable agent is available on PATH.
var ErrNoTextGenerator = errors.New("no TextGenerator-capable agent is installed on PATH")

// TextGeneratorChoice is a TextGenerator paired with its display name so
// the caller can tell the user which agent rendered the tour.
type TextGeneratorChoice struct {
	Generator   agent.TextGenerator
	DisplayName string
	Name        types.AgentName
}

// ResolveTextGenerator picks a TextGenerator-capable agent whose CLI is on
// PATH. Honors a configured summary provider when one is set; otherwise
// returns the first registered agent that meets both conditions.
//
// Discovers external entire-agent-* plugins on PATH first so users with
// only an external TextGenerator (no built-in claude/codex/etc.) still
// get a tour. Mirrors what `entire explain --generate` does at
// resolveCheckpointSummaryProvider in the cli package — minus the
// interactive picker, since `entire learn` runs non-interactively and a
// working tour beats a blocking prompt. Users who want to pin a
// specific provider can set it via `entire configure
// --summarize-provider`.
func ResolveTextGenerator(ctx context.Context, configuredProvider string) (TextGeneratorChoice, error) {
	external.DiscoverAndRegisterAlways(ctx)

	if configuredProvider != "" {
		if choice, ok := tryGenerator(types.AgentName(configuredProvider)); ok {
			return choice, nil
		}
	}
	for _, name := range agent.List() {
		if choice, ok := tryGenerator(name); ok {
			return choice, nil
		}
	}
	return TextGeneratorChoice{}, ErrNoTextGenerator
}

func tryGenerator(name types.AgentName) (TextGeneratorChoice, bool) {
	ag, err := agent.Get(name)
	if err != nil {
		return TextGeneratorChoice{}, false
	}
	tg, ok := agent.AsTextGenerator(ag)
	if !ok {
		return TextGeneratorChoice{}, false
	}
	if !isTextGeneratorAvailable(name, ag) {
		return TextGeneratorChoice{}, false
	}
	return TextGeneratorChoice{
		Generator:   tg,
		DisplayName: string(ag.Type()),
		Name:        name,
	}, true
}

// isTextGeneratorAvailable mirrors the cli package's
// isSummaryProviderAvailable: external plugins (entire-agent-*) are
// proven executable by the discovery step and gated only by the
// TextGenerator capability, while built-ins still need their CLI
// binary on PATH (claude, codex, gemini, cursor, copilot).
func isTextGeneratorAvailable(name types.AgentName, ag agent.Agent) bool {
	if external.IsExternal(ag) {
		_, ok := agent.AsTextGenerator(ag)
		return ok
	}
	return agent.IsSummaryCLIAvailable(name)
}
