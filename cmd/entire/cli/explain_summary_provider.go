package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/summarize"

	"charm.land/huh/v2"
)

var (
	loadSummarySettings            = LoadEntireSettings
	loadSummarySettingsFromFile    = settings.LoadFromFile
	saveLocalSummarySettings       = SaveEntireSettingsLocal
	getSummaryAgent                = agent.Get
	listRegisteredAgents           = agent.List
	isSummaryCLIAvailable          = agent.IsSummaryCLIAvailable
	discoverSummaryProviders       = external.DiscoverAndRegister
	discoverSummaryProvidersAlways = external.DiscoverAndRegisterAlways
)

type checkpointSummaryProvider struct {
	Name        types.AgentName
	DisplayName string
	Model       string
	Generator   summarize.Generator
}

func resolveCheckpointSummaryProvider(ctx context.Context, w io.Writer) (*checkpointSummaryProvider, error) {
	s, err := loadSummarySettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}

	if s.SummaryGeneration != nil && s.SummaryGeneration.Provider != "" {
		providerName := types.AgentName(s.SummaryGeneration.Provider)
		discoverSummaryProviderIfMissing(ctx, providerName)
		if err := ensureSummaryProviderPresent(ctx, providerName); err != nil {
			return nil, err
		}
		return buildCheckpointSummaryProvider(providerName, s.SummaryGeneration.Model)
	}

	// Use the always-variant so installed external plugins surface in the
	// picker even when external_agents is currently off. Installation
	// (placing entire-agent-* on $PATH) is the user's opt-in to "this
	// plugin exists"; selecting it in the picker is when external_agents
	// flips on (handled by persistSummaryProviderSelection).
	discoverSummaryProvidersAlways(ctx)
	candidates := listEnabledSummaryProviders(ctx)

	switch len(candidates) {
	case 0:
		return nil, errors.New("no summary-capable provider is available; install claude, codex, gemini, cursor, or copilot, install an external entire-agent-* plugin that declares text_generator, or set summary_generation.provider in settings")
	case 1:
		return autoSelectSummaryProvider(ctx, w, candidates[0].Name, "non-interactive auto-select: single installed provider")
	default:
		if !interactive.CanPromptInteractively() {
			return autoSelectSummaryProvider(ctx, w, candidates[0].Name, "non-interactive auto-select: first detected of multiple")
		}

		selected, err := promptForSummaryProvider(candidates)
		if err != nil {
			return nil, err
		}
		provider, err := autoSelectSummaryProvider(ctx, w, selected, "interactive prompt selection")
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(w, "Using %s for summary generation.\n", provider.DisplayName)
		return provider, nil
	}
}

func discoverSummaryProviderIfMissing(ctx context.Context, name types.AgentName) {
	if _, err := getSummaryAgent(name); err == nil {
		return
	}
	discoverSummaryProviders(ctx)
}

// autoSelectSummaryProvider builds a provider for an auto-selected candidate
// (single-installed or non-interactive-first-of-many) and persists the choice
// so subsequent runs don't re-decide. Persistence failure is surfaced as a
// warning — not an error — because the selection is still usable in-process.
func autoSelectSummaryProvider(ctx context.Context, w io.Writer, name types.AgentName, reason string) (*checkpointSummaryProvider, error) {
	logging.Info(ctx, reason, "provider", string(name))
	provider, err := buildCheckpointSummaryProvider(name, "")
	if err != nil {
		return nil, err
	}
	flagFlipped, saveErr := persistSummaryProviderSelection(ctx, provider.Name, provider.Model)
	if saveErr != nil {
		logging.Warn(ctx, "failed to save summary provider selection, continuing without persistence",
			"error", saveErr.Error())
		fmt.Fprintf(w, "Warning: could not save provider selection: %v\nUse `entire configure --summarize-provider %s` to set it manually.\n", saveErr, provider.Name)
	}
	if flagFlipped {
		fmt.Fprintln(w, externalAgentsAutoEnabledNotice)
	}
	return provider, nil
}

func listEnabledSummaryProviders(_ context.Context) []checkpointSummaryProvider {
	registered := listRegisteredAgents()
	providers := make([]checkpointSummaryProvider, 0, len(registered))
	for _, name := range registered {
		ag, err := getSummaryAgent(name)
		if err != nil {
			continue
		}
		if _, ok := agent.AsTextGenerator(ag); !ok {
			continue
		}
		// Check CLI binary on PATH for built-ins. External agents are already
		// proven executable by discovery and are gated by text_generator.
		if !isSummaryProviderAvailable(name, ag) {
			continue
		}
		providers = append(providers, checkpointSummaryProvider{
			Name:        name,
			DisplayName: string(ag.Type()),
		})
	}
	return providers
}

func isSummaryProviderAvailable(name types.AgentName, ag agent.Agent) bool {
	if external.IsExternal(ag) {
		_, ok := agent.AsTextGenerator(ag)
		return ok
	}
	return isSummaryCLIAvailable(name)
}

func promptForSummaryProvider(providers []checkpointSummaryProvider) (types.AgentName, error) {
	options := make([]huh.Option[string], 0, len(providers))
	for _, provider := range providers {
		options = append(options, huh.NewOption(provider.DisplayName, string(provider.Name)))
	}

	var selected string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Choose a summary provider").
				Description("This choice will be saved. Use `entire configure --summarize-provider <name>` to change it later.").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		return "", fmt.Errorf("summary provider selection cancelled: %w", err)
	}

	return types.AgentName(selected), nil
}

func buildCheckpointSummaryProvider(name types.AgentName, model string) (*checkpointSummaryProvider, error) {
	ag, err := getSummaryAgent(name)
	if err != nil {
		return nil, fmt.Errorf("loading summary provider %s: %w", name, err)
	}

	textGenerator, ok := agent.AsTextGenerator(ag)
	if !ok {
		return nil, fmt.Errorf("agent %s does not support summary generation", name)
	}

	effectiveModel := summarize.ResolveModel(name, model)

	return &checkpointSummaryProvider{
		Name:        name,
		DisplayName: string(ag.Type()),
		Model:       effectiveModel,
		Generator: &summarize.TextGeneratorAdapter{
			TextGenerator: textGenerator,
			Model:         effectiveModel,
		},
	}, nil
}

// ensureSummaryProviderPresent returns an error if the named summary provider's
// CLI binary is not on PATH. Checks the binary directly (via exec.LookPath)
// rather than DetectPresence, because DetectPresence checks repo-level agent
// configuration — a repo using Claude Code for development can still use Codex
// or Gemini for summary generation as long as the binary is installed.
func ensureSummaryProviderPresent(_ context.Context, name types.AgentName) error {
	ag, err := getSummaryAgent(name)
	if err != nil {
		return fmt.Errorf("unknown summary provider %s: %w", name, err)
	}
	if _, ok := agent.AsTextGenerator(ag); !ok {
		return fmt.Errorf("agent %s does not support summary generation", name)
	}
	if !isSummaryProviderAvailable(name, ag) {
		return fmt.Errorf("summary provider %q is configured but its CLI binary is not on PATH; install it or update summary_generation.provider in settings", name)
	}
	return nil
}

func validateSummaryProvider(provider string) error {
	name := types.AgentName(provider)
	ag, err := getSummaryAgent(name)
	if err != nil {
		return fmt.Errorf("unknown summary provider %q: %w", provider, err)
	}
	if _, ok := agent.AsTextGenerator(ag); !ok {
		return fmt.Errorf("agent %q does not support summary generation", provider)
	}
	if !isSummaryProviderAvailable(name, ag) {
		return fmt.Errorf("summary provider %q is configured but its CLI binary is not on PATH; install it or choose another provider", provider)
	}
	return nil
}

// persistSummaryProviderSelection writes the chosen provider to
// settings.local.json. When the chosen provider is an external agent and
// external_agents is not yet enabled, it also flips that setting on so the
// plugin can actually run; in that case it returns flagFlipped=true so the
// caller can surface a one-time notice. The flag is written to local because
// the provider choice is already machine-specific (depends on $PATH).
func persistSummaryProviderSelection(ctx context.Context, provider types.AgentName, model string) (flagFlipped bool, err error) {
	targetFileAbs, err := paths.AbsPath(ctx, settings.EntireSettingsLocalFile)
	if err != nil {
		targetFileAbs = settings.EntireSettingsLocalFile
	}

	s, err := loadSummarySettingsFromFile(targetFileAbs)
	if err != nil {
		return false, fmt.Errorf("loading settings for update: %w", err)
	}
	if s.SummaryGeneration == nil {
		s.SummaryGeneration = &settings.SummaryGenerationSettings{}
	}
	s.SummaryGeneration.SetProvider(string(provider), model)

	if ag, getErr := getSummaryAgent(provider); getErr == nil && external.IsExternal(ag) && !s.ExternalAgents {
		s.ExternalAgents = true
		flagFlipped = true
	}

	if err := saveLocalSummarySettings(ctx, s); err != nil {
		return false, fmt.Errorf("saving summary provider selection: %w", err)
	}
	return flagFlipped, nil
}

// summaryProviderRows builds the structured rows used by the summary-generation
// success block. Returns nil for a nil provider so callers can append optional
// provider info to a row slice without nil-checking themselves.
func summaryProviderRows(provider *checkpointSummaryProvider) []explainRow {
	if provider == nil {
		return nil
	}
	model := provider.Model
	if model == "" {
		model = "provider default"
	}
	return []explainRow{
		{Label: "provider", Value: provider.DisplayName},
		{Label: "model", Value: model},
	}
}
