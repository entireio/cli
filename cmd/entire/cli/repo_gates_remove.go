package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

func newRepoGatesRemoveCmd(open repoGatesOpener) *cobra.Command {
	var agentRef string
	cmd := &cobra.Command{
		Use:   "remove eval --agent <name|id>",
		Short: "Remove an agent's eval gate",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireEvalArg("removed", args[0]); err != nil {
				return err
			}
			if strings.TrimSpace(agentRef) == "" {
				return errors.New("--agent is required")
			}
			t, err := resolveRepoGatesTarget(cmd, open)
			if err != nil {
				return err
			}
			var name string
			saved, err := editRepoEvalGates(cmd.Context(), cmd.ErrOrStderr(), t, func(gates []api.RepoEvalGate) ([]api.RepoEvalGateInput, error) {
				i, err := findEvalGate(gates, agentRef)
				if err != nil {
					return nil, err
				}
				name = evalGateName(gates[i])
				items := make([]api.RepoEvalGateInput, 0, len(gates)-1)
				for j, g := range gates {
					if j != i {
						items = append(items, keepEvalGate(g))
					}
				}
				return items, nil
			})
			if err != nil {
				return err
			}
			return printEvalGateResult(cmd, t, saved,
				fmt.Sprintf("Removed the eval gate for %s from %s", tuiutil.SanitizeDisplayText(name), t.label))
		},
	}
	cmd.Flags().StringVar(&agentRef, "agent", "", "Agent whose eval gate to remove, by name or id")
	addJSONFlag(cmd)
	return cmd
}
