package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// repoGatesListJSON is `repo gates list --json`: each gate and each eval
// gate as the server sent it.
type repoGatesListJSON struct {
	Repo         string            `json:"repo"`
	GatesEnabled bool              `json:"gates_enabled"`
	Gates        []json.RawMessage `json:"gates"`
	EvalGates    []json.RawMessage `json:"eval_gates"`
}

func newRepoGatesListCmd(open repoGatesOpener) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show a repository's gates and their modes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := resolveRepoGatesTarget(cmd, open)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			cfg, err := t.client.RepoGateConfig(ctx, t.repoID)
			if err != nil {
				return fmt.Errorf("read gates: %w", err)
			}
			evals := &api.RepoEvalGates{Raw: []json.RawMessage{}}
			// Only a server that supports eval gates can have one, so a
			// repository without any never calls the evals route.
			if hasEvalGate(cfg.Gates) {
				if evals, err = t.client.RepoEvalGates(ctx, t.repoID); err != nil {
					return fmt.Errorf("read eval gates: %w", err)
				}
			}
			if jsonRequested(cmd) {
				gates := cfg.Raw
				if gates == nil {
					gates = []json.RawMessage{}
				}
				return printJSON(cmd.OutOrStdout(), repoGatesListJSON{
					Repo: t.label, GatesEnabled: cfg.GatesEnabled, Gates: gates, EvalGates: evals.Raw,
				})
			}
			return printRepoGates(cmd, t.label, cfg, evals.Items)
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func hasEvalGate(gates []api.RepoGateDefinition) bool {
	for _, g := range gates {
		if g.GateType == api.TrailGateTypeEval {
			return true
		}
	}
	return false
}

func printRepoGates(cmd *cobra.Command, label string, cfg *api.RepoGateConfig, evals []api.RepoEvalGate) error {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Gates for %s\n", label)
	if !cfg.GatesEnabled {
		fmt.Fprintln(w, "Gates are turned off for this repository, so none of them is enforced.")
	}
	fmt.Fprintln(w)
	var gates []api.RepoGateDefinition
	for _, g := range cfg.Gates {
		if g.GateType != api.TrailGateTypeEval {
			gates = append(gates, g)
		}
	}
	if len(gates) == 0 {
		fmt.Fprintln(w, "No gates.")
	} else if err := printTable(w, []string{"GATE", "TYPE", "MODE"}, gates, func(g api.RepoGateDefinition) []string {
		return []string{tuiutil.SanitizeDisplayText(g.GateKey), tuiutil.SanitizeDisplayText(g.GateType), repoGateMode(g.Enabled, g.Blocking)}
	}); err != nil {
		return err
	}
	fmt.Fprintln(w)
	if len(evals) == 0 {
		fmt.Fprintln(w, "No eval gates.")
		return nil
	}
	headers := []string{"AGENT", "MODE", "MUST BE"}
	withNote := false
	for _, g := range evals {
		withNote = withNote || evalGateNote(g) != ""
	}
	if withNote {
		headers = append(headers, "NOTE")
	}
	return printTable(w, headers, evals, func(g api.RepoEvalGate) []string {
		req := evalGateRequirement(g)
		if req == "" {
			req = "-"
		}
		cells := []string{tuiutil.SanitizeDisplayText(evalGateName(g)), repoGateMode(g.Enabled, g.Blocking), tuiutil.SanitizeDisplayText(req)}
		if withNote {
			cells = append(cells, evalGateNote(g))
		}
		return cells
	})
}

// evalGateNote says why an enabled eval gate is not enforced; "" when it is,
// or when the gate is off anyway.
func evalGateNote(g api.RepoEvalGate) string {
	issue := evalIssueText(g.ConfigurationIssue)
	if !g.Enabled || issue == "" {
		return ""
	}
	return "not enforced: " + tuiutil.SanitizeDisplayText(issue)
}
