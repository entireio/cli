package strategy

import (
	"context"
	"log/slog"

	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/ticket"
)

// ticketRefForBranch captures the external tracker ticket linked to branch (if
// any) as durable checkpoint provenance. It reads only the locally stored link
// and snapshot (no network fetch), so it adds no latency to condensation.
//
// Best-effort: any error or missing link yields nil, so a checkpoint is never
// blocked by ticket state. The strategy layer owns this ticket.Link → TicketRef
// mapping so api/checkpoint stays free of a ticket-package dependency.
func ticketRefForBranch(ctx context.Context, branch string) *cpkg.TicketRef {
	link, ok, err := ticket.LinkForBranch(ctx, branch)
	if err != nil || !ok {
		return nil
	}
	ref := &cpkg.TicketRef{Platform: link.Platform, ID: link.ID}
	if link.Snapshot != nil {
		ref.Title = link.Snapshot.Title
		ref.State = link.Snapshot.State
		ref.URL = link.Snapshot.URL
		ref.Digest = link.Snapshot.Digest
		ref.FetchedAt = link.Snapshot.FetchedAt
	}
	logging.Debug(ctx, "ticket captured into checkpoint provenance",
		slog.String("platform", ref.Platform),
		slog.String("ticket", ref.ID),
		slog.String("branch", branch))
	return ref
}
