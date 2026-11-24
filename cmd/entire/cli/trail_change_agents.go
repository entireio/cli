package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// fetchTrailChangeAgents is the change's project agents as the server sent
// them. A project change read already carries them; a repository list read
// does not, so the change's detail is read for them. The agents are a view
// beside the findings: a failed read warns and leaves them out.
func fetchTrailChangeAgents(ctx context.Context, errW io.Writer, client *api.Client, work api.TrailResource) json.RawMessage {
	if !api.RawJSONAbsent(work.Agents) {
		return work.Agents
	}
	if work.ID == "" {
		return nil
	}
	resp, err := client.Get(ctx, "/api/v1/trails/"+url.PathEscape(work.ID))
	if err == nil {
		defer resp.Body.Close()
		err = checkTrailResponse(resp)
	}
	var detail api.TrailResource
	if err == nil {
		err = api.DecodeJSON(resp, &detail)
	}
	if err != nil {
		fmt.Fprintf(errW, "Warning: could not read the change's agents: %s\n", tuiutil.SanitizeTerminalText(err.Error()))
		return nil
	}
	if api.RawJSONAbsent(detail.Agents) {
		return nil
	}
	return detail.Agents
}

// trailAgentWaiting says why an agent has not run on the change yet.
var trailAgentWaiting = map[string]string{
	"ready":     "waits for ready for review",
	"not_ready": "runs only before ready for review",
	"push":      "waits for the next push",
}

// printTrailChangeAgents renders the change's agents: each one's latest
// reading and run, or what it waits for. Nothing is printed when the read did
// not carry them.
func printTrailChangeAgents(w io.Writer, raw json.RawMessage) error {
	if api.RawJSONAbsent(raw) {
		return nil
	}
	agents, err := (&api.TrailResource{Agents: raw}).DecodeAgents()
	if err != nil {
		fmt.Fprintf(w, "  Agents: unreadable (%s); pass --json for the raw data\n\n", tuiutil.SanitizeTerminalText(err.Error()))
		return nil
	}
	if len(agents) == 0 {
		fmt.Fprintln(w, "  Agents: no project agent is on this change (see 'entire project agent list')")
		fmt.Fprintln(w)
		return nil
	}
	fmt.Fprintln(w, "  Agents:")
	err = printTable(w, []string{colHeaderAgent, "READING", "TREND", "STATUS", "HEAD", "RUN"}, agents, func(a api.TrailAgent) []string {
		status, head, run := "", "", ""
		if a.LatestRun != nil {
			status = a.LatestRun.Status
			head = shortSHA(stringPtrValue(a.LatestRun.HeadSHA))
			run = a.LatestRun.ID
		} else if a.WaitingFor != nil {
			status = trailAgentWaiting[*a.WaitingFor]
			if status == "" {
				status = "waiting: " + *a.WaitingFor
			}
		}
		return []string{tuiutil.SanitizeTerminalLabel(a.Agent.Name), trailAgentReadingText(a.Reading), trailAgentTrendText(a.TrendDelta),
			tuiutil.SanitizeTerminalLabel(status), tuiutil.SanitizeTerminalLabel(head), tuiutil.SanitizeTerminalLabel(run)}
	})
	fmt.Fprintln(w)
	return err
}

func trailAgentReadingText(r *api.TrailAgentReading) string {
	switch {
	case r == nil:
		return "-"
	case r.PercentValue != nil:
		return strconv.Itoa(*r.PercentValue) + "%"
	case r.SizeValue != nil:
		return tuiutil.SanitizeTerminalLabel(*r.SizeValue)
	case r.BooleanValue != nil:
		return strconv.FormatBool(*r.BooleanValue)
	}
	return "-"
}

func trailAgentTrendText(delta *int) string {
	switch {
	case delta == nil:
		return ""
	case *delta > 0:
		return "+" + strconv.Itoa(*delta)
	}
	return strconv.Itoa(*delta)
}
