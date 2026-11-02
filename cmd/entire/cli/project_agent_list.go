package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

func newProjectAgentListCmd() *cobra.Command {
	var presets bool
	cmd := &cobra.Command{
		Use:   cmdList,
		Short: "List the project's agents, or the presets that can be added",
		Long: `List the project's active review agents.

With --presets, list Entire's preset agents instead, and whether the project
already runs each. Add one with ` + "`entire project agent add <preset>`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			t, err := resolveProjectAgentTarget(cmd)
			if err != nil {
				return err
			}
			agents, err := t.listAgents(cmd.Context(), "")
			if err != nil {
				return err
			}
			if presets {
				catalog, err := t.listPresets(cmd.Context())
				if err != nil {
					return err
				}
				return printProjectAgentPresets(cmd, catalog, agents)
			}
			return printProjectAgents(cmd, t, agents)
		},
	}
	cmd.Flags().BoolVar(&presets, "presets", false, "List Entire's preset agents and whether the project runs each")
	addJSONFlag(cmd)
	return cmd
}

var projectAgentColumns = []string{"ID", colHeaderName, "PRESET", "HARNESS", "MODEL", "ENABLED", "REPOS"}

func printProjectAgents(cmd *cobra.Command, t *projectAgentTarget, agents []api.ProjectAgent) error {
	w := cmd.OutOrStdout()
	if jsonRequested(cmd) {
		if agents == nil {
			agents = []api.ProjectAgent{}
		}
		return printJSON(w, agents)
	}
	if len(agents) == 0 {
		fmt.Fprintf(w, "No agents in %s. List the presets with `entire project agent list --presets` and add one with `entire project agent add <preset>`.\n", t.label())
		return nil
	}
	return printTable(w, projectAgentColumns, agents, func(a api.ProjectAgent) []string {
		return []string{a.ID, a.Name, projectAgentPresetLabel(a), a.Harness, derefOr(a.Model, "default"),
			strconv.FormatBool(a.IsEnabled), strconv.Itoa(len(a.RepositoryIDs))}
	})
}

// projectAgentPresetJSON is a preset with whether the project runs it.
type projectAgentPresetJSON struct {
	api.ProjectAgentPreset

	Added bool `json:"added"`
}

func printProjectAgentPresets(cmd *cobra.Command, catalog []api.ProjectAgentPreset, agents []api.ProjectAgent) error {
	added := addedPresetIDs(agents)
	rows := make([]projectAgentPresetJSON, 0, len(catalog))
	for _, p := range catalog {
		rows = append(rows, projectAgentPresetJSON{ProjectAgentPreset: p, Added: added[strings.ToLower(p.ID)]})
	}
	w := cmd.OutOrStdout()
	if jsonRequested(cmd) {
		return printJSON(w, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "No agent presets are available.")
		return nil
	}
	return printTable(w, []string{"PRESET", colHeaderName, "HARNESS", "MODEL", "ADDED", "DESCRIPTION"}, rows, func(p projectAgentPresetJSON) []string {
		return []string{p.ID, p.Name, p.Agent.Harness, derefOr(p.Agent.Model, "default"), strconv.FormatBool(p.Added), tuiutil.SanitizeDisplayText(p.Description)}
	})
}

// addedPresetIDs is the set of presets the project already runs. A project
// runs each preset at most once among its active agents.
func addedPresetIDs(agents []api.ProjectAgent) map[string]bool {
	added := make(map[string]bool, len(agents))
	for _, a := range agents {
		if a.Preset != nil {
			added[strings.ToLower(a.Preset.ID)] = true
		}
	}
	return added
}
