package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/orcarouter"
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
	discoverNamedSummaryProvider   = external.DiscoverAndRegisterNamedAlways
	canPromptForSummaryProvider    = interactive.CanPromptInteractively
	promptSummaryProvider          = promptForSummaryProvider
)

// summarySelectionOrigin records who chose a summary provider. It gates the
// external_agents grant, which is repo-wide rather than scoped to the chosen
// provider: it turns on the $PATH sweep that executes every entire-agent-*
// binary from then on. Installing a plugin is consent to "this plugin exists",
// and picking it in the prompt is consent to run plugins generally, but a code
// path that selected the only candidate (or the first of several on a headless
// run) has no such consent behind it.
type summarySelectionOrigin int

const (
	// selectionAutomatic is a provider the resolver picked on its own.
	selectionAutomatic summarySelectionOrigin = iota
	// selectionByUser is a provider a human picked at the prompt.
	selectionByUser
)

type checkpointSummaryProvider struct {
	Name          types.AgentName
	DisplayName   string
	Model         string
	TextGenerator agent.TextGenerator
	Generator     summarize.Generator
	// Streaming reports whether the underlying text generator supports the
	// streaming path (the same predicate TextGeneratorAdapter dispatches on),
	// so the explain layer can attribute timeouts to the streaming diagnostic
	// even when the provider stalls before its first progress event.
	Streaming bool
}

func resolveDispatchSummaryProvider(ctx context.Context, w io.Writer, override string) (*checkpointSummaryProvider, error) {
	override = strings.TrimSpace(override)
	if override == "" {
		return resolveCheckpointSummaryProvider(ctx, w)
	}

	providerName := types.AgentName(override)
	if _, err := getSummaryAgent(providerName); err != nil {
		if err := discoverNamedSummaryProvider(ctx, providerName); err != nil {
			return nil, err
		}
	}
	if err := validateSummaryProvider(override); err != nil {
		return nil, err
	}
	return buildCheckpointSummaryProvider(providerName, "")
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
	// (placing entire-agent-* on $PATH) is the user's opt-in to "this plugin
	// exists", and picking it at the prompt is the opt-in to running plugins
	// generally, which is the only thing that flips external_agents. The
	// non-interactive branches below reach this same list without a prompt, so
	// they select a provider but grant nothing. See summarySelectionOrigin.
	discoverSummaryProvidersAlways(ctx)
	candidates := listEnabledSummaryProviders(ctx)

	switch len(candidates) {
	case 0:
		return nil, errors.New("no summary-capable provider is available; install claude, codex, gemini, pi, cursor, or copilot, install an external entire-agent-* plugin that declares text_generator, or set summary_generation.provider in settings")
	case 1:
		return autoSelectSummaryProvider(ctx, w, candidates[0].Name, "non-interactive auto-select: single installed provider", selectionAutomatic)
	default:
		if !canPromptForSummaryProvider() {
			return autoSelectSummaryProvider(ctx, w, candidates[0].Name, "non-interactive auto-select: first detected of multiple", selectionAutomatic)
		}

		selected, err := promptSummaryProvider(candidates)
		if err != nil {
			return nil, err
		}
		provider, err := autoSelectSummaryProvider(ctx, w, selected, "interactive prompt selection", selectionByUser)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(w, "Using %s for summary generation.\n", provider.DisplayName)
		return provider, nil
	}
}

// discoverSummaryProviderIfMissing resolves a configured provider name that is
// not registered yet.
//
// Named, never the sweep. The name arrives from summary_generation.provider,
// which is honored from the COMMITTED .entire/settings.json, so routing it
// through DiscoverAndRegisterAlways would let a pull request turn one settings
// line into "glob every absolute $PATH directory and run every
// entire-agent-* binary's info subcommand" on whoever pulls it. The named
// lookup returns immediately for a built-in and touches exactly one binary
// otherwise, so the ordinary case costs nothing either.
//
// The error is dropped for the reason discoverNamedExternalAgent gives: the
// caller reports an unresolvable provider a few lines later, in terms of the
// provider the user named rather than of the plugin protocol.
func discoverSummaryProviderIfMissing(ctx context.Context, name types.AgentName) {
	if _, err := getSummaryAgent(name); err == nil {
		return
	}
	//nolint:errcheck,gosec // see doc comment: ensureSummaryProviderPresent reports it
	discoverNamedSummaryProvider(ctx, name)
}

// autoSelectSummaryProvider builds a provider for an auto-selected candidate
// (single-installed or non-interactive-first-of-many) and persists the choice
// so subsequent runs don't re-decide. Persistence failure is surfaced as a
// warning — not an error — because the selection is still usable in-process.
func autoSelectSummaryProvider(ctx context.Context, w io.Writer, name types.AgentName, reason string, origin summarySelectionOrigin) (*checkpointSummaryProvider, error) {
	logging.Info(ctx, reason, "provider", string(name))
	provider, err := buildCheckpointSummaryProvider(name, "")
	if err != nil {
		return nil, err
	}
	flagFlipped, saveErr := persistSummaryProviderSelection(ctx, provider.Name, provider.Model, origin)
	if saveErr != nil {
		logging.Warn(ctx, "failed to save summary provider selection, continuing without persistence",
			"error", saveErr.Error())
		fmt.Fprintf(w, "Warning: could not save provider selection: %v\nUse `entire configure --summarize-provider %s` to set it manually.\n", saveErr, provider.Name)
	}
	// Verified, not assumed: persistSummaryProviderSelection writes the grant
	// into settings.local.json, and a tracked one is dropped wholesale on the
	// next load. Announcing the flip without reading it back is the same false
	// claim enableExternalAgentsLocally now avoids.
	if flagFlipped {
		reportExternalAgentsGrant(w, verifyExternalAgentsGrant(ctx))
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
	// HTTP-based providers have no CLI binary to look up on PATH. OrcaRouter is
	// available when its API key is present in the environment, which is the
	// same prerequisite its text generator authenticates with.
	if name == orcarouter.AgentNameOrcaRouter {
		return orcarouter.APIKey() != ""
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
	return buildCheckpointSummaryProviderWithEffectiveModel(name, summarize.ResolveModel(name, model))
}

func buildCheckpointSummaryProviderWithEffectiveModel(name types.AgentName, effectiveModel string) (*checkpointSummaryProvider, error) {
	ag, err := getSummaryAgent(name)
	if err != nil {
		return nil, fmt.Errorf("loading summary provider %s: %w", name, err)
	}

	textGenerator, ok := agent.AsTextGenerator(ag)
	if !ok {
		return nil, fmt.Errorf("agent %s does not support summary generation", name)
	}

	_, streaming := agent.AsStreamingTextGenerator(textGenerator)

	return &checkpointSummaryProvider{
		Name:          name,
		DisplayName:   string(ag.Type()),
		Model:         effectiveModel,
		TextGenerator: textGenerator,
		Streaming:     streaming,
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
		return fmt.Errorf("summary provider %q CLI binary is not on PATH; install it or choose another provider", provider)
	}
	return nil
}

// persistSummaryProviderSelection writes the chosen provider to
// settings.local.json. When the chosen provider is an external agent and
// external_agents is not yet enabled, it also flips that setting on so the
// plugin can actually run; in that case it returns flagFlipped=true so the
// caller can surface a one-time notice. The flag is written to local because
// the provider choice is already machine-specific (depends on $PATH).
func persistSummaryProviderSelection(ctx context.Context, provider types.AgentName, model string, origin summarySelectionOrigin) (flagFlipped bool, err error) {
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

	// Only a human's pick grants external_agents. The provider name is
	// persisted either way — it decides nothing on its own, and it is what
	// stops the next run re-deciding. An automatically chosen external
	// provider keeps working without the grant, because
	// discoverSummaryProviderIfMissing resolves a configured name through the
	// named ungated lookup rather than through the sweep the grant enables.
	if origin == selectionByUser {
		if ag, getErr := getSummaryAgent(provider); getErr == nil && external.IsExternal(ag) && !s.ExternalAgents {
			s.ExternalAgents = true
			flagFlipped = true
		}
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
