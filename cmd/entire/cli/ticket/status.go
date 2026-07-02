package ticket

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// newStatusCmd builds `entire ticket status`.
func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the ticket platform and the current branch's linked ticket",
		Long: `Show what Entire has configured and linked for tickets:
the active platform and team, whether a credential is stored, and the ticket
linked to the current branch (with its live title and state when reachable).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd.Context(), cmd.OutOrStdout(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// statusReport is the machine-readable form of `ticket status`.
type statusReport struct {
	Configured  bool     `json:"configured"`
	Platform    string   `json:"platform,omitempty"`
	Team        string   `json:"team,omitempty"`
	Credential  bool     `json:"credential"`
	Branch      string   `json:"branch,omitempty"`
	TicketID    string   `json:"ticket_id,omitempty"`
	TicketTitle string   `json:"ticket_title,omitempty"`
	TicketState string   `json:"ticket_state,omitempty"`
	TicketURL   string   `json:"ticket_url,omitempty"`
	Changes     []string `json:"changes,omitempty"`
}

func runStatus(ctx context.Context, out io.Writer, asJSON bool) error {
	rep := gatherStatus(ctx)
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return fmt.Errorf("encode status: %w", err)
		}
		return nil
	}
	printStatus(out, rep)
	return nil
}

// gatherStatus reads local config and link state, then best-effort fetches the
// linked ticket for its live title/state. Network or credential failures leave
// the live fields empty rather than erroring.
func gatherStatus(ctx context.Context) statusReport {
	var rep statusReport

	if s, err := settings.Load(ctx); err == nil {
		if tc := s.TicketConfig(); !tc.IsZero() {
			rep.Configured = true
			rep.Platform = tc.Platform
			rep.Team = tc.Team
			if platform, perr := ParsePlatform(tc.Platform); perr == nil {
				if _, terr := LoadToken(platform); terr == nil {
					rep.Credential = true
				}
			}
		}
	}

	branch, err := currentBranch(ctx)
	if err != nil {
		return rep
	}
	rep.Branch = branch

	link, ok, err := CurrentLink(ctx)
	if err != nil || !ok {
		return rep
	}
	rep.TicketID = link.ID

	if prov, perr := activeProvider(ctx); perr == nil {
		if task, ferr := prov.Fetch(ctx, link.ID); ferr == nil {
			rep.TicketTitle = task.Title
			rep.TicketState = string(task.State)
			rep.TicketURL = task.URL
			if changes, cerr := refreshSnapshot(ctx, branch, &task); cerr == nil {
				rep.Changes = changes
			}
		}
	}
	return rep
}

func printStatus(out io.Writer, rep statusReport) {
	if !rep.Configured {
		fmt.Fprintln(out, "Ticket platform: not configured (run `entire ticket setup`)")
		return
	}

	fmt.Fprintf(out, "Ticket platform: %s (team %s)\n", rep.Platform, rep.Team)
	if rep.Credential {
		fmt.Fprintln(out, "Credential:      present")
	} else {
		fmt.Fprintln(out, "Credential:      missing (run `entire ticket setup`)")
	}
	if rep.Branch != "" {
		fmt.Fprintf(out, "Branch:          %s\n", rep.Branch)
	}

	if rep.TicketID == "" {
		fmt.Fprintln(out, "Linked ticket:   (none — run `entire ticket link <id>`)")
		return
	}
	fmt.Fprintf(out, "Linked ticket:   %s\n", rep.TicketID)
	if rep.TicketTitle != "" {
		state := ""
		if rep.TicketState != "" {
			state = " [" + rep.TicketState + "]"
		}
		fmt.Fprintf(out, "                 %s%s\n", rep.TicketTitle, state)
	}
	if rep.TicketURL != "" {
		fmt.Fprintf(out, "                 %s\n", rep.TicketURL)
	}
	if len(rep.Changes) > 0 {
		fmt.Fprintf(out, "⚠️  Changed since last sync: %s\n", strings.Join(rep.Changes, "; "))
	}
}
