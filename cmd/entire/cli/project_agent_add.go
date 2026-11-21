package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// projectAgentDefaultEnvironment is the environment add creates for a project
// that has none, so adding a preset needs no separate setup step.
const projectAgentDefaultEnvironment = "Default"

type projectAgentAddOptions struct {
	presets     []string
	environment string // id or name; empty picks the only one, or creates one
	disabled    bool
}

func newProjectAgentAddCmd() *cobra.Command {
	var opts projectAgentAddOptions
	cmd := &cobra.Command{
		Use:   "add [<preset>...]",
		Short: "Add Entire preset agents to the project",
		Long: `Add Entire's preset agents to the project, reviewing this repository.

Preset agents are maintained by Entire, follow the preset as it changes, and
run on free credit — they work before billing is set up. List them with
` + "`entire project agent list --presets`" + `. In a terminal with no presets named,
add asks which to add.

Each agent reviews the current repository and runs on pushes to a branch with
an open Change that is ready for review — not while a GitHub pull request is a
draft; it runs once the pull request is marked ready. A repository without an
approvals gate keeps no readiness, so there every open Change is ready. Pass
--disabled to add it without running on pushes.

A project runs each preset once. When the preset already reviews other
repositories, add extends that agent's selection to this repository, which
needs push access on every repository it reviews. Its enabled state, trigger
and environment stay as they are, so --disabled and --environment are refused
when they ask for different ones.

Agents run in a project environment. With one environment, add uses it; with
none, it creates one named "Default"; with several, name one with
--environment.`,
		Example: "  entire project agent add security-review\n" +
			"  entire project agent add --project gh/acme security-review risk",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			opts.presets = args
			return runProjectAgentAdd(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.environment, "environment", "", "Project environment (id or name) the agents run in")
	cmd.Flags().BoolVar(&opts.disabled, "disabled", false, "Add the agents without running them on pushes")
	addJSONFlag(cmd)
	return cmd
}

func runProjectAgentAdd(cmd *cobra.Command, opts projectAgentAddOptions) error {
	ctx := cmd.Context()
	w, errW := cmd.OutOrStdout(), cmd.ErrOrStderr()

	t, err := resolveProjectAgentTarget(cmd)
	if err != nil {
		return err
	}
	repoID, repoLabel, err := t.currentRepoID(cmd)
	if err != nil {
		return err
	}
	catalog, err := t.listPresets(ctx)
	if err != nil {
		return err
	}
	existing, err := t.listAgents(ctx, "")
	if err != nil {
		return err
	}
	byPreset := presetAgents(existing)

	selected, err := selectProjectAgentPresets(ctx, errW, opts.presets, catalog, byPreset, repoID)
	if err != nil {
		return err
	}

	// An existing agent keeps its enabled state and environment: it is
	// shared with other repositories. A flag asking for different ones cannot
	// be honored, so it is refused before anything is written — for every
	// named preset the project runs, including one selection skipped because
	// it already reviews this repository, so no flag is silently ignored.
	needEnv := opts.environment != ""
	for _, p := range selected {
		if _, ok := byPreset[strings.ToLower(p.ID)]; !ok {
			needEnv = true
		}
	}
	touched := existingPresetAgents(byPreset, opts.presets, selected)
	for _, a := range touched {
		if opts.disabled && a.IsEnabled {
			return fmt.Errorf("--disabled cannot apply to %s: the project already runs it, enabled, and it is shared with the repositories it reviews; drop --disabled, or disable the agent itself", a.Name)
		}
	}
	var envID string
	if needEnv {
		// With --environment this only reads; without it, it may create the
		// default environment, which happens after every refusal above.
		if envID, err = resolveProjectAgentEnvironment(ctx, errW, t, opts.environment); err != nil || envID == "" {
			return err // an empty id without an error is a cancelled picker
		}
	}
	if opts.environment != "" {
		for _, a := range touched {
			if a.EnvironmentID != envID {
				return fmt.Errorf("--environment %s cannot apply to %s: the project already runs it in environment %s; drop --environment", opts.environment, a.Name, a.EnvironmentID)
			}
		}
	}

	saved := []api.ProjectAgent{}
	for _, p := range selected {
		a, err := addOrExtendPresetAgent(cmd, t, p, byPreset, repoID, repoLabel, envID, opts.disabled)
		if err != nil {
			return err
		}
		saved = append(saved, a)
	}

	if jsonRequested(cmd) {
		return printJSON(w, saved)
	}
	if len(saved) == 0 {
		return nil
	}
	fmt.Fprintf(errW, "\n%d agent(s) now review %s in %s.\n", len(saved), repoLabel, t.label())
	return nil
}

// addOrExtendPresetAgent makes the preset review the current repository: a
// new agent when the project does not run the preset, otherwise the existing
// agent with this repository added to its selection. The API allows one
// agent per preset per project, so the second repository of a preset is
// always an extension, never a second agent.
func addOrExtendPresetAgent(cmd *cobra.Command, t *projectAgentTarget, p api.ProjectAgentPreset, byPreset map[string]api.ProjectAgent, repoID, repoLabel, envID string, disabled bool) (api.ProjectAgent, error) {
	ctx, w := cmd.Context(), cmd.OutOrStdout()
	text := !jsonRequested(cmd)

	a, ok := byPreset[strings.ToLower(p.ID)]
	if !ok {
		created, err := t.createAgent(ctx, api.ProjectAgentWrite{
			Preset:        p.ID,
			IsEnabled:     !disabled,
			RepositoryIDs: []string{repoID},
			Triggers:      []api.ProjectAgentTrigger{{Kind: "push"}},
			EnvironmentID: envID,
		})
		if err != nil {
			return api.ProjectAgent{}, fmt.Errorf("add %s: %w", p.ID, err)
		}
		if text {
			state := ""
			if disabled {
				state = ", disabled"
			}
			fmt.Fprintf(w, "added %s (%s)%s\n", created.Name, created.ID, state)
		}
		return created, nil
	}

	// A replace of a preset agent sends the preset and its placement only;
	// its name and instructions are the catalog's. The agent is shared, so
	// its enabled state, trigger and environment stay as they are (the
	// caller refused flags asking otherwise).
	repos := append(liveRepositoryIDs(a), repoID)
	body := api.ProjectAgentWrite{
		Preset:        p.ID,
		IsEnabled:     a.IsEnabled,
		RepositoryIDs: repos,
		Triggers:      a.Triggers,
		EnvironmentID: a.EnvironmentID,
	}
	updated, err := t.replaceAgent(ctx, a.ID, a.ETag, body)
	if err != nil {
		if api.IsHTTPErrorStatus(err, http.StatusForbidden) {
			names := resolveProjectRepoNames(ctx, t.projectID)
			return api.ProjectAgent{}, fmt.Errorf("%s already reviews %s; adding %s to it needs push access on each of those repositories (ask someone who has it to run this command): %w",
				a.Name, repoNames(names, liveRepositoryIDs(a)), repoLabel, err)
		}
		return api.ProjectAgent{}, fmt.Errorf("add %s to %s: %w", repoLabel, a.Name, err)
	}
	if text {
		fmt.Fprintf(w, "extended %s (%s) to review %s\n", updated.Name, updated.ID, repoLabel)
	}
	// The server's version, not the one read before: its etag and version
	// are what the next write needs.
	return updated, nil
}

// existingPresetAgents are the project's agents for the named presets and the
// selected ones, once each: every existing agent the request touches or
// names, whether it is extended or already reviews this repository.
func existingPresetAgents(byPreset map[string]api.ProjectAgent, names []string, selected []api.ProjectAgentPreset) []api.ProjectAgent {
	ids := make([]string, 0, len(names)+len(selected))
	for _, n := range names {
		ids = append(ids, strings.ToLower(strings.TrimSpace(n)))
	}
	for _, p := range selected {
		ids = append(ids, strings.ToLower(p.ID))
	}
	var out []api.ProjectAgent
	seen := map[string]bool{}
	for _, id := range ids {
		if a, ok := byPreset[id]; ok && !seen[a.ID] {
			seen[a.ID] = true
			out = append(out, a)
		}
	}
	return out
}

// presetAgents maps each preset the project runs to its agent. A project runs
// each preset at most once among its active agents.
func presetAgents(agents []api.ProjectAgent) map[string]api.ProjectAgent {
	out := make(map[string]api.ProjectAgent, len(agents))
	for _, a := range agents {
		if a.Preset != nil {
			out[strings.ToLower(a.Preset.ID)] = a
		}
	}
	return out
}

// selectProjectAgentPresets resolves the named presets against the catalog,
// or asks a terminal which to add when none were named. A preset whose agent
// already reviews this repository is reported and skipped; one whose agent
// reviews only other repositories is kept, to be extended.
func selectProjectAgentPresets(ctx context.Context, errW io.Writer, names []string, catalog []api.ProjectAgentPreset, byPreset map[string]api.ProjectAgent, repoID string) ([]api.ProjectAgentPreset, error) {
	covers := func(id string) bool {
		a, ok := byPreset[id]
		return ok && slices.Contains(a.RepositoryIDs, repoID)
	}
	byID := make(map[string]api.ProjectAgentPreset, len(catalog))
	var available []api.ProjectAgentPreset
	for _, p := range catalog {
		id := strings.ToLower(p.ID)
		byID[id] = p
		if !covers(id) {
			available = append(available, p)
		}
	}

	if len(names) == 0 {
		if len(available) == 0 {
			fmt.Fprintln(errW, "Every preset already reviews this repository.")
			return nil, nil
		}
		if !interactive.CanPromptInteractively() {
			return nil, fmt.Errorf("name the presets to add: %s", strings.Join(presetIDs(available), ", "))
		}
		return pickProjectAgentPresets(ctx, errW, available)
	}

	var out []api.ProjectAgentPreset
	seen := map[string]bool{}
	for _, name := range names {
		id := strings.ToLower(strings.TrimSpace(name))
		p, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown preset %q (available: %s)", name, strings.Join(presetIDs(catalog), ", "))
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if covers(id) {
			fmt.Fprintf(errW, "skip %s: it already reviews this repository\n", p.ID)
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func pickProjectAgentPresets(ctx context.Context, errW io.Writer, available []api.ProjectAgentPreset) ([]api.ProjectAgentPreset, error) {
	options := make([]huh.Option[string], 0, len(available))
	for _, p := range available {
		options = append(options, huh.NewOption(p.Name+" — "+tuiutil.SanitizeDisplayText(p.Description), p.ID))
	}
	var chosen []string
	// The note is in the title: accessible mode renders no description.
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Which preset agents should review this repository? Preset agents run on Entire's free credit.").
			Options(options...).
			Value(&chosen),
	))
	if cancelled, err := runProjectAgentPrompt(ctx, errW, "Adding agents", form); err != nil || cancelled {
		return nil, err
	}
	var out []api.ProjectAgentPreset
	for _, p := range available {
		if slices.Contains(chosen, p.ID) {
			out = append(out, p)
		}
	}
	return out, nil
}

func presetIDs(presets []api.ProjectAgentPreset) []string {
	ids := make([]string, 0, len(presets))
	for _, p := range presets {
		ids = append(ids, p.ID)
	}
	return ids
}

// resolveProjectAgentEnvironment picks the environment new agents run in:
// the one named, the project's only one, or a new "Default" when it has none.
// Several and none named is a question for a terminal, or an error naming them.
func resolveProjectAgentEnvironment(ctx context.Context, errW io.Writer, t *projectAgentTarget, want string) (string, error) {
	envs, err := t.listEnvironments(ctx)
	if err != nil {
		return "", err
	}
	if want = strings.TrimSpace(want); want != "" {
		for _, e := range envs {
			if e.ID == want || strings.EqualFold(e.Name, want) {
				return e.ID, nil
			}
		}
		return "", fmt.Errorf("no environment %q in %s (have: %s)", want, t.label(), environmentNames(envs))
	}
	switch len(envs) {
	case 0:
		env, err := t.createEnvironment(ctx, api.ProjectEnvironmentInput{Name: projectAgentDefaultEnvironment, Description: "Created by entire project agent add"})
		if err != nil {
			return "", fmt.Errorf("create environment %q: %w", projectAgentDefaultEnvironment, err)
		}
		fmt.Fprintf(errW, "created environment %s (%s)\n", env.Name, env.ID)
		return env.ID, nil
	case 1:
		return envs[0].ID, nil
	}
	if !interactive.CanPromptInteractively() {
		return "", fmt.Errorf("%s has several environments; pass --environment with one of: %s", t.label(), environmentNames(envs))
	}
	options := make([]huh.Option[string], 0, len(envs))
	for _, e := range envs {
		options = append(options, huh.NewOption(e.Name, e.ID))
	}
	var id string
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Which environment should the agents run in?").Options(options...).Value(&id),
	))
	if cancelled, err := runProjectAgentPrompt(ctx, errW, "Adding agents", form); err != nil || cancelled {
		return "", err
	}
	return id, nil
}

func environmentNames(envs []api.ProjectEnvironment) string {
	if len(envs) == 0 {
		return "(no environments)"
	}
	names := make([]string, 0, len(envs))
	for _, e := range envs {
		names = append(names, e.Name)
	}
	return strings.Join(names, ", ")
}
