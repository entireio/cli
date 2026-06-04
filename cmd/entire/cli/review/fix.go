package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agentlaunch"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/mdrender"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

type reviewFixSourceKind string

const (
	reviewFixSourceAgent     reviewFixSourceKind = "agent"
	reviewFixSourceAggregate reviewFixSourceKind = "aggregate"
	reviewCommandBinary                          = "entire"
)

type reviewFixSource struct {
	Kind      reviewFixSourceKind
	Agent     string
	Label     string
	Output    string
	Synthetic bool
}

type reviewFinding struct {
	ID    string
	Title string
	Body  string
}

func runReviewFindings(ctx context.Context, cmd *cobra.Command, silentErr func(error) error) error {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
		return wrapReviewSilentError(silentErr, errors.New("not a git repository"))
	}
	manifests, err := loadLocalReviewManifests(ctx, worktreeRoot)
	if err != nil {
		return err
	}
	if len(manifests) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No local review findings found.")
		return nil
	}
	if interactive.IsTerminalWriter(cmd.OutOrStdout()) && interactive.CanPromptInteractively() {
		manifest, pickErr := promptForReviewManifest(ctx, manifests)
		if pickErr != nil {
			return pickErr
		}
		printReviewManifestDetail(cmd.OutOrStdout(), manifest)
		return nil
	}
	printReviewFindingsList(cmd.OutOrStdout(), manifests)
	return nil
}

func runReviewFix(
	ctx context.Context,
	cmd *cobra.Command,
	target string,
	all bool,
	agentOverride string,
	perRunPrompt string,
	silentErr func(error) error,
) error {
	if os.Getenv(EnvSession) != "" {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Already in a review session — refusing to start a nested review.")
		fmt.Fprintln(cmd.ErrOrStderr(), "If you reached this from a reviewer agent's prompt, this is likely a loop and you should exit the inner agent.")
		return wrapReviewSilentError(silentErr, errors.New("nested review session refused"))
	}

	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
		return wrapReviewSilentError(silentErr, errors.New("not a git repository"))
	}

	manifest, err := resolveReviewFixManifest(ctx, cmd, worktreeRoot, target)
	if err != nil {
		return err
	}
	// Interactive multi-source: one navigable form (source step ⇄ findings
	// step) so the user can go back and change the source — e.g. to the
	// aggregate — without restarting. Other cases keep the linear path.
	var sources []reviewFixSource
	var findings []reviewFinding
	if !all && shouldUseInteractiveFixPicker(cmd, manifest) {
		sources, findings, err = selectReviewFixInteractive(ctx, manifest)
	} else {
		if sources, err = selectReviewFixSources(ctx, cmd, manifest, all); err == nil {
			findings, err = selectReviewFindings(ctx, cmd, sources, all)
		}
	}
	if err != nil {
		return err
	}

	fixAgent, err := resolveReviewFixAgent(ctx, cmd, sources, agentOverride)
	if err != nil {
		return err
	}
	prompt := composeReviewFixPrompt(manifest, reviewFixSourcesFromFindings(findings))
	if perRunPrompt != "" {
		prompt += "\n\nAdditional context for this run:\n" + perRunPrompt
	}
	if err := agentlaunch.LaunchFixAgent(ctx, fixAgent, prompt); err != nil {
		return fmt.Errorf("launch review fix agent: %w", err)
	}
	return nil
}

func wrapReviewSilentError(silentErr func(error) error, err error) error {
	if silentErr == nil {
		return err
	}
	return silentErr(err)
}

func resolveReviewFixManifest(ctx context.Context, cmd *cobra.Command, worktreeRoot string, target string) (LocalReviewManifest, error) {
	if target != "" {
		manifest, _, err := resolveLocalReviewManifestBySessionID(ctx, worktreeRoot, target)
		return manifest, err
	}
	manifests, err := loadLocalReviewManifests(ctx, worktreeRoot)
	if err != nil {
		return LocalReviewManifest{}, err
	}
	switch len(manifests) {
	case 0:
		return LocalReviewManifest{}, errors.New("no local review findings found")
	case 1:
		return manifests[0], nil
	default:
		if !interactive.IsTerminalWriter(cmd.OutOrStdout()) || !interactive.CanPromptInteractively() {
			printReviewFindingsList(cmd.OutOrStdout(), manifests)
			return LocalReviewManifest{}, errors.New("multiple review runs found; pass a session id")
		}
		return promptForReviewManifest(ctx, manifests)
	}
}

func promptForReviewManifest(ctx context.Context, manifests []LocalReviewManifest) (LocalReviewManifest, error) {
	options := make([]huh.Option[int], len(manifests))
	for i, manifest := range manifests {
		options[i] = huh.NewOption(reviewManifestListLabel(manifest), i)
	}
	picked := 0
	form := newAccessibleForm(huh.NewGroup(
		huh.NewSelect[int]().
			Title("Select review findings").
			Options(options...).
			Height(min(len(options)+1, 10)).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return LocalReviewManifest{}, fmt.Errorf("review findings picker: %w", err)
	}
	return manifests[picked], nil
}

func selectReviewFixSources(ctx context.Context, cmd *cobra.Command, manifest LocalReviewManifest, all bool) ([]reviewFixSource, error) {
	sources := reviewFixSourcesForManifest(manifest)
	if len(sources) == 0 {
		return nil, errors.New("selected review has no output to fix")
	}
	if all {
		return reviewFixSourcesForAll(sources), nil
	}
	if len(sources) == 1 {
		return sources, nil
	}
	if !interactive.IsTerminalWriter(cmd.OutOrStdout()) || !interactive.CanPromptInteractively() {
		return nil, errors.New("multiple review sources found; rerun with --all or use an interactive terminal")
	}

	values := make([]string, len(sources))
	options := make([]huh.Option[string], len(sources))
	defaults := defaultReviewFixSourceSelection(sources)
	for i, source := range sources {
		value := strconv.Itoa(i)
		values[i] = value
		options[i] = huh.NewOption(source.Label, value)
	}
	picked := defaults
	form := newAccessibleForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title(reviewFixSourcePickerTitle(manifest)).
			Description("ctrl+a select all · space toggle · enter continue").
			Options(options...).
			Height(reviewPickerHeight(len(options))).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return nil, fmt.Errorf("review source picker: %w", err)
	}
	if len(picked) == 0 {
		return nil, errors.New("no review sources selected")
	}
	selected := make([]reviewFixSource, 0, len(picked))
	for _, value := range picked {
		idx := slices.Index(values, value)
		if idx >= 0 {
			selected = append(selected, sources[idx])
		}
	}
	return selected, nil
}

func selectReviewFindings(ctx context.Context, cmd *cobra.Command, sources []reviewFixSource, all bool) ([]reviewFinding, error) {
	findings := extractReviewFindings(sources)
	if all || len(findings) <= 1 {
		return findings, nil
	}
	if !interactive.IsTerminalWriter(cmd.OutOrStdout()) || !interactive.CanPromptInteractively() {
		return nil, errors.New("multiple findings found; rerun with --all or use an interactive terminal")
	}
	options := make([]huh.Option[string], len(findings))
	picked := make([]string, len(findings))
	for i, finding := range findings {
		picked[i] = finding.ID
		options[i] = huh.NewOption(finding.Title, finding.ID)
	}
	form := newAccessibleForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Select findings to fix").
			Description("ctrl+a select all · space toggle · enter fix").
			Options(options...).
			Height(reviewPickerHeight(len(options))).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return nil, fmt.Errorf("review finding picker: %w", err)
	}
	if len(picked) == 0 {
		return nil, errors.New("no findings selected")
	}
	selected := make([]reviewFinding, 0, len(picked))
	for _, finding := range findings {
		if slices.Contains(picked, finding.ID) {
			selected = append(selected, finding)
		}
	}
	return selected, nil
}

// shouldUseInteractiveFixPicker reports whether the unified, navigable
// source+findings picker applies: an interactive terminal with more than one
// fix source (the only case where the back-and-forth between the source and
// findings steps is useful).
func shouldUseInteractiveFixPicker(cmd *cobra.Command, manifest LocalReviewManifest) bool {
	if !interactive.IsTerminalWriter(cmd.OutOrStdout()) || !interactive.CanPromptInteractively() {
		return false
	}
	return len(reviewFixSourcesForManifest(manifest)) > 1
}

// selectReviewFixInteractive presents source selection and findings selection
// as a single huh form with two groups, so the user can move back to the
// source step and change their mind — e.g. switch to the Aggregate source —
// without restarting the command (mirrors the config wizard's navigation).
//
// The findings group's options are recomputed (OptionsFunc) from whichever
// sources are currently selected, bound to the source selection so going back
// and changing it refreshes the findings list. Returns the selected sources
// (for fix-agent resolution) and the selected findings.
func selectReviewFixInteractive(ctx context.Context, manifest LocalReviewManifest) ([]reviewFixSource, []reviewFinding, error) {
	sources := reviewFixSourcesForManifest(manifest)
	sourceValues := make([]string, len(sources))
	sourceOptions := make([]huh.Option[string], len(sources))
	for i, source := range sources {
		value := strconv.Itoa(i)
		sourceValues[i] = value
		sourceOptions[i] = huh.NewOption(source.Label, value)
	}
	pickedSources := defaultReviewFixSourceSelection(sources)
	var pickedFindings []string

	selectedSources := func() []reviewFixSource {
		out := make([]reviewFixSource, 0, len(pickedSources))
		for _, value := range pickedSources {
			if idx := slices.Index(sourceValues, value); idx >= 0 {
				out = append(out, sources[idx])
			}
		}
		return out
	}
	findingsOptions := func() []huh.Option[string] {
		findings := extractReviewFindings(selectedSources())
		opts := make([]huh.Option[string], len(findings))
		for i, finding := range findings {
			opts[i] = huh.NewOption(finding.Title, finding.ID)
		}
		return opts
	}

	form := newAccessibleForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title(reviewFixSourcePickerTitle(manifest)).
				Description("space toggle · ctrl+a all · enter next").
				Options(sourceOptions...).
				Height(reviewPickerHeight(len(sourceOptions))).
				Value(&pickedSources).
				Validate(func(value []string) error {
					if len(value) == 0 {
						return errors.New("select at least one source")
					}
					return nil
				}),
		),
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select findings to fix").
				Description("space toggle · ctrl+a all · shift+tab back to sources · enter fix").
				OptionsFunc(findingsOptions, &pickedSources).
				Height(reviewPickerHeight(len(extractReviewFindings(sources)))).
				Value(&pickedFindings).
				Validate(func(value []string) error {
					if len(value) == 0 {
						return errors.New("select at least one finding (or shift+tab to change sources)")
					}
					return nil
				}),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		return nil, nil, fmt.Errorf("review fix picker: %w", err)
	}

	chosenSources := selectedSources()
	allFindings := extractReviewFindings(chosenSources)
	selected := make([]reviewFinding, 0, len(pickedFindings))
	for _, finding := range allFindings {
		if slices.Contains(pickedFindings, finding.ID) {
			selected = append(selected, finding)
		}
	}
	return chosenSources, selected, nil
}

func composeReviewFixPrompt(manifest LocalReviewManifest, sources []reviewFixSource) string {
	var b strings.Builder
	b.WriteString("Fix only the selected review findings.\n")
	b.WriteString("Do not rewrite unrelated code. Run targeted tests where practical, then report what changed and what verification passed.\n")
	if manifest.StartingSHA != "" {
		fmt.Fprintf(&b, "\nReviewed commit: %s\n", manifest.StartingSHA)
	}
	if manifest.WorktreePath != "" {
		fmt.Fprintf(&b, "Worktree: %s\n", manifest.WorktreePath)
	}
	for _, source := range sources {
		if strings.TrimSpace(source.Output) == "" {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", source.Label, strings.TrimSpace(source.Output))
	}
	return strings.TrimSpace(b.String()) + "\n"
}

func reviewFixSourcesForManifest(manifest LocalReviewManifest) []reviewFixSource {
	sources := make([]reviewFixSource, 0, len(manifest.Sources)+1)
	for _, source := range manifest.Sources {
		if strings.TrimSpace(source.Output) == "" {
			continue
		}
		label := source.Label
		if label == "" {
			label = source.Agent
		}
		sources = append(sources, reviewFixSource{
			Kind:   reviewFixSourceAgent,
			Agent:  source.Agent,
			Label:  label + " findings",
			Output: source.Output,
		})
	}
	if strings.TrimSpace(manifest.AggregateOutput) != "" {
		sources = append(sources, reviewFixSource{
			Kind:   reviewFixSourceAggregate,
			Label:  "Aggregate summary",
			Output: manifest.AggregateOutput,
		})
	} else if len(sources) > 1 {
		sources = append(sources, reviewFixSource{
			Kind:      reviewFixSourceAggregate,
			Label:     "Aggregate findings",
			Output:    selectedSourcesOutput(sources),
			Synthetic: true,
		})
	}
	return sources
}

func reviewPickerHeight(optionCount int) int {
	// huh.MultiSelect subtracts the title and description from Height before
	// sizing the option viewport, so reserve those two lines explicitly.
	return min(optionCount+3, 14)
}

func reviewFixSourcePickerTitle(manifest LocalReviewManifest) string {
	handle := reviewManifestHandle(manifest)
	if handle == "" {
		return "Choose findings source"
	}
	return "Choose findings source (" + handle + ")"
}

func reviewFixSourcesForAll(sources []reviewFixSource) []reviewFixSource {
	selected := make([]reviewFixSource, 0, len(sources))
	for _, source := range sources {
		if source.Synthetic {
			continue
		}
		selected = append(selected, source)
	}
	if len(selected) == 0 {
		return sources
	}
	return selected
}

func defaultReviewFixSourceSelection(sources []reviewFixSource) []string {
	var aggregate []string
	var agents []string
	for i, source := range sources {
		value := strconv.Itoa(i)
		if source.Kind == reviewFixSourceAggregate {
			aggregate = append(aggregate, value)
			continue
		}
		agents = append(agents, value)
	}
	if len(aggregate) > 0 {
		return aggregate
	}
	return agents
}

func extractReviewFindings(sources []reviewFixSource) []reviewFinding {
	var findings []reviewFinding
	for i, source := range sources {
		sourceFindings := extractSourceFindings(source, i)
		findings = append(findings, sourceFindings...)
	}
	if len(findings) > 0 {
		return findings
	}
	combined := selectedSourcesOutput(sources)
	if combined == "" {
		return nil
	}
	return []reviewFinding{{
		ID:    "full-output",
		Title: "Full selected review output",
		Body:  combined,
	}}
}

func extractSourceFindings(source reviewFixSource, sourceIndex int) []reviewFinding {
	lines := strings.Split(source.Output, "\n")
	var findings []reviewFinding
	var current *reviewFinding
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		title, ok := reviewFindingTitle(trimmed)
		if ok {
			if current != nil {
				findings = append(findings, *current)
			}
			current = &reviewFinding{
				ID:    fmt.Sprintf("source-%d-%d", sourceIndex, len(findings)+1),
				Title: source.Label + ": " + stringutil.TruncateRunes(title, 90, "..."),
				Body:  title,
			}
			continue
		}
		if current != nil {
			current.Body = strings.TrimSpace(current.Body + "\n" + line)
		}
	}
	if current != nil {
		findings = append(findings, *current)
	}
	return findings
}

func reviewFindingTitle(line string) (string, bool) {
	line = strings.TrimLeft(line, "#*- \t")
	line = strings.TrimSpace(line)
	if len(line) < 3 {
		return "", false
	}
	if isSeverityNumberedTitle(line) {
		return line, true
	}
	lower := strings.ToLower(line)
	for _, sev := range []string{"blocker", "critical", "high", "medium", "low"} {
		if !strings.HasPrefix(lower, sev) {
			continue
		}
		// Require a non-letter boundary after the severity word so we match the
		// real formats agents emit — "Medium:", "High -", "Critical.", and
		// codex's bolded "**Medium** [file]: …" (which arrives here as
		// "Medium** [file]…" after the leading markdown is trimmed) — without
		// matching ordinary words like "lower" or "highlight".
		if rest := lower[len(sev):]; rest == "" || !isASCIILetter(rest[0]) {
			return line, true
		}
	}
	return "", false
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isSeverityNumberedTitle(line string) bool {
	if len(line) < 3 {
		return false
	}
	switch line[0] {
	case 'H', 'M', 'L':
	default:
		return false
	}
	return line[1] >= '0' && line[1] <= '9' && (line[2] == '.' || line[2] == ')')
}

func reviewFixSourcesFromFindings(findings []reviewFinding) []reviewFixSource {
	var b strings.Builder
	for _, finding := range findings {
		if strings.TrimSpace(finding.Body) == "" {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", finding.Title, strings.TrimSpace(finding.Body))
	}
	return []reviewFixSource{{
		Kind:   reviewFixSourceAgent,
		Label:  "Selected findings",
		Output: strings.TrimSpace(b.String()),
	}}
}

func selectedSourcesOutput(sources []reviewFixSource) string {
	var b strings.Builder
	for _, source := range sources {
		if strings.TrimSpace(source.Output) == "" {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", source.Label, strings.TrimSpace(source.Output))
	}
	return strings.TrimSpace(b.String())
}

func resolveReviewFixAgent(ctx context.Context, _ *cobra.Command, _ []reviewFixSource, agentOverride string) (string, error) {
	if agentOverride != "" {
		return agentOverride, nil
	}
	s, err := settings.Load(ctx)
	if err != nil {
		return "", fmt.Errorf("load review fix settings: %w", err)
	}
	fixer := FixerOf(s)
	if fixer == "" {
		return "", errors.New("no Fixer configured — run: entire review setup")
	}
	return fixer, nil
}

func writeReviewCompletionFooter(w io.Writer, manifest LocalReviewManifest) {
	// Skip only when the manifest is truly empty (review ran but nothing was
	// persisted). A codex-only run has sources with output but no SessionID
	// (codex exec fires no hook to tag a session) — it must still get the
	// footer. `entire review fix` with no handle resolves to the most-recent
	// manifest, so the handle is optional.
	if len(manifest.Sources) == 0 && strings.TrimSpace(manifest.AggregateOutput) == "" {
		return
	}
	handle := reviewManifestHandle(manifest)
	arg := ""
	if handle != "" {
		arg = " " + handle
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Review complete.")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Run: %s review fix%s --all      apply all findings\n", reviewCommandBinary, arg)
	fmt.Fprintf(w, "     %s review fix%s            choose findings interactively\n", reviewCommandBinary, arg)
}

func reviewManifestHandle(manifest LocalReviewManifest) string {
	for _, source := range manifest.Sources {
		if source.SessionID != "" {
			return source.SessionID
		}
	}
	return ""
}

func printReviewFindingsList(w io.Writer, manifests []LocalReviewManifest) {
	fmt.Fprintln(w, "Review Findings")
	fmt.Fprintln(w)
	commandName := reviewCommandBinary
	for _, manifest := range manifests {
		fmt.Fprintf(w, "%s\n", reviewManifestListLabel(manifest))
		fmt.Fprintf(w, "  fix all: %s review fix %s --all\n", commandName, reviewManifestHandle(manifest))
		fmt.Fprintf(w, "  choose:  %s review fix %s\n", commandName, reviewManifestHandle(manifest))
	}
}

func printReviewManifestDetail(w io.Writer, manifest LocalReviewManifest) {
	fmt.Fprintf(w, "Review findings from %s\n\n", reviewManifestListLabel(manifest))
	for _, source := range manifest.Sources {
		printRenderedReviewSection(w, source.Label, source.Output)
	}
	if strings.TrimSpace(manifest.AggregateOutput) != "" {
		printRenderedReviewSection(w, "Aggregate summary", manifest.AggregateOutput)
	}
	writeReviewCompletionFooter(w, manifest)
}

func printRenderedReviewSection(w io.Writer, title string, body string) {
	markdown := fmt.Sprintf("## %s\n\n%s\n", title, strings.TrimSpace(body))
	rendered, err := mdrender.RenderForWriter(w, markdown)
	if err != nil {
		rendered = markdown
	}
	fmt.Fprint(w, rendered)
	if !strings.HasSuffix(rendered, "\n") {
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
}

func reviewManifestListLabel(manifest LocalReviewManifest) string {
	handle := reviewManifestHandle(manifest)
	if handle == "" {
		handle = "unknown-session"
	}
	agents := make([]string, 0, len(manifest.Sources))
	for _, source := range manifest.Sources {
		if source.Label != "" {
			agents = append(agents, source.Label)
			continue
		}
		agents = append(agents, source.Agent)
	}
	preview := reviewManifestPreview(manifest)
	if preview != "" {
		return fmt.Sprintf("%s · local · %s · %s", handle, strings.Join(agents, ", "), preview)
	}
	return fmt.Sprintf("%s · local · %s", handle, strings.Join(agents, ", "))
}

// newReviewFixCmd returns the `entire review fix` cobra subcommand.
//
// It is the canonical entry point for applying review findings; the
// legacy `entire review --fix` flag still works but emits a deprecation
// hint via the parent `review` command's RunE.
func newReviewFixCmd(deps Deps) *cobra.Command {
	var (
		all           bool
		agentOverride string
		perRunPrompt  string
		reviewersFlag []string
		fixerFlag     string
	)
	cmd := &cobra.Command{
		Use:   "fix [session-id]",
		Short: "Apply review findings via the configured Fixer agent",
		Long: `Apply review findings via the configured Fixer agent.

With no positional argument, the most recent review run is selected
(or the user is prompted to pick when multiple exist).

Flags:
  --all              apply all findings/sources without selectors
  --agent NAME       override the configured Fixer for this run
  --prompt TEXT      per-run extra context appended to the fix prompt
  --reviewers LIST   one-off override: agents to use as reviewers
  --fixer NAME       one-off override: agent to use as fixer`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			external.DiscoverAndRegister(ctx)
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			// One-off flag overrides apply by writing to settings in-memory
			// before runReviewFix loads them via settings.Load — we capture
			// the override here and re-resolve fix agent.
			installed := deps.GetAgentsWithHooksInstalled(ctx)
			override, err := resolveRolesFromFlags(reviewersFlag, fixerFlag, installed)
			if err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
				return wrapReviewSilentError(deps.NewSilentError, err)
			}
			if override != nil {
				// --fixer wins for the fix subcommand. Prefer the
				// explicit --fixer name; otherwise use FixerOf on the
				// override.
				fix := strings.TrimSpace(fixerFlag)
				if fix != "" {
					agentOverride = fix
				} else {
					tmp := &settings.EntireSettings{Review: override}
					if f := FixerOf(tmp); f != "" {
						agentOverride = f
					}
				}
			}
			return runReviewFix(ctx, cmd, target, all, agentOverride, perRunPrompt, deps.NewSilentError)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "apply all findings/sources without selectors")
	cmd.Flags().StringVar(&agentOverride, "agent", "", "override the configured Fixer for this run")
	cmd.Flags().StringVar(&perRunPrompt, "prompt", "", "per-run extra context appended to the fix prompt")
	cmd.Flags().StringSliceVar(&reviewersFlag, "reviewers", nil, "one-off override: agents to use as reviewers (comma-separated or repeatable)")
	cmd.Flags().StringVar(&fixerFlag, "fixer", "", "one-off override: agent to use as fixer")
	return cmd
}

func reviewManifestPreview(manifest LocalReviewManifest) string {
	for _, source := range manifest.Sources {
		if text := strings.TrimSpace(source.Output); text != "" {
			return stringutil.TruncateRunes(strings.Join(strings.Fields(text), " "), 70, "...")
		}
	}
	if text := strings.TrimSpace(manifest.AggregateOutput); text != "" {
		return stringutil.TruncateRunes(strings.Join(strings.Fields(text), " "), 70, "...")
	}
	return ""
}
