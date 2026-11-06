package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

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
