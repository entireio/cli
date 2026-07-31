package cli

import (
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// Typed resume failures, recoverable via errors.As (including through
// SilentError wrapping). Named Resume* rather than TrailResume* because the
// shared restore engine means `entire session resume` surfaces the
// no-checkpoint and metadata-unavailable cases too.

// ResumeWorktreeClashError reports that the resume target branch is already
// checked out in another worktree. WorktreePath carries that worktree's path
// so an orchestrating caller can re-run resume from the right directory.
type ResumeWorktreeClashError struct {
	Branch       string
	WorktreePath string
}

func (e *ResumeWorktreeClashError) Error() string {
	return fmt.Sprintf("branch %q is already checked out in another worktree: %s", e.Branch, e.WorktreePath)
}

// ResumeNoCheckpointError reports that the branch has no Entire checkpoint,
// so there is no session to resume.
type ResumeNoCheckpointError struct {
	Branch string
}

func (e *ResumeNoCheckpointError) Error() string {
	return fmt.Sprintf("no Entire checkpoint found on branch %q — nothing to resume; start a new agent session on this branch to create checkpoints", e.Branch)
}

// ResumeMetadataUnavailableError reports that a checkpoint referenced by a
// commit trailer exists but its metadata could not be read locally or fetched
// from any remote.
type ResumeMetadataUnavailableError struct {
	CheckpointID id.CheckpointID
}

func (e *ResumeMetadataUnavailableError) Error() string {
	return fmt.Sprintf("checkpoint %s metadata is not available locally or from a remote", e.CheckpointID)
}
