package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func newRepoGatesAddCmd(open repoGatesOpener) *cobra.Command {
	var agentRef, threshold, mode string
	cmd := &cobra.Command{
		Use:   "add eval --agent <name|id> --threshold <value> --mode attention|block",
		Short: "Add an eval gate on an agent's eval",
		Long: `Add an eval gate: the trail must pass when the agent's eval on its latest
commit is within the threshold.

--threshold depends on the agent's eval: a whole percent from 0 to 100, small,
medium or large for a size, or yes or no (the answer the gate requires) for a
yes/no eval. For a percent or size, the agent's better direction says whether
the reading must be at most or at least the threshold; an agent with no better
direction can't back a gate.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireEvalArg("added", args[0]); err != nil {
				return err
			}
			if strings.TrimSpace(agentRef) == "" || strings.TrimSpace(threshold) == "" || strings.TrimSpace(mode) == "" {
				return errors.New("--agent, --threshold and --mode are required")
			}
			enabled, blocking, err := parseRepoGateMode(mode)
			if err != nil {
				return err
			}
			t, err := resolveRepoGatesTarget(cmd, open)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			agents, err := t.client.RepoAgents(ctx, t.repoID)
			if err != nil {
				return fmt.Errorf("read the repository's agents: %w", err)
			}
			agent, err := findRepoAgent(agents, agentRef)
			if err != nil {
				return err
			}
			value, err := evalGateThreshold(agent.Name, agent.Signal, threshold)
			if err != nil {
				return err
			}
			signal := agent.Signal
			saved, err := editRepoEvalGates(ctx, cmd.ErrOrStderr(), t, func(gates []api.RepoEvalGate) ([]api.RepoEvalGateInput, error) {
				items := make([]api.RepoEvalGateInput, 0, len(gates)+1)
				for _, g := range gates {
					if g.AgentID == agent.ID {
						return nil, fmt.Errorf("%s already has an eval gate; change it with `entire repo gates set eval --agent %s`", agent.Name, agent.ID)
					}
					items = append(items, keepEvalGate(g))
				}
				return append(items, api.RepoEvalGateInput{
					AgentID: agent.ID, Threshold: value, Enabled: enabled, Blocking: blocking, Signal: &signal,
				}), nil
			})
			if err != nil {
				return err
			}
			return printEvalGateResult(cmd, t, saved,
				fmt.Sprintf("Added eval gate on %s: %s", t.label, savedEvalGateSummary(saved, agent.ID, agent.Name)))
		},
	}
	cmd.Flags().StringVar(&agentRef, "agent", "", "Agent whose eval the gate checks, by name or id")
	cmd.Flags().StringVar(&threshold, "threshold", "", "Threshold: a percent (0-100), small/medium/large, or yes/no")
	cmd.Flags().StringVar(&mode, "mode", "", "attention marks a failing trail as needing attention; block stops its merge")
	addJSONFlag(cmd)
	return cmd
}
