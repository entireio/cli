package cli

// ticket_bridge.go wires cli-package implementations into the ticket
// subpackage's NewCommand Deps struct. The bridge lives in the cli package so
// the ticket subpackage does not import the per-agent packages (import cycle).

import (
	"github.com/entireio/cli/cmd/entire/cli/agentlaunch"
	"github.com/entireio/cli/cmd/entire/cli/ticket"
)

// buildTicketDeps builds the ticket.Deps used by ticket.NewCommand.
func buildTicketDeps() ticket.Deps {
	return ticket.Deps{
		LaunchFix: agentlaunch.LaunchFixAgent,
	}
}
