package cli

import (
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/spf13/cobra"
)

// envSessionEndSyncCondense forces the SessionEnd hook to run the eager
// condense inline instead of handing it to a detached child. Integration
// tests set it for determinism; it also serves as an escape hatch when the
// detached child is undesirable (e.g. debugging).
const envSessionEndSyncCondense = "ENTIRE_SESSION_END_SYNC"

// sessionEndCondenseSpawn is the process-spawn seam used by
// handleLifecycleSessionEnd. Swapped in tests so they can assert the hook
// requests a detached condense without forking a real subprocess (a real
// `go test` binary doesn't understand `__condense_session` as an argument).
// Production code always uses spawnDetachedSessionEndCondense.
var sessionEndCondenseSpawn = spawnDetachedSessionEndCondense

// spawnDetachedSessionEndCondense starts `entire __condense_session <id>` as
// a detached child so the eager condense survives the SessionEnd hook being
// cancelled (agents give these hooks a short budget — Claude Code cancels
// after ~1.5s — and never wait for them on exit). The child runs from the
// worktree root because the strategy resolves the repo and session store
// from its working directory.
func spawnDetachedSessionEndCondense(worktreeRoot, sessionID string) {
	execx.SpawnDetached(worktreeRoot, "__condense_session", sessionID)
}

// newCondenseSessionCmd creates the hidden command that runs the eager
// session-end condense out of band. It is invoked by
// spawnDetachedSessionEndCondense from a detached subprocess and should not
// be called directly.
func newCondenseSessionCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__condense_session <session-id>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Detached child with discarded stdout/stderr: initialize file
			// logging so a failing condense is diagnosable in
			// .entire/logs/entire.log rather than vanishing. Guard on
			// WorktreeRoot first — matching __refresh_trail_enablement — so a
			// child whose worktree was removed between spawn and exec doesn't
			// create a stray .entire/logs/ in an arbitrary directory.
			if _, err := paths.WorktreeRoot(ctx); err == nil {
				logging.SetLogLevelGetter(GetLogLevel)
				if err := logging.Init(ctx, ""); err == nil {
					defer logging.Close()
				}
			}
			sessionID := args[0]
			if err := GetStrategy(ctx).CondenseAndMarkFullyCondensed(ctx, sessionID); err != nil {
				// Fail-open, like the inline condense it replaces: PostCommit
				// retries on the next commit and the exited-session sweep
				// retries from status/doctor.
				logging.Warn(logging.WithComponent(ctx, "lifecycle"),
					"detached session-end condense failed",
					slog.String("session_id", sessionID),
					slog.String("error", err.Error()))
			}
			return nil
		},
	}
}
