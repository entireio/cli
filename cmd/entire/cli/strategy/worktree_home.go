package strategy

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// homeVerdict is what a session's home marker (State.WorktreeIncarnation)
// says about whether a worktree is the session's home.
type homeVerdict int

const (
	// homeUnknown: the marker can't tell. The session is homed in the main
	// worktree, was homed before markers were kept, or the check failed;
	// callers keep their path rule.
	homeUnknown homeVerdict = iota
	// homeHere: the worktree is the home, at its recorded path.
	homeHere
	// homeMovedHere: the worktree is the home, moved here by `git worktree
	// move` from its recorded path.
	homeMovedHere
	// homeNotHere: a worktree under the home's ID that can't be confirmed as
	// the home (a marker Entire didn't write, a symlink in its place, a copy).
	// A worktree with another ID is homeUnknown: the marker says nothing about
	// it, and the path rules (the sibling fallback among them) decide.
	homeNotHere
	// homeReplaced: a worktree git added at the home's path, under the
	// home's ID, after removing the home: known not to be it.
	homeReplaced
)

// worktreeAt is a hook's worktree, with its git ID resolved once for checking
// several sessions' homes against it.
type worktreeAt struct {
	root     string
	id       string
	idErr    error
	resolved bool
}

// homeAt reports whether the worktree at root is state's home, by its marker.
// Path comparison alone can't tell: `git worktree move` takes the home to a
// new path, and git gives a removed home's path and ID to the next worktree
// added there.
func homeAt(ctx context.Context, state *SessionState, root string) homeVerdict {
	here := &worktreeAt{root: root}
	return here.home(ctx, state)
}

func (w *worktreeAt) home(ctx context.Context, state *SessionState) homeVerdict {
	if state.WorktreeID == "" || state.WorktreeIncarnation == "" || state.WorktreePath == "" || w.root == "" {
		return homeUnknown
	}
	if !w.resolved {
		w.id, w.idErr = paths.GetWorktreeID(w.root)
		w.resolved = true
	}
	if w.idErr != nil {
		return homeUnknown
	}
	if w.id != state.WorktreeID {
		return homeUnknown
	}
	admin, adminPath, closeAdmin, err := openWorktreeAdmin(ctx, w.root, w.id)
	if err != nil {
		return homeUnknown
	}
	defer closeAdmin()

	atPath := isSessionHomeWorktree(w.root, state) || sameDir(w.root, state.WorktreePath)
	marker, err := readIncarnation(admin)
	switch {
	case errors.Is(err, fs.ErrNotExist), err == nil && marker != state.WorktreeIncarnation:
		// The admin directory is there and holds no marker, or another one:
		// git made it for a worktree added since the home was removed.
		if atPath {
			return homeReplaced
		}
		return homeNotHere
	case errors.Is(err, errMalformedMarker), errors.Is(err, osroot.ErrSymlinkedPath):
		return homeNotHere
	case err != nil:
		return homeUnknown
	case atPath:
		return homeHere
	case movedFrom(ctx, admin, adminPath, w.root, state.WorktreePath):
		return homeMovedHere
	default:
		return homeNotHere
	}
}

// movedFrom reports whether the worktree at root, holding the home's marker,
// is the home moved from recorded: git's record of the worktree (its admin
// directory's gitdir file) names root, which a copy of the home does not, and
// recorded is no longer the home's: git lists no worktree there, or one with
// another ID (added at the old path after the move).
func movedFrom(ctx context.Context, admin *os.Root, adminPath, root, recorded string) bool {
	data, err := osroot.ReadFileNoFollow(admin, "gitdir")
	if err != nil {
		return false
	}
	gitDir := strings.TrimSpace(string(data))
	if gitDir == "" {
		return false
	}
	if !filepath.IsAbs(gitDir) {
		// worktree.useRelativePaths records it relative to the admin directory.
		gitDir = filepath.Join(adminPath, gitDir)
	}
	if !sameDir(gitDir, filepath.Join(root, ".git")) {
		return false
	}
	registered, err := gitrepo.ListWorktreePaths(ctx, root)
	if err != nil {
		return false
	}
	for _, p := range registered {
		if !sameDir(p, recorded) {
			continue
		}
		id, err := paths.GetWorktreeID(p)
		return err == nil && id != filepath.Base(adminPath)
	}
	return true
}

// followMovedHome repoints state at root when root is its home moved there
// (homeMovedHere), keeping WorktreeID, which `git worktree move` keeps too,
// and returns the verdict. Only the worktree git lists as the home can claim
// it, so two hooks following the same move converge.
func followMovedHome(ctx context.Context, state *SessionState, root string) homeVerdict {
	verdict := homeAt(ctx, state, root)
	if verdict == homeMovedHere {
		logging.Info(logging.WithComponent(ctx, "session"), "following a moved home worktree",
			slog.String("session_id", state.SessionID),
			slog.String("from", state.WorktreePath),
			slog.String("to", root))
		state.WorktreePath = root
	}
	return verdict
}

// rehome makes the worktree at root, which git added at state's home path
// after removing the home (homeReplaced), the session's home: the agent
// resumed there. What the removed worktree held is gone with it: its pending
// files and their hashes, and the turns no commit carried, which would
// otherwise condense into this worktree's next commit. Tokens are cumulative
// and kept, and background tasks launched before now finish on their own but
// claim nothing here (RehomedAt). The caller moves BaseCommit to root's HEAD.
func rehome(ctx context.Context, state *SessionState, root string) {
	marker, err := MarkWorktree(ctx, root, state.WorktreeID)
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "session"), "could not mark the worktree at the session's home path; not re-homing",
			slog.String("session_id", state.SessionID),
			slog.String("worktree", root),
			slog.String("error", err.Error()))
		return
	}
	logging.Info(logging.WithComponent(ctx, "session"), "re-homing session in a worktree re-added at its home path",
		slog.String("session_id", state.SessionID),
		slog.String("worktree", root),
		slog.Int("dropped_files", len(state.FilesTouched)))
	state.WorktreeIncarnation = marker
	state.FilesTouched, state.TouchedFileHashes = nil, nil
	state.LastCheckpointID = ""
	state.StepCount = 0
	if pos, ok := currentTranscriptPosition(state); ok {
		state.AdvanceDisplayWindow(pos)
	} else {
		// Re-homed anyway: left at the removed worktree, the session could
		// link nothing here. Its earlier turns stay in the window and go
		// with this worktree's next checkpoint.
		logging.Info(logging.WithComponent(ctx, "session"), "re-homed session's transcript position unknown; its earlier turns stay in the next checkpoint",
			slog.String("session_id", state.SessionID))
	}
	now := time.Now()
	state.RehomedAt = &now
}

// currentTranscriptPosition returns the end of state's live transcript in the
// agent's own position units, when the agent can tell.
func currentTranscriptPosition(state *SessionState) (int, bool) {
	ag, err := agent.GetByAgentType(state.AgentType)
	if err != nil {
		return 0, false
	}
	analyzer, ok := agent.AsTranscriptAnalyzer(ag)
	if !ok {
		return 0, false
	}
	transcriptPath, err := resolveTranscriptPath(state)
	if err != nil {
		return 0, false
	}
	pos, err := analyzer.GetTranscriptPosition(transcriptPath)
	if err != nil {
		return 0, false
	}
	return pos, true
}

// stampLinkedHome gives a linked-worktree session homed before markers were
// kept its home's marker, at a turn starting there, so it can follow the home
// from then on.
func stampLinkedHome(ctx context.Context, state *SessionState, root string) {
	if state.WorktreeID == "" || state.WorktreeIncarnation != "" || !homedAt(state, root) {
		return
	}
	if worktreeID, err := paths.GetWorktreeID(root); err != nil || worktreeID != state.WorktreeID {
		return
	}
	marker, err := MarkWorktree(ctx, root, state.WorktreeID)
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "session"), "could not mark the session's home worktree",
			slog.String("session_id", state.SessionID),
			slog.String("error", err.Error()))
		return
	}
	state.WorktreeIncarnation = marker
}

// HomeReplaced reports whether the linked worktree now at state's recorded
// home path is known not to be its home: git removed the home and added
// another worktree there. Unknown (no marker, a failed check) is false.
func HomeReplaced(ctx context.Context, state *SessionState) bool {
	return homeAt(ctx, state, state.WorktreePath) == homeReplaced
}
