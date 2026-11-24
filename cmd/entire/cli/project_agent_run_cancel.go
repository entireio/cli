package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

func newProjectAgentRunCancelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Cancel a queued or running agent run",
		Long: `Cancel a queued or running agent run; a finished run is left as it ended.
Canceling a run someone else or a push started needs permission to manage the
project's agents, and canceling any run needs project membership: a repository
collaborator outside the project gets "Project not found". 'entire project
agent run retry' starts it again.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return projectAgentRunAction(cmd, args, func(ctx context.Context, t *projectAgentTarget, id string, out *json.RawMessage) error {
				if err := t.client.ProjectConfigRequest(ctx, http.MethodPost, t.runPath(id)+"/cancel", nil, nil, out); err != nil {
					return fmt.Errorf("cancel run %s: %w", id, err)
				}
				return nil
			}, func(w io.Writer, id string, run api.ProjectAgentRun) {
				if run.Status == "canceled" {
					fmt.Fprintf(w, "Canceled run %s\n", tuiutil.SanitizeTerminalLabel(id))
					return
				}
				fmt.Fprintf(w, "Run %s had already finished: %s\n", tuiutil.SanitizeTerminalLabel(id), tuiutil.SanitizeTerminalLabel(run.Status))
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}
