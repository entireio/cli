package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

func newProjectAgentRunRetryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retry <run-id>",
		Short: "Start a failed, timed-out, or canceled run again",
		Long: `Start the next attempt of a failed, timed-out, or canceled run, from the same
agent version and change it ran on. It spends the project's agent budget.
Retrying needs project membership: a repository collaborator outside the
project gets "Project not found".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return projectAgentRunAction(cmd, args, func(ctx context.Context, t *projectAgentTarget, id string, out *json.RawMessage) error {
				if err := t.client.CreateProjectConfig(ctx, t.runPath(id)+"/retry", nil, out); err != nil {
					return fmt.Errorf("retry run %s: %w", id, err)
				}
				return nil
			}, func(w io.Writer, id string, run api.ProjectAgentRun) {
				fmt.Fprintf(w, "Retrying run %s as run %s (attempt %d, %s)\n", tuiutil.SanitizeTerminalLabel(id),
					tuiutil.SanitizeTerminalLabel(run.ID), run.Attempt, tuiutil.SanitizeTerminalLabel(run.Status))
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}
