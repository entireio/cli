package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/ticket"
)

func TestFormatTicketState(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"":            "",
		"in_progress": "in progress",
		"done":        "done",
		"in_review":   "in review",
	}
	for in, want := range cases {
		if got := formatTicketState(in); got != want {
			t.Errorf("formatTicketState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatTicketRefLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ref  *checkpoint.TicketRef
		want string
	}{
		{
			name: "full",
			ref:  &checkpoint.TicketRef{ID: "LIN-9", Title: "Ship it", State: "in_progress"},
			want: "LIN-9 — Ship it (in progress)",
		},
		{
			name: "id only",
			ref:  &checkpoint.TicketRef{ID: "LIN-9"},
			want: "LIN-9",
		},
		{
			name: "no id falls back to platform",
			ref:  &checkpoint.TicketRef{Platform: "linear", Title: "Ship it"},
			want: "linear — Ship it",
		},
		{
			name: "title without state",
			ref:  &checkpoint.TicketRef{ID: "LIN-9", Title: "Ship it"},
			want: "LIN-9 — Ship it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := formatTicketRefLine(tc.ref); got != tc.want {
				t.Errorf("formatTicketRefLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTicketBriefFromLink(t *testing.T) {
	t.Parallel()

	got := ticketBriefFromLink(ticket.Link{
		Platform: "linear",
		ID:       "LIN-123",
		Snapshot: &ticket.Snapshot{Title: "Fix login", State: "in_progress", URL: "https://example.com/LIN-123"},
	})
	if got.ID != "LIN-123" || got.Platform != "linear" || got.Title != "Fix login" ||
		got.State != "in_progress" || got.URL != "https://example.com/LIN-123" {
		t.Errorf("unexpected brief: %+v", got)
	}

	// A link without a fetched snapshot carries only identity.
	bare := ticketBriefFromLink(ticket.Link{Platform: "linear", ID: "LIN-9"})
	if bare.ID != "LIN-9" || bare.Title != "" || bare.State != "" || bare.URL != "" {
		t.Errorf("expected identity-only brief, got: %+v", bare)
	}
}

func TestFormatTicketLinkLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		link ticket.Link
		want string
	}{
		{
			name: "full snapshot",
			link: ticket.Link{ID: "LIN-123", Snapshot: &ticket.Snapshot{Title: "Fix login", State: "in_progress"}},
			want: "LIN-123 — Fix login (in progress)",
		},
		{
			name: "no snapshot yet",
			link: ticket.Link{ID: "LIN-123"},
			want: "LIN-123",
		},
		{
			name: "snapshot without title",
			link: ticket.Link{ID: "LIN-123", Snapshot: &ticket.Snapshot{State: "done"}},
			want: "LIN-123 (done)",
		},
		{
			name: "no id falls back to platform",
			link: ticket.Link{Platform: "linear", Snapshot: &ticket.Snapshot{Title: "Fix login"}},
			want: "linear — Fix login",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := formatTicketLinkLine(tc.link); got != tc.want {
				t.Errorf("formatTicketLinkLine() = %q, want %q", got, tc.want)
			}
		})
	}
}
