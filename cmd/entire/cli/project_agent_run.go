package cli

import (
	"context"
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
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// projectAgentRunStatuses are the statuses GET .../runs filters by.
var projectAgentRunStatuses = strings.Fields("queued running succeeded failed timed_out canceled")

// projectAgentRunDefaultLimit is run list's page size without --limit.
const projectAgentRunDefaultLimit = 20

// newProjectAgentRunCmd is `entire project agent run`: the runs of the
// project's agents, across trails. A trail's own view of its agents is
// `entire trail finding`, and `entire trail rerun-review` starts them.
func newProjectAgentRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "List, inspect, cancel, and retry the project's agent runs",
		Long: `List, inspect, cancel, and retry the runs of the project's review agents.

Each run is one agent reviewing one trail change. 'entire trail finding' shows
the latest run of each agent on a change, and 'entire trail rerun-review'
starts new ones.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newProjectAgentRunListCmd(), newProjectAgentRunShowCmd(), newProjectAgentRunCancelCmd(), newProjectAgentRunRetryCmd())
	return cmd
}

func (t *projectAgentTarget) runsPath() string { return t.base + "/runs" }

func (t *projectAgentTarget) runPath(id string) string {
	return t.runsPath() + "/" + url.PathEscape(id)
}

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
		out += " --project " + tuiutil.SanitizeTerminalLabel(project)
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
	} else if err := printTable(w, []string{"ID", colHeaderAgent, "STATUS", "TRIGGER", "BRANCH", "CREATED"}, runs, func(r api.ProjectAgentRun) []string {
		return []string{tuiutil.SanitizeTerminalLabel(r.ID), tuiutil.SanitizeTerminalLabel(r.Agent.Name), tuiutil.SanitizeTerminalLabel(r.Status),
			tuiutil.SanitizeTerminalLabel(r.Trigger.Kind), tuiutil.SanitizeTerminalLabel(r.Branch), r.CreatedAt.Format(time.RFC3339)}
	}); err != nil {
		return err
	}
	if page.NextCursor != nil && *page.NextCursor != "" {
		fmt.Fprintf(w, "More runs: entire project agent run list --cursor %s%s\n", tuiutil.SanitizeTerminalLabel(*page.NextCursor), next)
	}
	return nil
}

// projectAgentRunArg is the run ID argument, checked before any request.
func projectAgentRunArg(args []string) (string, error) {
	id := strings.TrimSpace(args[0])
	if !looksLikeULID(id) {
		return "", fmt.Errorf("expected a run ID (see 'entire project agent run list'), got %q", tuiutil.SanitizeTerminalLabel(id))
	}
	return id, nil
}

// projectAgentRunAction runs one request on a run and prints the run the
// server returns: as served with --json, else through describe.
func projectAgentRunAction(cmd *cobra.Command, args []string, request func(ctx context.Context, t *projectAgentTarget, id string, out *json.RawMessage) error, describe func(w io.Writer, id string, run api.ProjectAgentRun)) error {
	cmd.SilenceUsage = true
	id, err := projectAgentRunArg(args)
	if err != nil {
		return err
	}
	t, err := resolveProjectAgentTarget(cmd)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := request(cmd.Context(), t, id, &raw); err != nil {
		return err
	}
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), raw)
	}
	var run api.ProjectAgentRun
	if err := json.Unmarshal(raw, &run); err != nil {
		return fmt.Errorf("decode run: %w", err)
	}
	describe(cmd.OutOrStdout(), id, run)
	return nil
}

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

func printProjectAgentRun(w io.Writer, _ string, run api.ProjectAgentRun) {
	field := func(name, value string) {
		if value != "" {
			fmt.Fprintf(w, "%-10s %s\n", name+":", value)
		}
	}
	field("Run", tuiutil.SanitizeTerminalLabel(run.ID))
	agent := tuiutil.SanitizeTerminalLabel(run.Agent.Name)
	if run.Agent.ID != "" {
		agent += fmt.Sprintf(" (%s, version %d)", tuiutil.SanitizeTerminalLabel(run.Agent.ID), run.Agent.Version)
	}
	field("Agent", agent)
	status := tuiutil.SanitizeTerminalLabel(run.Status)
	if run.Error != nil && run.Error.Message != "" {
		status += ": " + tuiutil.SanitizeTerminalText(run.Error.Message)
	}
	field("Status", status)
	trigger := tuiutil.SanitizeTerminalLabel(run.Trigger.Kind)
	if run.RetryOf != nil {
		trigger += fmt.Sprintf(" of %s (attempt %d)", tuiutil.SanitizeTerminalLabel(*run.RetryOf), run.Attempt)
	}
	field("Trigger", trigger)
	if t := run.Target; t != nil {
		change := tuiutil.SanitizeTerminalLabel(t.Ref)
		if t.HeadSHA != "" {
			change += " at " + shortSHA(tuiutil.SanitizeTerminalLabel(t.HeadSHA))
		}
		field("Change", change)
		field("Trail", tuiutil.SanitizeTerminalLabel(t.TrailID))
	}
	if !run.CreatedAt.IsZero() {
		field("Created", run.CreatedAt.Format(time.RFC3339))
	}
	if r := run.ReviewChangesResult; r != nil {
		value := strings.Trim(string(r.Signal.Value), `"`)
		if r.Signal.ValueType == "percent" {
			value += "%"
		}
		field("Reading", tuiutil.SanitizeTerminalLabel(value))
		field("Findings", strconv.Itoa(len(r.Findings)))
		if r.Summary != nil {
			fmt.Fprintf(w, "\n%s\n", tuiutil.SanitizeTerminalText(*r.Summary))
		}
	}
	if run.Diagnostic != nil && *run.Diagnostic != "" {
		fmt.Fprintf(w, "\nDiagnostic (process output tail):\n%s\n", tuiutil.SanitizeTerminalText(*run.Diagnostic))
	}
}
