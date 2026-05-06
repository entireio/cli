package cli

// review_bridge.go wires cli-package implementations into the review
// subpackage's NewCommand Deps struct. Functions that need checkpoint
// access (headHasReviewCheckpoint) and per-agent reviewer constructors
// (launchableReviewerFor) live here to avoid the import cycle:
//   review → checkpoint → codex → review
//   review → claudecode/codex/geminicli → review

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// buildReviewDeps builds the review.Deps struct used by review.NewCommand.
// attachCmd is the cobra.Command for `entire review attach`; pass nil in
// tests that don't need the subcommand.
//
// SynthesisProvider is a lazySynthesisProvider that defers resolution of the
// configured summary provider to the first Synthesize call. This avoids
// running resolveCheckpointSummaryProvider during CLI startup (and during
// every `entire review --help` invocation in tests). Note: side effects are
// DEFERRED, not eliminated — see lazySynthesisProvider doc below.
func buildReviewDeps(attachCmd *cobra.Command) cliReview.Deps {
	return cliReview.Deps{
		GetAgentsWithHooksInstalled: GetAgentsWithHooksInstalled,
		NewSilentError: func(err error) error {
			return NewSilentError(err)
		},
		HeadHasReviewCheckpoint: headHasReviewCheckpoint,
		ReviewCheckpointContext: reviewCheckpointContext,
		ReviewerFor:             launchableReviewerFor,
		PromptForAgentFn:        nil, // use real PromptForAgent
		AttachCmd:               attachCmd,
		SynthesisProvider:       lazySynthesisProvider{},
	}
}

// lazySynthesisProvider wraps the summary-provider resolution so it runs
// only when Synthesize is first called (not at CLI startup).
//
// IMPORTANT: side effects are DEFERRED, not eliminated. resolveCheckpoint-
// SummaryProvider auto-selects a default provider AND persists the choice
// to .entire/settings.local.json (via persistSummaryProviderSelection) on
// the FIRST call against an unconfigured repo. The disk write still
// happens — it's just triggered by the user picking "y" on the synthesis
// prompt, not by every `entire review --help`.
//
// If a future caller needs read-only resolution (e.g. CI mode, where
// touching settings would dirty the working tree), introduce a flag on
// resolveCheckpointSummaryProvider for skip-persistence.
type lazySynthesisProvider struct{}

// Synthesize resolves the configured summary provider on demand and delegates
// the generation call to the underlying TextGenerator. Errors from resolution
// are returned to SynthesisSink, which prints "synthesis unavailable: <err>"
// and lets the user continue without blocking the commit.
//
// resolveCheckpointSummaryProvider's user-facing chatter (auto-select notice,
// "Using <provider>" line, external_agents flag-flip note, persistence-failure
// warning) is captured and routed through logging instead of printing inline
// with the synthesis output. The persistence-failure path is also surfaced as
// logging.Warn at the source (explain_summary_provider.go), so real failures
// are not silenced — they live in .entire/logs/.
//
// Note: first call against an unconfigured repo will write
// .entire/settings.local.json — see the lazySynthesisProvider doc above.
func (lazySynthesisProvider) Synthesize(ctx context.Context, prompt string) (string, error) {
	var captured bytes.Buffer
	provider, err := resolveCheckpointSummaryProvider(ctx, &captured)
	logProviderResolutionOutput(ctx, &captured)
	if err != nil {
		return "", err
	}
	ag, agErr := getSummaryAgent(provider.Name)
	if agErr != nil {
		return "", agErr
	}
	tg, ok := agent.AsTextGenerator(ag)
	if !ok {
		return "", fmt.Errorf("agent %s does not support text generation", provider.Name)
	}
	return tg.GenerateText(ctx, prompt, provider.Model) //nolint:wrapcheck // SynthesisSink owns display
}

// logProviderResolutionOutput routes captured output from resolveCheckpoint-
// SummaryProvider through logging so it ends up in .entire/logs/ rather than
// inline with the synthesis verdict. Lines starting with "Warning:" go to
// Warn; other notices (auto-select reason, "Using X for summary generation",
// external_agents flag-flip note) go to Info.
func logProviderResolutionOutput(ctx context.Context, buf *bytes.Buffer) {
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Warning:") {
			logging.Warn(ctx, "synthesis provider resolution", "message", line)
			continue
		}
		logging.Info(ctx, "synthesis provider resolution", "message", line)
	}
}

// launchableReviewerFor returns the AgentReviewer for known launchable agents,
// or nil for non-launchable agents (cursor, opencode, factoryai-droid,
// copilot-cli). This lives in the cli package to avoid the import cycle:
//
//	review/cmd.go → claudecode/codex/geminicli → review
func launchableReviewerFor(agentName string) reviewtypes.AgentReviewer {
	switch agentName {
	case string(agent.AgentNameClaudeCode):
		return claudecode.NewReviewer()
	case string(agent.AgentNameCodex):
		return codex.NewReviewer()
	case string(agent.AgentNameGemini):
		return geminicli.NewReviewer()
	default:
		return nil
	}
}
