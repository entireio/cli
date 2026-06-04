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
	"os"
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

// Agent registry names used by DetectInvokingAgent. Declared as
// constants so goconst doesn't flag the literals as duplicates of
// strings used elsewhere in the package.
const (
	agentNameClaudeCode = "claude-code"
	agentNameCodex      = "codex"
	agentNameGemini     = "gemini-cli"
	agentNameCopilot    = "copilot-cli"
	agentNamePi         = "pi"
)

// DetectInvokingAgent reads the same env sentinels that
// interactive.CanPromptInteractively() consults to detect when entire
// is running inside an agent CLI. Returns the registry name of the
// caller (e.g. "claude-code", "codex") or "" if none detected.
//
// IMPORTANT: keep this list in sync with the sentinels in the
// interactive package — drift between the two would mean we'd detect
// non-interactive correctly but fail to identify the caller.
func DetectInvokingAgent() string {
	switch {
	case os.Getenv("CLAUDE_CODE") != "":
		return agentNameClaudeCode
	case os.Getenv("CODEX") != "":
		return agentNameCodex
	case os.Getenv("GEMINI_CLI") != "":
		return agentNameGemini
	case os.Getenv("COPILOT_CLI") != "":
		return agentNameCopilot
	case os.Getenv("PI_CODING_AGENT") != "":
		return agentNamePi
	}
	return ""
}

// seedDefaultSkills returns the skill invocation tokens to use for an agent
// that has no saved skills and no explicit per-run skill flags. It prefers
// the agent's curated built-ins; when an agent ships none (e.g. codex, whose
// review skills are discovered on disk rather than bundled in the binary) it
// falls back to on-disk discovered skills whose name signals a review skill
// (contains "review"). Without this, codex would fall back to a generic
// scope-only prompt instead of invoking e.g. $code-reviewer.
//
// Best-effort: returns nil (the run still works — the agent reviews against
// scope) if the agent can't be resolved or nothing matches.
func seedDefaultSkills(ctx context.Context, agentName string) []string {
	if curated := skilldiscovery.CuratedBuiltinsFor(agentName); len(curated) > 0 {
		out := make([]string, 0, len(curated))
		for _, b := range curated {
			out = append(out, b.Name)
		}
		return out
	}
	ag, err := agent.Get(types.AgentName(agentName))
	if err != nil {
		return nil
	}
	d, ok := ag.(agent.SkillDiscoverer)
	if !ok {
		return nil
	}
	discovered, err := d.DiscoverReviewSkills(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, sk := range discovered {
		if strings.Contains(strings.ToLower(sk.Name), "review") {
			out = append(out, sk.Name)
		}
	}
	return out
}

// invokerOnlyReviewConfig builds a default review map from a single
// invoking agent. The agent gets Role Both (reviews AND fixes).
// Returns an error if the agent isn't installed or has no launcher.
func invokerOnlyReviewConfig(ctx context.Context, agentName string, installed []types.AgentName) (map[string]settings.ReviewConfig, error) {
	installedSet := make(map[string]struct{}, len(installed))
	for _, a := range installed {
		installedSet[string(a)] = struct{}{}
	}
	if _, ok := installedSet[agentName]; !ok {
		return nil, fmt.Errorf("invoking agent %q is not installed in this repo", agentName)
	}
	if _, ok := agent.LauncherFor(types.AgentName(agentName)); !ok {
		return nil, fmt.Errorf("invoking agent %q has no launcher (cannot subprocess-spawn)", agentName)
	}
	cfg := settings.ReviewConfig{Role: settings.RoleBoth, Skills: seedDefaultSkills(ctx, agentName)}
	return map[string]settings.ReviewConfig{agentName: cfg}, nil
}

// formatInvokerOnlyNote returns the one-line user-visible explanation
// shown when an unconfigured review falls back to the invoking agent.
func formatInvokerOnlyNote(agentName string) string {
	return fmt.Sprintf(
		"Note: %s as reviewer + fixer (no config). Customize with `entire review setup`, "+
			"or tell your agent who should review/fix.",
		agentName,
	)
}

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
//
//nolint:unparam // map return is consumed by setup tests; production callers ignore it intentionally.
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

	// Pre-seed current roles from saved settings; default to Skip when the
	// agent has no prior entry. Defaulting to Skip makes reviewing opt-in:
	// every agent with Entire hooks installed is listed, but the user must
	// explicitly choose Reviewer/Both for the ones they want — otherwise a
	// machine with several agents enabled silently turns them all into
	// reviewers (the "why is gemini/opencode reviewing?" surprise). The
	// "at least one reviewer" guard below forces an explicit pick. A load
	// error is non-fatal — proceed with Skip seeds and warn.
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
		current[name] = settings.RoleSkip
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

	// Guard against saving a non-functional config (zero reviewers).
	// Without at least one agent in Role Reviewer or Both, `entire review`
	// has nothing to run — refusing here surfaces the problem at setup
	// time instead of after the user tries to invoke review. Checked
	// against the normalized map so that pick-time edge cases (e.g.
	// duplicate-fixer demotion creating a reviewer) are accounted for;
	// only configs that are truly non-functional after normalization fail.
	hasReviewer := false
	for _, cfg := range normalized {
		if cfg.Role.IsReviewer() {
			hasReviewer = true
			break
		}
	}
	if !hasReviewer {
		return nil, errors.New(
			"no agents have role Reviewer or Both; entire review needs at least one " +
				"(re-run `entire review setup` and pick Reviewer or Both for at least one agent)",
		)
	}

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
// agent (seeded from current, defaulting to Skip when unset so reviewing is
// opt-in), passes the pointer map to BuildPickRolesFields, runs the form, then
// copies values back into current.
func realPickRoles(ctx context.Context, agents []string, current map[string]settings.Role) (map[string]settings.Role, error) {
	ptrs := make(map[string]*settings.Role, len(agents))
	for _, name := range agents {
		v := current[name]
		if v == "" {
			v = settings.RoleSkip
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

	// Header identifying which agent these skills are being configured for.
	// The per-agent setup loop reuses this form for each agent in turn, so
	// without it the skill list is ambiguous (e.g. claude vs codex).
	// Note: huh renders Note text as markdown, so backticks get mangled in the
	// terminal — keep these strings backtick-free.
	fields = append(fields, huh.NewNote().
		Title("Review skills — "+displayLabelFor(agentName)).
		Description("Skills this agent runs during entire review."))

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
// uses BuildSetupSkillsFields (Input not Text for instructions) so a
// single-line entry behaves like the rest of the setup wizard.
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
			reviewers = append(reviewers, displayLabelFor(name)+" "+reviewerSkillSuffix(cfg))
		}
		if cfg.Role.IsFixer() {
			fixer = displayLabelFor(name)
		}
	}
	sort.Strings(reviewers)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Review configured.")
	if len(reviewers) > 0 {
		fmt.Fprintln(out, "  Reviewers:")
		for _, r := range reviewers {
			fmt.Fprintf(out, "    %s\n", r)
		}
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

// reviewerSkillSuffix annotates a reviewer with what will drive its review.
// A configured prompt counts as guidance even with zero skills — that's how
// skill-less agents (notably gemini, which has no skill system here) get a
// focused review. Only an agent with neither skills nor a prompt does a truly
// generic, scope-only pass, which is what the last branch flags.
func reviewerSkillSuffix(cfg settings.ReviewConfig) string {
	switch n := len(cfg.Skills); {
	case n == 1:
		return "(1 skill)"
	case n > 1:
		return fmt.Sprintf("(%d skills)", n)
	case strings.TrimSpace(cfg.Prompt) != "":
		return "(custom prompt)"
	default:
		return "(no skills — generic review)"
	}
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
