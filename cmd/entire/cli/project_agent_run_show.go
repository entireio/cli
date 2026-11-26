package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

func newProjectAgentRunShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <run-id>",
		Short: "Show an agent run: its status, change, and result",
		Long: `Show an agent run: the agent and change it reviewed, its status or why it failed,
and its result. --json prints the whole run, including its findings and events.

--web opens the run on the project's agent runs page in the Entire web app.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if webRequested(cmd) {
				return openProjectAgentRunInWeb(cmd, args)
			}
			return projectAgentRunAction(cmd, args, func(ctx context.Context, t *projectAgentTarget, id string, out *json.RawMessage) error {
				if err := t.client.ProjectConfigRequest(ctx, http.MethodGet, t.runPath(id), nil, nil, out); err != nil {
					return fmt.Errorf("read run %s: %w", id, err)
				}
				return nil
			}, printProjectAgentRun)
		},
	}
	addJSONFlag(cmd)
	addWebFlag(cmd)
	return cmd
}

// openProjectAgentRunInWeb serves `run show --web`: it reads the run as
// usual, so an unknown run errors before anything opens, then opens the run
// on its project's agent-runs page.
func openProjectAgentRunInWeb(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	id, err := projectAgentRunArg(args)
	if err != nil {
		return err
	}
	t, err := resolveProjectAgentTarget(cmd)
	if err != nil {
		return err
	}
	if err := t.client.ProjectConfigRequest(cmd.Context(), http.MethodGet, t.runPath(id), nil, nil, nil); err != nil {
		return fmt.Errorf("read run %s: %w", id, err)
	}
	base, err := auth.WebBaseURL()
	if err != nil {
		return fmt.Errorf("resolve the Entire web app URL: %w", err)
	}
	return openInWeb(cmd, projectAgentRunWebURL(base, t.host, t.project, id))
}

// projectAgentRunWebURL is a run's page in the web app: the project's agent
// runs page, <web-origin>/<host>/<project>/-/agents/runs, selecting the run.
func projectAgentRunWebURL(base, host, project, runID string) string {
	return strings.TrimRight(base, "/") + "/" + url.PathEscape(host) + "/" + url.PathEscape(project) +
		"/-/agents/runs?" + url.Values{"runId": {runID}}.Encode()
}
