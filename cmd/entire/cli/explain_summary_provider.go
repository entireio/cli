package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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
		blocked := discoverSummaryProviderIfMissing(ctx, providerName)
		if err := ensureSummaryProviderPresent(ctx, providerName); err != nil {
			if blocked {
				return nil, fmt.Errorf("%w\nIf %s is an external plugin, enable external agents first: `entire agent` and pick it, or set \"external_agents\": true in .entire/settings.local.json", err, providerName)
			}
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
// not registered yet. It reports whether it declined to look because external
// agents are not enabled, so the caller can say so rather than leaving the user
// with "unknown summary provider" about a plugin that is installed.
//
// Named, never the sweep. The name arrives from summary_generation.provider,
// which is honored from the COMMITTED .entire/settings.json, so routing it
// through DiscoverAndRegisterAlways would let a pull request turn one settings
// line into "glob every absolute $PATH directory and run every
// entire-agent-* binary's info subcommand" on whoever pulls it. The named
// lookup returns immediately for a built-in and touches exactly one binary
// otherwise, so the ordinary case costs nothing either.
//
// Named is not sufficient on its own, though, which is what the
// external_agents check adds: one binary is still one binary, and the name
// deciding WHICH one still came out of a tracked file. `{"summary_generation":
// {"provider": "evil"}}` in a pull request is then enough to run
// `entire-agent-evil info` on everyone who pulls it and runs `entire explain`,
// with no prompt and no grant — the exact thing enforceExternalAgentsTrust
// exists to prevent, reached by a path that never consults it.
//
// The gate lands only on the external case: the early return above covers every
// registered agent, so a committed `"provider": "claude-code"` keeps working
// with external agents off, as it must.
//
// The lighter gate rather than a third trust gate beside enforceOPFCommandTrust
// and enforceExternalAgentsTrust. An enforceSummaryProviderTrust would let a
// developer name an external provider in their own untracked
// settings.local.json without granting the $PATH sweep, which is a real if
// narrow want; it costs another settings-layer classification, another
// rejection channel for `entire status` to surface, and another gate to keep in
// step. It becomes worth writing when someone actually asks for that
// combination -- until then, `entire agent` already offers the plugin, and
// picking it there is what flips the grant.
//
// The discovery error is dropped for the reason discoverNamedExternalAgent
// gives: the caller reports an unresolvable provider a few lines later, in
// terms of the provider the user named rather than of the plugin protocol.
func discoverSummaryProviderIfMissing(ctx context.Context, name types.AgentName) (blockedByExternalAgents bool) {
	if _, err := getSummaryAgent(name); err == nil {
		return false
	}
	if !settings.IsExternalAgentsEnabled(ctx) {
		return true
	}
	//nolint:errcheck,gosec // see doc comment: ensureSummaryProviderPresent reports it
	discoverNamedSummaryProvider(ctx, name)
	return false
}

// errSelectionNotPersistable reports a selection that is correct for this run
// and must not be written down. See persistSummaryProviderSelection.
var errSelectionNotPersistable = errors.New("selection is valid for this run but would not resolve on the next one")

// autoSelectSummaryProvider builds a provider for an auto-selected candidate
// (single-installed or non-interactive-first-of-many) and persists the choice
// so subsequent runs don't re-decide. Persistence failure is surfaced as a
// warning — not an error — because the selection is still usable in-process.
// An external provider chosen without a human is the one case that persists
// nothing at all; see persistSummaryProviderSelection.
func autoSelectSummaryProvider(ctx context.Context, w io.Writer, name types.AgentName, reason string, origin summarySelectionOrigin) (*checkpointSummaryProvider, error) {
	logging.Info(ctx, reason, "provider", string(name))
	provider, err := buildCheckpointSummaryProvider(name, "")
	if err != nil {
		return nil, err
	}
	flagFlipped, saveErr := persistSummaryProviderSelection(ctx, provider.Name, provider.Model, origin)
	switch {
	case errors.Is(saveErr, errSelectionNotPersistable):
		// Not a warning: nothing went wrong and the run is unaffected. Say what
		// would make the choice stick, since re-selecting it on every run is the
		// only symptom the user would otherwise see.
		logging.Info(ctx, "not persisting auto-selected external summary provider without the external_agents grant",
			"provider", string(provider.Name))
		fmt.Fprintf(w, "Using %s for this run. To save it as the default, run `entire agent` and pick it: an external plugin needs the external_agents grant, and only your own selection can give it.\n", provider.DisplayName)
	case saveErr != nil:
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

	// Only a human's pick grants external_agents: an automatic selection is no
	// one's decision to widen a repo-wide execution grant.
	//
	// Which leaves the case this returns early on. An external provider chosen
	// automatically cannot be persisted either, because the name alone no longer
	// resolves. discoverSummaryProviderIfMissing gates the named lookup on the
	// grant, so writing `provider: X` without it produces a configuration that
	// fails on the very next run: Entire breaking itself with its own write. It
	// did resolve ungated once, which is what the comment that used to sit here
	// said. Gating that lookup closed a hole and invalidated the claim.
	//
	// Writing nothing is not a lost setting. The run in progress already has the
	// agent registered from discoverSummaryProvidersAlways, so it completes, and
	// the next run re-discovers and auto-selects the same provider by the same
	// route. What is lost is only the shortcut of not re-deciding, against a
	// stored value that would make the command fail.
	//
	// This is the rule enableExternalAgentsLocally already follows: do not report
	// success for a write that cannot take effect. Persisting here is that same
	// false claim written to disk instead of printed.
	if ag, getErr := getSummaryAgent(provider); getErr == nil && external.IsExternal(ag) && !s.ExternalAgents {
		if origin != selectionByUser {
			return false, errSelectionNotPersistable
		}
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
