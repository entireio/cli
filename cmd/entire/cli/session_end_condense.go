package cli

import (
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/spf13/cobra"
)

// envSessionEndSyncCondense forces the SessionEnd hook to run the eager
// condense inline instead of handing it to a detached child. Any non-empty
// value enables it (including "0"/"false", matching the ACCESSIBLE
// convention). Integration tests set it for determinism; it also serves as
// an escape hatch when the detached child is undesirable (e.g. debugging).
const envSessionEndSyncCondense = "ENTIRE_SESSION_END_SYNC"

// sessionEndCondenseSpawn is the process-spawn seam used by
// handleLifecycleSessionEnd. SpawnDetached is already a no-op under `go test`
// (see execx.SpawnDetached); this seam exists so unit tests can record that a
// detached condense was requested, and with which arguments. Production code
// always uses spawnDetachedSessionEndCondense.
var sessionEndCondenseSpawn = spawnDetachedSessionEndCondense

// spawnDetachedSessionEndCondense starts `entire __condense_session <id>` as
// a detached child so the eager condense survives the SessionEnd hook being
// cancelled. Agents give SessionEnd hooks a short budget and do not wait for
// them on exit — empirically ~1.5s for Claude Code (v2.1.x, observed 2026-07;
// undocumented, so treat the figure as approximate). Other comments reference
// this one rather than restating the number. The child runs from the worktree
// root because the strategy resolves the repo and session store from its
// working directory.
//
// The returned error means the child never started (e.g. the local-dev cache
// binary was deleted between exec and spawn); callers stay fail-open but must
// log it — the parent is the last process with working logging.
func spawnDetachedSessionEndCondense(worktreeRoot, sessionID string) error {
	if err := execx.SpawnDetached(worktreeRoot, "__condense_session", sessionID); err != nil {
		return fmt.Errorf("spawn detached session-end condense: %w", err)
	}
	return nil
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
			// create a stray .entire/logs/ in an arbitrary directory
			// (logging.Init falls back to cwd when WorktreeRoot fails).
			if _, err := paths.WorktreeRoot(ctx); err == nil {
				logging.SetLogLevelGetter(GetLogLevel)
				if err := logging.Init(ctx, ""); err == nil {
					defer logging.Close()
				}
			}
			sessionID := args[0]
			if err := GetStrategy(ctx).CondenseAndMarkFullyCondensed(ctx, sessionID); err != nil {
				// Fail-open, like the inline condense it replaces: PostCommit
				// retries on the next commit, and `entire doctor` detects and
				// offers to condense stuck ended sessions. (The exited-session
				// sweep does NOT cover this — it only finalizes ACTIVE
				// sessions whose owner died, and this session is already
				// ENDED.)
				logging.Warn(logging.WithComponent(ctx, "lifecycle"),
					"detached session-end condense failed",
					slog.String("session_id", sessionID),
					slog.String("error", err.Error()))
			}
			return nil
		},
	}
}
