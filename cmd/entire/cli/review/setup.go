// Package review — see env.go for package-level rationale.
//
// setup.go implements `entire review setup`: a two-step picker for
// role-first review configuration. Step 1 collects a role per installed
// agent; step 2 collects skills + optional instructions for each agent
// with the Reviewer or Both role.
//
// The legacy in-flow picker on `entire review` is unchanged; Chunk 3
// replaces it with a `Run: entire review setup` pointer.
package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// SetupForms collects the form constructors RunSetup uses. Production
// passes a zero value (uses the real huh forms); tests inject stubs.
type SetupForms struct {
	PickRoles  func(ctx context.Context, agents []string, current map[string]settings.Role) (map[string]settings.Role, error)
	PickSkills func(ctx context.Context, agentName string, prefill settings.ReviewConfig) (settings.ReviewConfig, error)
}

// RunSetup runs the role-first configuration flow. Returns the persisted
// per-agent review map (mirrors the in-memory settings.Review post-save).
//
// Step 1: present a role picker per installed agent (Reviewer/Fixer/Both/Skip),
// seeded from existing settings when available. Step 2: for every agent that
// landed on a Reviewer-side role, present a skills + instructions picker.
// After both steps, NormalizeRoles enforces the at-most-one-fixer invariant.
func RunSetup(
	ctx context.Context,
	out io.Writer,
	getInstalled func(context.Context) []types.AgentName,
	forms SetupForms,
) (map[string]settings.ReviewConfig, error) {
	installed := getInstalled(ctx)
	if len(installed) == 0 {
		return nil, errors.New(
			"no agents with hooks installed; run 'entire configure --agent <name>' " +
				"or 'entire enable' first",
		)
	}

	agentNames := make([]string, 0, len(installed))
	for _, a := range installed {
		agentNames = append(agentNames, string(a))
	}
	sort.Strings(agentNames)

	// Pre-seed current roles from saved settings; default to Reviewer when
	// the agent has no prior entry. A load error is non-fatal here — we
	// proceed with the default seeds and warn so users debugging "my saved
	// roles aren't pre-selected" can find the reason.
	current := make(map[string]settings.Role, len(agentNames))
	saved, loadErr := settings.Load(ctx)
	if loadErr != nil {
		logging.Warn(ctx, "review setup: settings.Load failed, proceeding with empty preselects",
			slog.String("error", loadErr.Error()))
	}
	for _, name := range agentNames {
		if saved != nil {
			if cfg, ok := saved.Review[name]; ok && cfg.Role != "" {
				current[name] = cfg.Role
				continue
			}
		}
		current[name] = settings.RoleReviewer
	}

	pickRoles := forms.PickRoles
	if pickRoles == nil {
		pickRoles = realPickRoles
	}
	chosen, err := pickRoles(ctx, agentNames, current)
	if err != nil {
		return nil, fmt.Errorf("roles picker: %w", err)
	}

	// Convert chosen roles into a ReviewConfig map for NormalizeRoles; carry
	// over saved skills/prompt so users don't re-enter them on every setup
	// run when the role is unchanged.
	configMap := make(map[string]settings.ReviewConfig, len(chosen))
	for name, role := range chosen {
		cfg := settings.ReviewConfig{Role: role}
		if saved != nil {
			if prev, ok := saved.Review[name]; ok {
				cfg.Skills = prev.Skills
				cfg.Prompt = prev.Prompt
			}
		}
		configMap[name] = cfg
	}
	normalized := NormalizeRoles(configMap)

	pickSkills := forms.PickSkills
	if pickSkills == nil {
		pickSkills = realPickSkills
	}

	result := make(map[string]settings.ReviewConfig, len(normalized))
	for _, name := range agentNames {
		cfg := normalized[name]
		if cfg.Role.IsReviewer() {
			prefill := settings.ReviewConfig{}
			if saved != nil {
				prefill = saved.Review[name]
			}
			prefill.Role = cfg.Role
			chosenSkill, err := pickSkills(ctx, name, prefill)
			if err != nil {
				return nil, fmt.Errorf("skills picker for %s: %w", name, err)
			}
			chosenSkill.Role = cfg.Role
			result[name] = chosenSkill
		} else {
			result[name] = settings.ReviewConfig{Role: cfg.Role}
		}
	}

	// Persist only the review map, and only to clone-local preferences
	// (.git/entire/preferences.json). Saving the full merged settings object
	// to the project file would promote clone-local and local-override values
	// (and, on a Load error, an empty EntireSettings with Enabled=false) into
	// the committed .entire/settings.json. SaveReviewConfig writes just the
	// review keys and refuses to clobber a malformed preferences file.
	if err := SaveReviewConfig(ctx, result); err != nil {
		return nil, fmt.Errorf("save review config: %w", err)
	}

	PrintSetupBanner(out, result)
	return result, nil
}

// BuildPickRolesFields constructs one huh.Select[settings.Role] per agent.
// Each Select binds to the pointer in ptrs[agent], so callers can read back
// the user's selection by dereferencing *ptrs[agent] after the form runs.
//
// Ranging over a map yields non-addressable values, so callers must supply
// a pointer-valued map to give huh a stable address per row. realPickRoles
// is the production caller; tests construct their own pointer map.
func BuildPickRolesFields(agents []string, ptrs map[string]*settings.Role) []huh.Field {
	fields := make([]huh.Field, 0, len(agents)+1)
	opts := []huh.Option[settings.Role]{
		huh.NewOption("Reviewer", settings.RoleReviewer),
		huh.NewOption("Fixer", settings.RoleFixer),
		huh.NewOption("Both", settings.RoleBoth),
		huh.NewOption("Skip", settings.RoleSkip),
	}
	for _, name := range agents {
		// Inline(true) collapses each Select to a single-line
		// dropdown row (← / → cycle the value) so the form reads
		// as one row per agent rather than expanding the full
		// option list under each.
		//
		// Validate enforces at-most-one Fixer/Both INLINE so the
		// user sees the conflict immediately. NormalizeRoles still
		// runs after the form as a defensive backstop.
		fields = append(fields,
			huh.NewSelect[settings.Role]().
				Title(displayLabelFor(name)).
				Options(opts...).
				Value(ptrs[name]).
				Inline(true).
				Validate(func(r settings.Role) error {
					if !r.IsFixer() {
						return nil
					}
					for other, p := range ptrs {
						if other == name {
							continue
						}
						if p.IsFixer() {
							return fmt.Errorf("%s already has the %s role; only one agent can be Fixer or Both",
								displayLabelFor(other), *p)
						}
					}
					return nil
				}),
		)
	}
	// Legend Note at the bottom — describes the role choices once,
	// rather than repeating long Option labels per row.
	fields = append(fields, huh.NewNote().
		Description(
			"Reviewer = runs on entire review\n"+
				"Fixer    = runs on entire review fix\n"+
				"Both     = reviews and fixes (counts as the Fixer)\n"+
				"Skip     = exclude from review",
		))
	return fields
}

// realPickRoles is the production role picker. It allocates one pointer per
// agent (seeded from current, defaulting to Reviewer when unset), passes the
// pointer map to BuildPickRolesFields, runs the form, then copies values
// back into current.
func realPickRoles(ctx context.Context, agents []string, current map[string]settings.Role) (map[string]settings.Role, error) {
	ptrs := make(map[string]*settings.Role, len(agents))
	for _, name := range agents {
		v := current[name]
		if v == "" {
			v = settings.RoleReviewer
		}
		ptrs[name] = &v
	}
	fields := BuildPickRolesFields(agents, ptrs)
	form := newAccessibleForm(huh.NewGroup(fields...))
	if err := form.RunWithContext(ctx); err != nil {
		return nil, fmt.Errorf("roles form: %w", err)
	}
	for k, p := range ptrs {
		current[k] = *p
	}
	return current, nil
}

// BuildSetupSkillsFields is a copy-and-adapt of BuildReviewPickerFields
// that swaps the "Additional instructions" huh.NewText for huh.NewInput.
// huh.NewText requires a modifier key (e.g. Ctrl+D) to submit, which is
// ambiguous on macOS/Linux/Windows; huh.NewInput accepts plain Enter so
// users can move forward without consulting docs.
//
// The signature mirrors BuildReviewPickerFields exactly so realPickSkills
// can call it as a drop-in replacement.
func BuildSetupSkillsFields(
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

	// The key difference from BuildReviewPickerFields: Input not Text.
	// huh.NewInput submits on plain Enter; huh.NewText requires Ctrl+D
	// (or a similar modifier) which the user has no in-form way to learn.
	input := huh.NewInput().
		Title("Additional instructions (optional)").
		Description("Added after selected skills. If no skills are selected, this becomes the full review prompt.")
	if promptOut != nil {
		*promptOut = previousPrompt
		input = input.Value(promptOut)
	}
	fields = append(fields, input)

	return fields
}

// realPickSkills is the production skills picker for a single agent. It
// mirrors the per-agent loop inside RunReviewConfigPicker but uses
// BuildSetupSkillsFields (Input not Text for instructions).
func realPickSkills(ctx context.Context, agentName string, prefill settings.ReviewConfig) (settings.ReviewConfig, error) {
	ag, err := agent.Get(types.AgentName(agentName))
	if err != nil {
		return settings.ReviewConfig{}, fmt.Errorf("resolve agent %s: %w", agentName, err)
	}
	curated := skilldiscovery.CuratedBuiltinsFor(agentName)
	var discovered []agent.DiscoveredSkill
	if d, ok := ag.(agent.SkillDiscoverer); ok {
		if ds, dErr := d.DiscoverReviewSkills(ctx); dErr == nil {
			discovered = ds
		} else {
			logging.Debug(ctx, "review setup discovery failed",
				slog.String("agent", agentName), slog.String("error", dErr.Error()))
		}
	}
	builtinNames := builtinNameSet(curated)
	discovered = filterOutBuiltinCollisions(discovered, builtinNames)

	discoveredSet := make(map[string]struct{}, len(discovered))
	for _, d := range discovered {
		discoveredSet[d.Name] = struct{}{}
	}
	activeHints := skilldiscovery.ActiveInstallHintsFor(agentName, discoveredSet)

	builtinPicks, discoveredPicks := SplitSavedPicks(prefill.Skills, curated, discovered)
	prompt := prefill.Prompt

	fields := BuildSetupSkillsFields(
		agentName, curated, discovered, activeHints, prompt,
		&builtinPicks, &discoveredPicks, &prompt,
	)

	form := newAccessibleForm(huh.NewGroup(fields...))
	if err := form.RunWithContext(ctx); err != nil {
		return settings.ReviewConfig{}, fmt.Errorf("skills form: %w", err)
	}
	return settings.ReviewConfig{
		Skills: dedupeStrings(append(builtinPicks, discoveredPicks...)),
		Prompt: strings.TrimSpace(prompt),
	}, nil
}

// PrintSetupBanner prints the post-setup summary banner. Reviewers and
// Fixer are listed by display label (e.g. "Claude Code"), not registry
// name, so users see the same string the picker rendered.
func PrintSetupBanner(out io.Writer, review map[string]settings.ReviewConfig) {
	var reviewers []string
	var fixer string
	for name, cfg := range review {
		if cfg.Role.IsReviewer() {
			reviewers = append(reviewers, displayLabelFor(name))
		}
		if cfg.Role.IsFixer() {
			fixer = displayLabelFor(name)
		}
	}
	sort.Strings(reviewers)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Review configured.")
	if len(reviewers) > 0 {
		fmt.Fprintf(out, "  Reviewers: %s\n", strings.Join(reviewers, ", "))
	} else {
		fmt.Fprintln(out, "  Reviewers: (none)")
	}
	if fixer != "" {
		fmt.Fprintf(out, "  Fixer:     %s\n", fixer)
	} else {
		fmt.Fprintln(out, "  Fixer:     (none)")
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Edit later: entire review setup")
	fmt.Fprintln(out, "Run: entire review")
}

// displayLabelFor resolves an agent's human-readable name (Type) from the
// registry, falling back to the registry key if Get fails. Used by the
// roles picker and the post-setup banner so users see "Claude Code"
// instead of "claude-code".
func displayLabelFor(agentName string) string {
	ag, err := agent.Get(types.AgentName(agentName))
	if err != nil {
		return agentName
	}
	if t := string(ag.Type()); t != "" {
		return t
	}
	return agentName
}

// newReviewSetupCmd returns the `entire review setup` cobra subcommand.
// It's wired in NewCommand alongside the existing attach subcommand.
func newReviewSetupCmd(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Configure reviewers, fixer, and per-agent review skills",
		Long: `Two-step picker: choose a role for each installed agent (Reviewer,
Fixer, Both, or Skip), then choose skills + optional instructions
for each Reviewer/Both agent. Saves to clone-local preferences
(.git/entire/preferences.json) so the config stays private.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// External agents (e.g., cursor, opencode) need to be registered before
			// RunSetup can offer them as Reviewer/Fixer/Both choices. Mirrors the
			// same call in NewCommand's RunE.
			external.DiscoverAndRegister(cmd.Context())
			_, err := RunSetup(cmd.Context(), cmd.OutOrStdout(),
				deps.GetAgentsWithHooksInstalled, SetupForms{})
			// Ctrl+C during a huh form surfaces as context.Canceled
			// (the cobra root cancels ctx on SIGINT). Wrap it as a
			// SilentError so the root doesn't print a noisy
			// "roles picker: ...: context canceled" on a normal abort.
			if errors.Is(err, context.Canceled) {
				return deps.NewSilentError(err)
			}
			return err
		},
	}
}
