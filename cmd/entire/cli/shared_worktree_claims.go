package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// git status shows every dirty file in the worktree, not which session
// changed it. A turn end, and a foreground task's completion, use it to find
// edits no transcript names (shell commands), and statusClaim narrows what
// they take from it to what the turn or task could have changed (#3011).

// statusClaim is what a turn end or a foreground task completion needs to
// narrow git status's view of its changes.
type statusClaim struct {
	repoRoot  string
	sessionID string

	// related are sessions whose activity is not another session's turn:
	// the parent of a subagent that runs as a session of its own.
	related []string

	// forTask marks a foreground task's completion, for which the session's
	// own subagent sessions are other tasks' work.
	forTask bool

	// capturedAt is when the prompt-start or task-launch baseline was
	// captured; zero when unknown.
	capturedAt time.Time

	// dirtyTracked is the baseline's DirtyTrackedFileStats.
	dirtyTracked map[string]UntrackedFileStat

	// named are the files the transcript names, repo-relative.
	named []string
}

// narrow returns the files of modified, created and deleted (git status's
// view, repo-relative) that the turn or task can claim:
//
//   - A tracked file that was already dirty when the baseline was captured,
//     and whose stat still matches, is left out: it was not changed since.
//   - While another session in the worktree was active at some point since
//     the capture (strategy.OtherSessionActiveSince), only files the
//     transcript names are kept. Either session could have changed any other
//     file git status lists, and taking it would link this session to the
//     other's commit and show its prompt as that commit's intent. A shell
//     edit made during such an overlap is therefore claimed by neither
//     session, the same trade the background-task path makes
//     (unrecordedNewFilesSinceLaunch). If the other sessions cannot be read,
//     the overlap is assumed.
func (c statusClaim) narrow(ctx context.Context, modified, created, deleted []string) (claimedModified, claimedCreated, claimedDeleted []string) {
	modified = c.withoutUnchangedDirty(modified)
	deleted = c.withoutUnchangedDirty(deleted)
	claimedModified = onlyFiles(modified, c.named)
	claimedCreated = onlyFiles(created, c.named)
	claimedDeleted = onlyFiles(deleted, c.named)
	unnamed := len(modified) + len(created) + len(deleted) - len(claimedModified) - len(claimedCreated) - len(claimedDeleted)
	if unnamed == 0 || c.capturedAt.IsZero() {
		return modified, created, deleted
	}
	logCtx := logging.WithComponent(ctx, "lifecycle")
	overlapped, err := strategy.OtherSessionActiveSince(ctx, c.repoRoot, c.sessionID, c.related, c.capturedAt, c.forTask)
	if err != nil {
		logging.Warn(logCtx, "cannot read other sessions in the worktree; claiming only files the transcript names",
			slog.String("error", err.Error()))
		overlapped = true
	}
	if !overlapped {
		return modified, created, deleted
	}
	logging.Info(logCtx, "another session was active in the worktree; not claiming files only git status shows",
		slog.Int("files", unnamed))
	return claimedModified, claimedCreated, claimedDeleted
}

// withoutUnchangedDirty drops the files the baseline recorded as already
// dirty whose stat still matches.
func (c statusClaim) withoutUnchangedDirty(files []string) []string {
	if len(c.dirtyTracked) == 0 {
		return files
	}
	var kept []string
	for _, file := range files {
		if before, dirty := c.dirtyTracked[file]; dirty {
			if now, ok := statTrackedFile(c.repoRoot, file); ok && now == before {
				continue
			}
		}
		kept = append(kept, file)
	}
	return kept
}

// onlyFiles returns the files of all that are also in keep, in all's order.
func onlyFiles(all, keep []string) []string {
	if len(all) == 0 || len(keep) == 0 {
		return nil
	}
	keepSet := make(map[string]struct{}, len(keep))
	for _, f := range keep {
		keepSet[f] = struct{}{}
	}
	var kept []string
	for _, f := range all {
		if _, ok := keepSet[f]; ok {
			kept = append(kept, f)
		}
	}
	return kept
}

// statusClaim returns the statusClaim for the turn this state began. A nil
// state has no baseline, and narrows nothing.
func (s *PrePromptState) statusClaim(repoRoot, sessionID string, named []string, related ...string) statusClaim {
	if s == nil {
		return statusClaim{}
	}
	return newStatusClaim(repoRoot, sessionID, s.Timestamp, s.DirtyTrackedFileStats, named, related)
}

// statusClaim returns the statusClaim for the task this state began. A nil
// state has no baseline, and narrows nothing.
func (s *PreTaskState) statusClaim(repoRoot, sessionID string, named []string) statusClaim {
	if s == nil {
		return statusClaim{}
	}
	c := newStatusClaim(repoRoot, sessionID, s.Timestamp, s.DirtyTrackedFileStats, named, nil)
	c.forTask = true
	return c
}

func newStatusClaim(repoRoot, sessionID, timestamp string, dirtyTracked map[string]UntrackedFileStat, named, related []string) statusClaim {
	c := statusClaim{
		repoRoot:     repoRoot,
		sessionID:    sessionID,
		capturedAt:   parseCaptureTime(timestamp),
		dirtyTracked: dirtyTracked,
		named:        named,
	}
	for _, id := range related {
		if id != "" {
			c.related = append(c.related, id)
		}
	}
	return c
}

// parseCaptureTime parses a pre-prompt or pre-task Timestamp; zero when it is
// empty or malformed.
func parseCaptureTime(timestamp string) time.Time {
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return time.Time{}
	}
	return t
}
