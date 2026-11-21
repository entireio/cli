package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

func newProjectAgentRunShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <run-id>",
		Short: "Show an agent run: its status, change, and result",
		Long: `Show an agent run: the agent and change it reviewed, its status or why it failed,
and its result. --json prints the whole run, including its findings and events.

--web opens the page of the trail the run belongs to, since a run has no page
of its own yet.`,
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

// openProjectAgentRunInWeb serves `run show --web`. A run has no page of its
// own, so it opens the page of the trail the run belongs to: the run is read
// as usual, then its trail for the server-supplied URL.
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
	var run api.ProjectAgentRun
	if err := t.client.ProjectConfigRequest(cmd.Context(), http.MethodGet, t.runPath(id), nil, nil, &run); err != nil {
		return fmt.Errorf("read run %s: %w", id, err)
	}
	trailURL, err := t.runTrailURL(cmd.Context(), run)
	if err != nil {
		return fmt.Errorf("run %s: %w", id, err)
	}
	if err := openInWeb(cmd, trailURL); err != nil {
		return fmt.Errorf("run %s: its trail has no page to open: %w", id, err)
	}
	return nil
}

// runTrailURL is the web URL of the trail run belongs to, as the server
// supplies it on the trail; empty when the trail carries none. It reads the
// trail from the project's cell, the one the run was read from.
func (t *projectAgentTarget) runTrailURL(ctx context.Context, run api.ProjectAgentRun) (string, error) {
	trailID := run.TrailID
	if run.Target != nil && run.Target.TrailID != "" {
		trailID = run.Target.TrailID
	}
	if trailID == "" {
		return "", errors.New("it belongs to no trail, and a run has no page of its own")
	}
	if !looksLikeULID(trailID) {
		return "", fmt.Errorf("unexpected trail ID %q", tuiutil.SanitizeTerminalLabel(trailID))
	}
	trail := &projectTrailTarget{
		Client:    t.client,
		ProjectID: t.projectID,
		Host:      t.host,
		Project:   t.project,
		BasePath:  projectTrailBasePath(t.host, t.project),
		TrailID:   trailID,
	}
	out, _, err := trail.read(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out.URL), nil
}
