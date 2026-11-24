package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

func newProjectAgentRunShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <run-id>",
		Short: "Show an agent run: its status, change, and result",
		Long: `Show an agent run: the agent and change it reviewed, its status or why it failed,
and its result. --json prints the whole run, including its findings and events.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return projectAgentRunAction(cmd, args, func(ctx context.Context, t *projectAgentTarget, id string, out *json.RawMessage) error {
				if err := t.client.ProjectConfigRequest(ctx, http.MethodGet, t.runPath(id), nil, nil, out); err != nil {
					return fmt.Errorf("read run %s: %w", id, err)
				}
				return nil
			}, printProjectAgentRun)
		},
	}
	addJSONFlag(cmd)
	return cmd
}
