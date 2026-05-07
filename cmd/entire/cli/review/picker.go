// Package review — see env.go for package-level rationale.
//
// picker.go implements the interactive review skills picker and agent selection
// helpers. pickConfig presents a huh multi-select per installed agent and saves
// the selection to .entire/settings.json.
package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// AgentChoice is one row in the spawn-time picker. Name is the agent
// registry key (used for marker/override); Label is the picker-visible
// string ("<name>   (N skills configured)" or "<name>   (prompt-only)").
type AgentChoice struct {
	Name  string
	Label string
}

// newAccessibleForm creates a huh form with accessibility mode enabled when
// the ACCESSIBLE env var is set. Mirrors cli.NewAccessibleForm without
// requiring an import of the cli package (which would be circular).
func newAccessibleForm(groups ...*huh.Group) *huh.Form {
	form := huh.NewForm(groups...).WithTheme(huh.ThemeFunc(huh.ThemeDracula))
	if os.Getenv("ACCESSIBLE") != "" {
		form = form.WithAccessible(true)
	}
	return form
}

// ConfirmFirstRunSetup prints a banner framing the picker as first-run
// setup (rather than the review itself) and waits for the user to confirm.
// Returns false if the user cancels; caller should bail gracefully.
//
// Signposting matters here because `entire review` with no config silently
// drops into the picker — users running the command to start a review can
// mistake the picker for the review. The banner + confirmation makes the
// setup phase explicit, and the trailing "running review now" line in the
// caller closes the loop on what comes next.
func ConfirmFirstRunSetup(ctx context.Context, out io.Writer) bool {
	fmt.Fprintln(out, "No review config found — let's set one up first.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "You'll pick skills for each installed agent. They're saved to")
	fmt.Fprintln(out, ".entire/settings.json; edit later with `entire review --edit`.")
	fmt.Fprintln(out, "After setup, the review will run with your selection.")
	fmt.Fprintln(out)

	proceed := true
	form := newAccessibleForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Set up review skills now?").
			Affirmative("Yes").
			Negative("Cancel").
			Value(&proceed),
	))
	if err := form.RunWithContext(ctx); err != nil {
		fmt.Fprintln(out, "Setup cancelled.")
		return false
	}
	if !proceed {
		fmt.Fprintln(out, "Setup cancelled.")
	}
	return proceed
}

// RunReviewConfigPicker presents a huh multi-select for each installed agent
// that has curated review skills, and saves the selection to
// .entire/settings.json. Previously-saved skills are pre-checked via
// huh.Option.Selected(true), mirroring how `entire enable` preserves prior
// selections in its own agent picker.
//
// getInstalled is injected to avoid an import cycle with the cli package.
func RunReviewConfigPicker(ctx context.Context, out io.Writer, getInstalled func(context.Context) []types.AgentName) (map[string]settings.ReviewConfig, error) {
	installed := getInstalled(ctx)
	if len(installed) == 0 {
		return nil, errors.New(
			"no agents with hooks installed; " +
				"run 'entire configure --agent <name>' to install hooks for one, " +
				"or 'entire enable' to set up the repo",
		)
	}

	// Narrow to agents that have a curated skills list; others need manual
	// editing of settings.json under review.<agent-name>.
	type configurableAgent struct {
		name types.AgentName
		ag   agent.Agent
	}
	var configurable []configurableAgent
	for _, name := range installed {
		if !skilldiscovery.IsEligible(string(name)) {
			continue
		}
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		configurable = append(configurable, configurableAgent{name: name, ag: ag})
	}
	if len(configurable) == 0 {
		return nil, errors.New(
			"no installed agents have curated review skills; " +
				"edit .entire/settings.json directly under review.<agent-name>",
		)
	}

	// Load existing config so we can pre-check saved skills and seed saved
	// prompts. A load error here means the settings file is malformed; log
	// at Warn so users debugging "my saved skills aren't pre-checked" can
	// see why, but keep going with an empty prefill — runReview already
	// surfaces the same error distinctly when it's the first load.
	existing := map[string]settings.ReviewConfig{}
	if s, err := settings.Load(ctx); err != nil {
		logging.Warn(ctx, "settings.Load failed when pre-filling picker", slog.String("error", err.Error()))
	} else if s != nil {
		existing = s.Review
	}

	// Up-front header: make the order and count obvious so users can spot
	// when an agent they expected isn't being offered (e.g., hooks not
	// installed for it yet).
	labels := make([]string, 0, len(configurable))
	for _, c := range configurable {
		labels = append(labels, string(c.ag.Type()))
	}
	fmt.Fprintf(out, "Configuring review for %d agent(s): %s\n", len(configurable), strings.Join(labels, ", "))
	fmt.Fprintln(out, "(Previously-saved skills are pre-checked. Space to toggle, enter to confirm.)")
	fmt.Fprintln(out)

	selected := map[string]settings.ReviewConfig{}
	for i, c := range configurable {
		curated := skilldiscovery.CuratedBuiltinsFor(string(c.name))

		// Discover + dedupe + filter hints.
		var discovered []agent.DiscoveredSkill
		if d, ok := c.ag.(agent.SkillDiscoverer); ok {
			if ds, dErr := d.DiscoverReviewSkills(ctx); dErr == nil {
				discovered = ds
			} else {
				logging.Debug(ctx, "review discovery failed",
					slog.String("agent", string(c.name)), slog.String("error", dErr.Error()))
			}
		}
		builtinNames := builtinNameSet(curated)
		discovered = filterOutBuiltinCollisions(discovered, builtinNames)

		discoveredSet := make(map[string]struct{}, len(discovered))
		for _, d := range discovered {
			discoveredSet[d.Name] = struct{}{}
		}
		activeHints := skilldiscovery.ActiveInstallHintsFor(string(c.name), discoveredSet)

		// Pre-populate pick slices from saved config so the picker preselects
		// them. The header promises "previously-saved skills are pre-checked";
		// without this split + Option.Selected(true) in BuildReviewPickerFields,
		// --edit with accept-defaults silently wipes the agent's saved skills.
		builtinPicks, discoveredPicks := SplitSavedPicks(
			existing[string(c.name)].Skills, curated, discovered,
		)
		prompt := existing[string(c.name)].Prompt

		fields := BuildReviewPickerFields(
			string(c.name), curated, discovered, activeHints, prompt,
			&builtinPicks, &discoveredPicks, &prompt,
		)

		// Prepend a non-blocking header Note so the agent being configured
		// is always clearly visible.
		header := huh.NewNote().
			Title(string(c.ag.Type())).
			Description(fmt.Sprintf("Agent %d of %d · pick review skills and optional instructions", i+1, len(configurable)))
		fields = append([]huh.Field{header}, fields...)

		form := newAccessibleForm(huh.NewGroup(fields...))
		if err := form.RunWithContext(ctx); err != nil {
			return nil, fmt.Errorf("picker for %s: %w", c.name, err)
		}

		cfg := settings.ReviewConfig{
			Skills: dedupeStrings(append(builtinPicks, discoveredPicks...)),
			Prompt: strings.TrimSpace(prompt),
		}
		if !cfg.IsZero() {
			selected[string(c.name)] = cfg
		}
	}
	// Merge the picker's output with existing entries the picker could not
	// surface. Without the merge, save would replace s.Review wholesale and
	// silently drop entries the user had configured for external agents,
	// uncurated agents, or agents whose hooks are temporarily uninstalled.
	offered := make(map[string]struct{}, len(configurable))
	for _, c := range configurable {
		offered[string(c.name)] = struct{}{}
	}
	merged := MergePickerResults(existing, offered, selected)

	// The emptiness check runs on `merged`, not `selected`.
	if len(merged) == 0 {
		return nil, errors.New("no review skills or prompt configured")
	}

	if err := SaveReviewConfig(ctx, merged); err != nil {
		return nil, err
	}
	fmt.Fprintln(out, "Saved review config to .entire/settings.json. Edit directly or run `entire review --edit`.")
	return merged, nil
}

// MergePickerResults combines the picker's output with existing review
// config entries that the picker did not surface. Agents in `offered` are
// fully controlled by the picker: if they appear in `selected` with a
// non-zero config the entry is set, otherwise the entry is removed.
// Agents not in `offered` keep their existing config untouched.
//
// Exported so tests can drive it directly — the picker itself
// can't run headless.
func MergePickerResults(existing map[string]settings.ReviewConfig, offered map[string]struct{}, selected map[string]settings.ReviewConfig) map[string]settings.ReviewConfig {
	merged := make(map[string]settings.ReviewConfig, len(existing)+len(selected))
	for name, cfg := range existing {
		if _, wasOffered := offered[name]; !wasOffered {
			merged[name] = cfg
		}
	}
	for name, cfg := range selected {
		merged[name] = cfg
	}
	return merged
}

// SaveReviewConfig persists the review map into .entire/settings.json while
// preserving all other settings. A Load error means the file exists but is
// malformed — we must NOT silently overwrite it with an empty struct, or
// every unrelated setting the user had configured would be wiped. Return the
// error so the caller can surface it instead.
func SaveReviewConfig(ctx context.Context, review map[string]settings.ReviewConfig) error {
	s, err := settings.Load(ctx)
	if err != nil {
		return fmt.Errorf("load settings before save: %w", err)
	}
	if s == nil {
		s = &settings.EntireSettings{}
	}
	s.Review = review
	if err := settings.Save(ctx, s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return nil
}

// ComputeEligibleConfigured returns the sorted list of agents that are both
// configured (non-zero ReviewConfig entry) AND have hooks installed. Only
// eligible agents are valid picker targets — spawning a review for an agent
// without hooks would silently drop the review metadata.
func ComputeEligibleConfigured(s *settings.EntireSettings, installed []types.AgentName) []AgentChoice {
	if s == nil {
		return nil
	}
	installedSet := make(map[types.AgentName]struct{}, len(installed))
	for _, name := range installed {
		installedSet[name] = struct{}{}
	}
	out := make([]AgentChoice, 0, len(s.Review))
	for name, cfg := range s.Review {
		if cfg.IsZero() {
			continue
		}
		if _, ok := installedSet[types.AgentName(name)]; !ok {
			continue
		}
		out = append(out, AgentChoice{Name: name, Label: labelForAgentChoice(name, cfg)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// labelForAgentChoice builds the picker-visible label for an agent row.
func labelForAgentChoice(name string, cfg settings.ReviewConfig) string {
	switch {
	case len(cfg.Skills) > 0:
		return fmt.Sprintf("%s   (%d skills configured)", name, len(cfg.Skills))
	case cfg.Prompt != "":
		return name + "   (prompt-only)"
	default:
		return name
	}
}

// computeLaunchableEligible returns the subset of ComputeEligibleConfigured
// that also have a non-nil AgentReviewer (i.e., are launchable by the CLI).
// Used by the dispatch fork in cmd.go to decide whether to route to the
// multi-agent path.
//
// reviewerFor is deps.ReviewerFor injected at the cmd layer; it returns nil
// for non-launchable agents (cursor, opencode, factoryai-droid, copilot-cli).
func computeLaunchableEligible(
	s *settings.EntireSettings,
	installed []types.AgentName,
	reviewerFor func(string) reviewtypes.AgentReviewer,
) []AgentChoice {
	eligible := ComputeEligibleConfigured(s, installed)
	out := make([]AgentChoice, 0, len(eligible))
	for _, c := range eligible {
		if reviewerFor(c.Name) != nil {
			out = append(out, c)
		}
	}
	return out
}

// PromptForAgent renders the single-select agent picker shown when more than
// one eligible agent is configured. Returns the chosen agent name. Respects
// accessibility mode via newAccessibleForm.
func PromptForAgent(ctx context.Context, eligible []AgentChoice) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("agent picker: %w", err)
	}
	if len(eligible) == 0 {
		return "", errors.New("no eligible agents to prompt for")
	}
	options := make([]huh.Option[string], 0, len(eligible))
	for _, c := range eligible {
		options = append(options, huh.NewOption(c.Label, c.Name))
	}
	picked := eligible[0].Name
	form := newAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Which agent should run this review?").
			Options(options...).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return "", fmt.Errorf("agent picker: %w", err)
	}
	return picked, nil
}

// SelectReviewAgent picks an agent from the configured review map.
//
// If override is non-empty, returns the config for that agent or an error
// listing the configured alternatives. Otherwise returns the alphabetically
// first configured agent — deterministic but user-overridable via --agent.
func SelectReviewAgent(review map[string]settings.ReviewConfig, override string) (string, settings.ReviewConfig, error) {
	if len(review) == 0 {
		return "", settings.ReviewConfig{}, errors.New("no review config found")
	}
	var names []string
	for name, cfg := range review {
		if !cfg.IsZero() {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", settings.ReviewConfig{}, errors.New("no review config found")
	}
	sort.Strings(names)
	if override != "" {
		if cfg, ok := review[override]; ok && !cfg.IsZero() {
			return override, cfg, nil
		}
		return "", settings.ReviewConfig{}, fmt.Errorf(
			"agent %q is not configured for review; configured agents: %s",
			override, strings.Join(names, ", "),
		)
	}
	pick := names[0]
	return pick, review[pick], nil
}

// VerifyConfiguredSkillsInstalled is the spawn-time backstop for the
// silent-failure vector. For each skill in cfg.Skills, check it's either a
// curated built-in or returned by the agent's SkillDiscoverer; fail with a
// user-facing error if any skill is missing. Empty Skills (prompt-only
// config) short-circuits to nil — a freeform prompt has no skill list to
// validate against.
func VerifyConfiguredSkillsInstalled(ctx context.Context, ag agent.Agent, cfg settings.ReviewConfig) error {
	if len(cfg.Skills) == 0 {
		return nil
	}
	builtins := builtinNameSet(skilldiscovery.CuratedBuiltinsFor(string(ag.Name())))
	discoveredNames := map[string]struct{}{}
	if d, ok := ag.(agent.SkillDiscoverer); ok {
		if skills, err := d.DiscoverReviewSkills(ctx); err == nil {
			for _, s := range skills {
				discoveredNames[s.Name] = struct{}{}
			}
		} else {
			logging.Debug(ctx, "skill verification discovery failed",
				slog.String("agent", string(ag.Name())), slog.String("error", err.Error()))
		}
	}
	var missing []string
	for _, s := range cfg.Skills {
		if _, ok := builtins[s]; ok {
			continue
		}
		if _, ok := discoveredNames[s]; ok {
			continue
		}
		missing = append(missing, s)
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"configured review skill(s) not installed: %s\n"+
			"run `entire review --edit` to reconfigure, or install the plugin and retry",
		strings.Join(missing, ", "),
	)
}

// BuildReviewPickerFields composes the per-agent group fields for the
// review picker. Returns a slice of huh.Field in render order:
//
//	0: built-in commands (multiselect) OR note
//	1: installed plugin skills (multiselect) OR note
//	2: install hints (note with all active hint messages) — OMITTED if empty
//	3: additional instructions (text) — always present
func BuildReviewPickerFields(
	agentName string,
	builtins []skilldiscovery.CuratedSkill,
	discovered []agent.DiscoveredSkill,
	activeHints []skilldiscovery.InstallHint,
	previousPrompt string,
	builtinPicksOut, discoveredPicksOut *[]string,
	promptOut *string,
) []huh.Field {
	var fields []huh.Field

	if builtinPicksOut != nil && len(*builtinPicksOut) == 0 &&
		len(builtins) == 1 && strings.TrimSpace(previousPrompt) == "" {
		*builtinPicksOut = []string{builtins[0].Name}
	}

	builtinPreselected := preselectedSet(builtinPicksOut)
	discoveredPreselected := preselectedSet(discoveredPicksOut)

	if len(builtins) > 0 {
		opts := make([]huh.Option[string], 0, len(builtins))
		for _, b := range builtins {
			opt := huh.NewOption(b.Name, b.Name)
			if _, ok := builtinPreselected[b.Name]; ok {
				opt = opt.Selected(true)
			}
			opts = append(opts, opt)
		}
		ms := huh.NewMultiSelect[string]().
			Title("Built-in commands").
			Options(opts...).
			Height(len(opts) + 1)
		if builtinPicksOut != nil {
			ms = ms.Value(builtinPicksOut)
		}
		fields = append(fields, ms)
	} else {
		fields = append(fields, huh.NewNote().
			Title("Built-in commands").
			Description(fmt.Sprintf("No built-in review commands in %s.", agentName)))
	}

	if len(discovered) > 0 {
		opts := make([]huh.Option[string], 0, len(discovered))
		for _, d := range discovered {
			opt := huh.NewOption(d.Name, d.Name)
			if _, ok := discoveredPreselected[d.Name]; ok {
				opt = opt.Selected(true)
			}
			opts = append(opts, opt)
		}
		ms := huh.NewMultiSelect[string]().
			Title("Installed plugin skills").
			Options(opts...).
			Height(len(opts) + 1)
		if discoveredPicksOut != nil {
			ms = ms.Value(discoveredPicksOut)
		}
		fields = append(fields, ms)
	} else {
		fields = append(fields, huh.NewNote().
			Title("Installed plugin skills").
			Description("No plugin review skills detected on disk."))
	}

	if len(activeHints) > 0 {
		var sb strings.Builder
		for i, h := range activeHints {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString("• ")
			sb.WriteString(h.Message)
		}
		fields = append(fields, huh.NewNote().
			Title("Install more").
			Description(sb.String()))
	}

	text := huh.NewText().
		Title("Additional instructions (optional)").
		Description("Added after selected skills. If no skills are selected, this becomes the full review prompt.")
	if promptOut != nil {
		*promptOut = previousPrompt
		text = text.Value(promptOut)
	}
	fields = append(fields, text)

	return fields
}

// SplitSavedPicks partitions a flat saved-skills list into the subset that
// matches built-in curated commands and the subset that matches discovered
// plugin skills. Skill names that match neither are dropped from both — they're
// preserved on the settings side via MergePickerResults when they belong to a
// picker-unaware agent entry.
func SplitSavedPicks(saved []string, builtins []skilldiscovery.CuratedSkill, discovered []agent.DiscoveredSkill) ([]string, []string) {
	builtinNames := make(map[string]struct{}, len(builtins))
	for _, b := range builtins {
		builtinNames[b.Name] = struct{}{}
	}
	discoveredNames := make(map[string]struct{}, len(discovered))
	for _, d := range discovered {
		discoveredNames[d.Name] = struct{}{}
	}
	var builtinPicks, discoveredPicks []string
	for _, s := range saved {
		if _, ok := builtinNames[s]; ok {
			builtinPicks = append(builtinPicks, s)
			continue
		}
		if _, ok := discoveredNames[s]; ok {
			discoveredPicks = append(discoveredPicks, s)
		}
	}
	return builtinPicks, discoveredPicks
}

// preselectedSet turns a slice pointer's current contents into a lookup
// set for the picker's "previously-saved" pre-selection.
func preselectedSet(slice *[]string) map[string]struct{} {
	if slice == nil || len(*slice) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(*slice))
	for _, s := range *slice {
		out[s] = struct{}{}
	}
	return out
}

func builtinNameSet(curated []skilldiscovery.CuratedSkill) map[string]struct{} {
	set := make(map[string]struct{}, len(curated))
	for _, c := range curated {
		set[c.Name] = struct{}{}
	}
	return set
}

// filterOutBuiltinCollisions drops any discovered skill whose name collides
// with a curated built-in. Built-in wins because it carries a richer,
// hand-authored description.
func filterOutBuiltinCollisions(discovered []agent.DiscoveredSkill, builtins map[string]struct{}) []agent.DiscoveredSkill {
	if len(discovered) == 0 || len(builtins) == 0 {
		return discovered
	}
	out := make([]agent.DiscoveredSkill, 0, len(discovered))
	for _, d := range discovered {
		if _, clash := builtins[d.Name]; clash {
			continue
		}
		out = append(out, d)
	}
	return out
}

func dedupeStrings(xs []string) []string {
	if len(xs) == 0 {
		return xs
	}
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}
