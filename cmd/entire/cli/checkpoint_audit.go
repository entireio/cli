package cli

import (
	"context"
	"errors"

	"github.com/entireio/cli/cmd/entire/cli/review/intentlens"
	"github.com/spf13/cobra"
)

type checkpointEvidenceCollector interface {
	Collect(ctx context.Context, checkpointID string) (intentlens.EvidencePackage, error)
}

func newCheckpointAuditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "audit <checkpoint-id>",
		Short: "Audit a checkpoint against its collected implementation evidence",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("checkpoint audit evidence collection is not integrated; it must supply checkpoint, source, git diff, graph, and test evidence")
		},
	}
}
