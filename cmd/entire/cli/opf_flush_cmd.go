package cli

import (
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// newOPFFlushCmd creates the hidden command the pre-push path spawns detached
// to rewrite queued checkpoint refs with the OpenAI privacy filter in the
// background (see strategy.RunOPFFlush). The model call it makes used to run
// inline in the pre-push hook, where the user's `git push` waited on it.
//
// Not for direct use: it rewrites, never pushes, so running it by hand
// delivers nothing. `entire push` is the interactive surface for delivery.
func newOPFFlushCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__opf_flush",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Detached child with discarded stdout/stderr: make sure a file
			// logger is attached so a flush that cannot make progress (a ref
			// over the per-ref cap, a broken OPF runtime) is diagnosable in
			// .entire/logs/entire.log rather than vanishing. Idempotent, and it
			// resolves the worktree root itself — a child whose worktree was
			// removed between spawn and exec gets no logger rather than a stray
			// .entire/logs/ in an arbitrary directory. The root
			// PersistentPostRun closes whichever logger ends up on the context.
			ensureLogger(cmd)
			// RunOPFFlush is best-effort by contract and never returns a
			// non-nil error, so there is no top-level error to log here the way
			// __sweep_sessions does: nothing watches this child's exit code.
			return strategy.RunOPFFlush(cmd.Context())
		},
	}
}
