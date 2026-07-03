package cli

import (
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/ticket"
)

// Shared rendering for the external-tracker ticket linked to a branch. Two
// input shapes exist: the live ticket.Link (from ticket.LinkForBranch, used by
// status/session-info/review) and the frozen checkpoint.TicketRef (captured
// into per-session metadata, used by explain). Both collapse to a single-line
// display, e.g. "LIN-123 — Fix login redirect (in progress)".

// formatTicketState humanizes a raw tracker state value ("in_progress" →
// "in progress"); an empty state yields an empty string so callers can omit the
// parenthetical entirely. (The ticket package's own displayState renders empty
// as "unknown", which is the wrong default for these compact one-liners.)
func formatTicketState(state string) string {
	if state == "" {
		return ""
	}
	return strings.ReplaceAll(state, "_", " ")
}

// formatTicketRefLine renders a frozen checkpoint.TicketRef for single-line
// display, e.g. "LIN-123 — Fix login redirect (in progress)". Title and state
// are omitted when the snapshot never recorded them; the URL is rendered
// separately by the caller as a continuation line.
func formatTicketRefLine(t *checkpoint.TicketRef) string {
	label := t.ID
	if label == "" {
		label = t.Platform
	}
	if t.Title != "" {
		label += " — " + t.Title
	}
	if state := formatTicketState(t.State); state != "" {
		label += fmt.Sprintf(" (%s)", state)
	}
	return label
}

// formatTicketLinkLine renders a live ticket.Link for single-line display, e.g.
// "LIN-123 — Fix login redirect (in progress)". The ID is always present; title
// and state come from the last-fetched snapshot and are omitted when it has
// none.
func formatTicketLinkLine(link ticket.Link) string {
	label := link.ID
	if label == "" {
		label = link.Platform
	}
	if link.Snapshot != nil {
		if link.Snapshot.Title != "" {
			label += " — " + link.Snapshot.Title
		}
		if state := formatTicketState(link.Snapshot.State); state != "" {
			label += fmt.Sprintf(" (%s)", state)
		}
	}
	return label
}

// ticketBriefJSON is the structured ticket shape shared by the `--json` output
// of the CLI surfaces (status, session info). It projects a live ticket.Link
// onto plain fields; the digest is intentionally omitted as an internal detail.
type ticketBriefJSON struct {
	Platform string `json:"platform,omitempty"`
	ID       string `json:"id"`
	Title    string `json:"title,omitempty"`
	State    string `json:"state,omitempty"`
	URL      string `json:"url,omitempty"`
}

// ticketBriefFromLink projects a live ticket.Link onto the JSON brief.
func ticketBriefFromLink(link ticket.Link) *ticketBriefJSON {
	brief := &ticketBriefJSON{Platform: link.Platform, ID: link.ID}
	if link.Snapshot != nil {
		brief.Title = link.Snapshot.Title
		brief.State = link.Snapshot.State
		brief.URL = link.Snapshot.URL
	}
	return brief
}
