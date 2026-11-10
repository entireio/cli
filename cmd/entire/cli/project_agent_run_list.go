package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// projectAgentRunStatuses are the statuses GET .../runs filters by.
var projectAgentRunStatuses = strings.Fields("queued running succeeded failed timed_out canceled")

// projectAgentRunDefaultLimit is run list's page size without --limit.
const projectAgentRunDefaultLimit = 20

func newProjectAgentRunListCmd() *cobra.Command {
	var agentID, status, cursor string
	var limit int
	cmd := &cobra.Command{
		Use:   cmdList,
		Short: "List the project's agent runs, newest first",
		Long: `List the project's agent runs, newest first, in repositories you can read.
One page per call: the next page's command is printed, with --cursor and the
same --project, --agent, --status, and --limit, which a cursor needs repeated.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			q := url.Values{}
			if agentID = strings.TrimSpace(agentID); agentID != "" {
				if !looksLikeULID(agentID) {
					return fmt.Errorf("--agent must be an agent ID (see 'entire project agent list'), got %q", tuiutil.SanitizeTerminalLabel(agentID))
				}
				q.Set("agent_id", agentID)
			}
			if status = strings.TrimSpace(status); status != "" {
				if !slices.Contains(projectAgentRunStatuses, status) {
					return fmt.Errorf("--status must be one of %s", strings.Join(projectAgentRunStatuses, ", "))
				}
				q.Set("status", status)
			}
			if limit < 1 || limit > 100 {
				return errors.New("--limit must be between 1 and 100")
			}
			// A cursor is bound to the filters of the listing it came from, which
			// must be sent with it again; per_page is read on every page.
			project, _ := cmd.Flags().GetString("project") //nolint:errcheck // an inherited flag of the agent group
			next := projectAgentRunNextFlags(strings.TrimSpace(project), agentID, status, limit)
			if cursor = strings.TrimSpace(cursor); cursor != "" {
				q.Set("cursor", cursor)
			}
			q.Set("per_page", strconv.Itoa(limit))
			t, err := resolveProjectAgentTarget(cmd)
			if err != nil {
				return err
			}
			var page api.ProjectAgentRunPage
			if err := t.client.ProjectConfigRequest(cmd.Context(), http.MethodGet, t.runsPath()+"?"+q.Encode(), nil, nil, &page); err != nil {
				return fmt.Errorf("list runs of %s: %w", t.label(), err)
			}
			if page.Items == nil {
				page.Items = []json.RawMessage{}
			}
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), page)
			}
			return printProjectAgentRuns(cmd.OutOrStdout(), t, page, next)
		},
	}
	cmd.Flags().StringVar(&agentID, "agent", "", "Only this agent's runs (its ID from 'entire project agent list')")
	cmd.Flags().StringVar(&status, "status", "", "Only runs in this status: "+strings.Join(projectAgentRunStatuses, ", "))
	cmd.Flags().IntVar(&limit, "limit", projectAgentRunDefaultLimit, "Runs per page (1-100)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue a listing from its printed cursor")
	addJSONFlag(cmd)
	return cmd
}

// projectAgentRunNextFlags are the flags a next-page command repeats: the
// cursor is bound to the project and the filters, and the page size is not
// remembered.
func projectAgentRunNextFlags(project, agentID, status string, limit int) string {
	var out string
	if project != "" {
		out += " --project " + strategy.ShellQuoteForDisplay(tuiutil.SanitizeTerminalLabel(project))
	}
	if agentID != "" {
		out += " --agent " + agentID
	}
	if status != "" {
		out += " --status " + status
	}
	if limit != projectAgentRunDefaultLimit {
		out += " --limit " + strconv.Itoa(limit)
	}
	return out
}

// printProjectAgentRuns prints one page; next is the flags its next page
// repeats with the cursor.
func printProjectAgentRuns(w io.Writer, t *projectAgentTarget, page api.ProjectAgentRunPage, next string) error {
	runs := make([]api.ProjectAgentRun, len(page.Items))
	for i, raw := range page.Items {
		if err := json.Unmarshal(raw, &runs[i]); err != nil {
			return fmt.Errorf("decode run: %w", err)
		}
	}
	if len(runs) == 0 {
		fmt.Fprintf(w, "No agent runs in %s.\n", t.label())
	} else if err := printTable(w, []string{"ID", colHeaderAgent, "STATUS", "TRIGGER", "BRANCH", colHeaderCreated}, runs, func(r api.ProjectAgentRun) []string {
		return []string{tuiutil.SanitizeTerminalLabel(r.ID), tuiutil.SanitizeTerminalLabel(r.Agent.Name), tuiutil.SanitizeTerminalLabel(r.Status),
			tuiutil.SanitizeTerminalLabel(r.Trigger.Kind), tuiutil.SanitizeTerminalLabel(r.Branch), r.CreatedAt.Format(time.RFC3339)}
	}); err != nil {
		return err
	}
	if page.NextCursor != nil && *page.NextCursor != "" {
		// The cursor is the server's, and the line is meant to be pasted
		// into a shell, so it is quoted whenever it is not a plain word.
		fmt.Fprintf(w, "More runs: entire project agent run list --cursor %s%s\n", strategy.ShellQuoteForDisplay(tuiutil.SanitizeTerminalLabel(*page.NextCursor)), next)
	}
	return nil
}
