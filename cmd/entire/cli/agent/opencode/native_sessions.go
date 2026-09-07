package opencode

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time assertion that OpenCode can list its own untracked sessions.
var _ agent.NativeSessionLister = (*OpenCodeAgent)(nil)

// ListNativeSessions implements agent.NativeSessionLister (entireio/cli#1992):
// it lists sessions from OpenCode's own store — including ones Entire's hooks
// never observed, since OpenCode is DB-backed rather than file-based — scoped
// to dir (the current worktree root) exactly as described by the issue: a
// session's reported directory must equal dir or be a directory below it.
// Sessions with no directory recorded, or one Entire cannot resolve against
// dir, are excluded rather than guessed at.
func (a *OpenCodeAgent) ListNativeSessions(ctx context.Context, dir string) ([]agent.NativeSessionInfo, error) {
	entries, err := runOpenCodeSessionList(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]agent.NativeSessionInfo, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		if !directoryWithinWorktree(e.Directory, dir) {
			continue
		}
		out = append(out, agent.NativeSessionInfo{
			SessionID: e.ID,
			Title:     e.Title,
			Directory: e.Directory,
			UpdatedAt: epochMillisToTime(e.UpdatedAt),
		})
	}
	return out, nil
}

// directoryWithinWorktree reports whether candidate is worktreeRoot itself or
// a directory below it. Both are resolved to cleaned absolute paths first, so
// e.g. a trailing slash or "." component doesn't produce a false negative. An
// unresolvable candidate (empty, or Abs fails) is never considered in scope —
// a session with no usable directory is excluded, not guessed at.
func directoryWithinWorktree(candidate, worktreeRoot string) bool {
	if strings.TrimSpace(candidate) == "" {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rootAbs, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return false
	}
	candidateAbs = filepath.Clean(candidateAbs)
	rootAbs = filepath.Clean(rootAbs)
	if candidateAbs == rootAbs {
		return true
	}
	rel, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// epochMillisToTime converts an epoch-milliseconds timestamp (the export
// format's convention for SessionInfo.CreatedAt/UpdatedAt) to time.Time,
// returning the zero value for a non-positive input so a session with no
// reported update time renders as "unknown" rather than the Unix epoch.
func epochMillisToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
