package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// snapshotAgentStop records the worktree as it stands when an agent stops
// without a turn-end step of its own: a turn whose transcript and change
// detection named no files (a shell command can still have written some), or
// a background subagent finishing after its parent's turn ended. Everything
// changed while an agent was busy is agent work, so the snapshot must exist
// before the next prompt's human diff or a commit reads it. The write is
// skipped when nothing changed since the previous snapshot, and nothing is
// written for a session whose state is gone or ended (no resurrection).
func snapshotAgentStop(ctx context.Context, ag agent.Agent, sessionID, commitMessage string) error {
	// The snapshot carries the session's metadata directory, which a turn
	// end creates; a subagent can stop before its parent's first turn end.
	root, err := entiredir.Open(ctx)
	if err != nil {
		return fmt.Errorf("open .entire: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, sessionMetadataName(sessionID), 0o750); err != nil {
		return fmt.Errorf("create session metadata directory: %w", err)
	}
	author, err := GetGitAuthor(ctx)
	if err != nil {
		return fmt.Errorf("get git author: %w", err)
	}
	err = GetStrategy(ctx).SaveStep(ctx, strategy.StepContext{
		SessionID:     sessionID,
		MetadataDir:   paths.SessionMetadataDirFromSessionID(sessionID),
		CommitMessage: commitMessage,
		AuthorName:    author.Name,
		AuthorEmail:   author.Email,
		AgentType:     ag.Type(),
		// A stop with nothing changed since the last snapshot writes nothing,
		// and a swept or ended session is left alone (judged under the
		// save's lock, so it cannot race session cleanup).
		SkipWhenUnchanged:   true,
		ExistingSessionOnly: true,
	})
	if errors.Is(err, strategy.ErrStateNotFound) || errors.Is(err, strategy.ErrNothingToSnapshot) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("snapshot worktree: %w", err)
	}
	return nil
}
