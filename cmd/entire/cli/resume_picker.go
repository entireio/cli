package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"charm.land/huh/v2"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
)

// resumePickerCancel is the sentinel option value for the picker's Cancel entry.
const (
	resumePickerCancel = "cancel"
	unknownAgentLabel  = "(unknown agent)"
)

// resumableSession pairs a session with the branch and committed checkpoint we
// resolved for it. A session is only resumable when BOTH are known: the branch
// is where its code lives, and checkpointID identifies the exact session to
// restore (so two sessions on the same branch resume independently). Entries
// missing either are shown but cannot be selected.
type resumableSession struct {
	state        *strategy.SessionState
	branch       string
	checkpointID id.CheckpointID
}

// isResumable reports whether this entry can actually be resumed: we need a
// branch to switch to and a committed checkpoint identifying the session.
func (r resumableSession) isResumable() bool {
	return r.branch != "" && !r.checkpointID.IsEmpty()
}

// unresumableReason explains why a non-resumable entry can't be picked.
func (r resumableSession) unresumableReason() string {
	if r.branch == "" {
		return "no branch"
	}
	return "no committed checkpoint"
}

// runResumePicker lists stopped sessions across all worktrees and lets the user
// pick one to resume. Selecting a session checks out its branch (or, when the
// branch is already checked out in another worktree, points there), restores its
// checkpoint session log, and offers to start the agent.
func runResumePicker(ctx context.Context, cmd *cobra.Command, force bool) error {
	w := cmd.OutOrStdout()

	// The picker is interactive. Without a usable terminal (CI, piped, agent
	// subprocess) the form can't render — bail with guidance instead of hanging
	// or erroring on /dev/tty, matching `entire attach`.
	if !interactive.CanPromptInteractively() {
		fmt.Fprintln(w, "The resume picker needs an interactive terminal.")
		fmt.Fprintln(w, "Pass a branch instead, e.g. 'entire session resume <branch>'.")
		return nil
	}

	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	resumable := filterResumableSessions(states)
	if len(resumable) == 0 {
		fmt.Fprintln(w, "No resumable sessions found.")
		fmt.Fprintln(w, "Tip: pass a branch to resume directly, e.g. 'entire session resume <branch>'.")
		return nil
	}

	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}
	items := resolveResumableBranches(repo, resumable)
	_ = repo.Close()

	options, hasSelectable := buildResumeOptions(items)
	if !hasSelectable {
		fmt.Fprintln(w, "Found session(s) but none can be resumed (no branch or no committed checkpoint).")
		fmt.Fprintln(w, "Pass a branch directly to resume, e.g. 'entire session resume <branch>'.")
		return nil
	}

	var selected string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Resume a session").
				Description("Checks out the branch, restores the session log, and offers to start the agent.\n" +
					"Lists sessions from this machine — to resume a branch from origin, run: entire resume <branch>").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		// User cancelled (esc/Ctrl+C) or the context was cancelled — exit cleanly
		// without a noisy error.
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("selection failed: %w", err)
	}

	if selected == resumePickerCancel || selected == "" {
		fmt.Fprintln(w, "Resume cancelled.")
		return nil
	}

	idx, convErr := strconv.Atoi(selected)
	if convErr != nil || idx < 0 || idx >= len(items) {
		return fmt.Errorf("invalid selection %q", selected)
	}
	chosen := items[idx]

	if !chosen.isResumable() {
		fmt.Fprintf(w, "Session %s can't be resumed: %s.\n", chosen.state.SessionID, chosen.unresumableReason())
		return nil
	}

	// If the branch is already checked out in another worktree, git won't allow
	// a second checkout here. Point the user at that worktree and tell them to
	// re-run the picker there — that preserves the selected-session flow (the
	// picker resumes the exact session by its checkpoint), whereas suggesting
	// `entire resume <branch>` would resume the branch's latest checkpoint and
	// pick the wrong session when several share the branch.
	if otherPath, ok := branchCheckedOutElsewhere(ctx, chosen.branch); ok {
		fmt.Fprint(w, worktreeClashMessage(chosen.branch, otherPath, chosen.state.LastPrompt))
		return nil
	}

	// Resume the specific selected session by its checkpoint, not whatever is
	// latest on the branch — otherwise two sessions on one branch would collide.
	return resumeSessionOnBranch(ctx, cmd, chosen.branch, chosen.checkpointID, force)
}

// filterResumableSessions returns sessions you can pick up later — anything not
// currently mid-turn — sorted most-recently-active first. This deliberately
// includes idle sessions (the common "exited the agent / walked away" case),
// not just sessions explicitly ended via `session stop`; only PhaseActive
// sessions (a turn is running right now) are excluded.
func filterResumableSessions(states []*strategy.SessionState) []*strategy.SessionState {
	var resumable []*strategy.SessionState
	for _, s := range states {
		if s == nil {
			continue
		}
		if s.Phase == session.PhaseActive {
			continue
		}
		resumable = append(resumable, s)
	}
	sort.SliceStable(resumable, func(i, j int) bool {
		return sessionLastActiveTime(resumable[i]).After(sessionLastActiveTime(resumable[j]))
	})
	return resumable
}

// sessionLastActiveTime returns the best timestamp to represent when a session
// was last touched: its end time, else last interaction, else start time.
func sessionLastActiveTime(s *strategy.SessionState) time.Time {
	if s.EndedAt != nil {
		return *s.EndedAt
	}
	if s.LastInteractionTime != nil {
		return *s.LastInteractionTime
	}
	return s.StartedAt
}

// resolveResumableBranches maps each stopped session to a branch, using the
// stored branch when it still exists locally and falling back to deriving it
// from committed checkpoint trailers.
func resolveResumableBranches(repo *git.Repository, stopped []*strategy.SessionState) []resumableSession {
	items := make([]resumableSession, 0, len(stopped))
	// The checkpoint→branch index is only needed for the derivation fallback, and
	// building it scans local branches. Build it lazily, and only once: sessions
	// that carry a stored branch (the common case going forward) skip it entirely.
	var index map[string]string
	for _, s := range stopped {
		// checkpointID identifies the exact session to restore; it's required for
		// the entry to be resumable (a session that never committed has none).
		cpID := s.LastCheckpointID
		if s.Branch != "" && branchExistsLocally(repo, s.Branch) {
			items = append(items, resumableSession{state: s, branch: s.Branch, checkpointID: cpID})
			continue
		}
		if index == nil {
			index = buildCheckpointBranchIndex(repo)
		}
		items = append(items, resumableSession{state: s, branch: resolveSessionBranch(repo, s, index), checkpointID: cpID})
	}
	return items
}

// resolveSessionBranch determines the branch for a session. The branch recorded
// on the session wins when it still exists; otherwise the session is matched to
// a branch via its last checkpoint ID (which appears in that branch's commit
// trailers). Returns "" when neither resolves.
func resolveSessionBranch(repo *git.Repository, s *strategy.SessionState, index map[string]string) string {
	if s.Branch != "" && branchExistsLocally(repo, s.Branch) {
		return s.Branch
	}
	if !s.LastCheckpointID.IsEmpty() {
		if b, ok := index[s.LastCheckpointID.String()]; ok {
			return b
		}
	}
	return ""
}

// branchExistsLocally reports whether a local branch of the given name exists.
func branchExistsLocally(repo *git.Repository, name string) bool {
	_, err := repo.Reference(plumbing.NewBranchReferenceName(name), true)
	return err == nil
}

// Scan depths for buildCheckpointBranchIndex. The default branch is a single
// branch, so it can be walked deeper; feature branches are bounded tighter since
// there may be hundreds of them.
const (
	defaultBranchScanDepth = 500
	featureBranchScanDepth = 50
)

// buildCheckpointBranchIndex maps committed checkpoint IDs to the branch whose
// history carries them, for the legacy fallback that resolves a session with no
// stored Branch. The default branch is indexed FIRST (so a checkpoint committed
// to main/master maps to the default branch, not to some feature branch that
// merely contains the commit), and its commit hashes seed a stop-set: feature
// branch walks halt when they reach shared default history, so they only claim
// their own branch-only checkpoints. First branch to claim a checkpoint wins.
//
// This deliberately avoids go-git's MergeBase (which walks full history and,
// run once per branch, becomes O(branches × history) and hangs on large repos);
// the precomputed default-commit set is a cheap stand-in for branch-only scoping.
// Internal entire/ refs (checkpoint metadata + shadow branches) are never
// indexed — they are not resumable and number in the hundreds.
func buildCheckpointBranchIndex(repo *git.Repository) map[string]string {
	index := map[string]string{}

	defaultBranch := resolveDefaultBranchName(repo)

	// Seed with the default branch's checkpoints and record its commit hashes so
	// feature walks can stop at shared history.
	defaultCommits := map[plumbing.Hash]bool{}
	if defaultBranch != "" {
		if c := resolveBranchCommit(repo, defaultBranch); c != nil {
			indexBranchCheckpoints(c, defaultBranch, defaultBranchScanDepth, nil, defaultCommits, index)
		}
	}

	iter, err := repo.Branches()
	if err != nil {
		return index
	}
	defer iter.Close()
	forEachErr := iter.ForEach(func(ref *plumbing.Reference) error {
		branchName := ref.Name().Short()
		if branchName == defaultBranch || strings.HasPrefix(branchName, "entire/") {
			return nil
		}
		headCommit, err := repo.CommitObject(ref.Hash())
		if err != nil {
			return nil //nolint:nilerr // skip unreadable branch, keep indexing others
		}
		indexBranchCheckpoints(headCommit, branchName, featureBranchScanDepth, defaultCommits, nil, index)
		return nil
	})
	if forEachErr != nil {
		return index
	}

	return index
}

// resolveDefaultBranchName returns the repo's default branch name (from origin's
// HEAD when available, else the first of main/master that exists locally), or ""
// if none can be determined.
func resolveDefaultBranchName(repo *git.Repository) string {
	if name := getDefaultBranchFromRemote(repo); name != "" {
		return name
	}
	for _, name := range []string{defaultBaseBranch, masterBaseBranch} {
		if _, err := repo.Reference(plumbing.NewBranchReferenceName(name), true); err == nil {
			return name
		}
	}
	return ""
}

// resolveBranchCommit returns the commit a branch points at, preferring the local
// ref and falling back to origin's remote-tracking ref (the default branch may
// not be checked out locally in a worktree-only setup).
func resolveBranchCommit(repo *git.Repository, name string) *object.Commit {
	for _, ref := range []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName(name),
		plumbing.NewRemoteReferenceName("origin", name),
	} {
		if r, err := repo.Reference(ref, true); err == nil {
			if c, err := repo.CommitObject(r.Hash()); err == nil {
				return c
			}
		}
	}
	return nil
}

// indexBranchCheckpoints walks history from start back maxCommits commits,
// recording each checkpoint trailer under branch (first writer wins). It stops
// when it reaches a commit in stopAt (shared default history). When recordVisited
// is non-nil, every visited commit hash is added to it (used while seeding the
// default branch so feature walks can later stop at those commits).
func indexBranchCheckpoints(
	start *object.Commit,
	branch string,
	maxCommits int,
	stopAt map[plumbing.Hash]bool,
	recordVisited map[plumbing.Hash]bool,
	index map[string]string,
) {
	current := start
	for i := 0; current != nil && i < maxCommits; i++ {
		if stopAt[current.Hash] {
			return
		}
		if recordVisited != nil {
			recordVisited[current.Hash] = true
		}
		for _, cpID := range trailers.ParseAllCheckpoints(current.Message) {
			key := cpID.String()
			if _, ok := index[key]; !ok {
				index[key] = branch
			}
		}
		if current.NumParents() == 0 {
			return
		}
		parent, err := current.Parent(0)
		if err != nil {
			return
		}
		current = parent
	}
}

// buildResumeOptions builds the picker options (one per session, keyed by index,
// plus Cancel) and reports whether at least one entry is selectable.
func buildResumeOptions(items []resumableSession) ([]huh.Option[string], bool) {
	options := make([]huh.Option[string], 0, len(items)+1)
	hasSelectable := false
	for i, item := range items {
		options = append(options, huh.NewOption(resumeOptionLabel(item), strconv.Itoa(i)))
		if item.isResumable() {
			hasSelectable = true
		}
	}
	options = append(options, huh.NewOption("Cancel", resumePickerCancel))
	return options, hasSelectable
}

// resumeOptionLabel renders a single picker row for a stopped session.
func resumeOptionLabel(item resumableSession) string {
	s := item.state

	agentLabel := string(s.AgentType)
	if agentLabel == "" {
		agentLabel = unknownAgentLabel
	}

	prompt := strings.TrimSpace(s.LastPrompt)
	if prompt == "" {
		prompt = "(no prompt recorded)"
	} else {
		prompt = stringutil.TruncateRunes(stringutil.CollapseWhitespace(prompt), 50, "...")
	}

	when := timeAgo(sessionLastActiveTime(s))

	if !item.isResumable() {
		return fmt.Sprintf("(%s) · \"%s\" · %s · last active %s — can't resume", item.unresumableReason(), prompt, agentLabel, when)
	}
	return fmt.Sprintf("%s · \"%s\" · %s · last active %s", item.branch, prompt, agentLabel, when)
}

// shellQuote wraps a string in single quotes for safe inclusion in a copy-paste
// /bin/sh command, escaping any embedded single quotes. Prevents shell
// metacharacters in paths (or other interpolated values) from being executed.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// worktreeClashMessage builds the guidance shown when the chosen session's branch
// is already checked out in another worktree. It steers the user to re-run the
// picker in that worktree (which resumes the exact selected session by its
// checkpoint) rather than `entire resume <branch>` (which would resume the
// branch's latest checkpoint and pick the wrong session when several share it).
// The only value placed in the copy-paste command is the worktree path, and it
// is shell-quoted; the branch name appears only in non-executable prose.
func worktreeClashMessage(branch, otherPath, lastPrompt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Branch %q is already checked out in another worktree:\n", branch)
	fmt.Fprintf(&b, "  %s\n", otherPath)
	if prompt := strings.TrimSpace(lastPrompt); prompt != "" {
		fmt.Fprintf(&b, "\nResume this session (%q) there by running the picker in that worktree:\n",
			stringutil.TruncateRunes(stringutil.CollapseWhitespace(prompt), 50, "..."))
	} else {
		b.WriteString("\nResume this session there by running the picker in that worktree:\n")
	}
	fmt.Fprintf(&b, "  cd %s && entire session resume\n", shellQuote(otherPath))
	return b.String()
}

// branchCheckedOutElsewhere reports whether branch is checked out in a worktree
// other than the current one, returning that worktree's path.
func branchCheckedOutElsewhere(ctx context.Context, branch string) (string, bool) {
	rawRoot, rootErr := paths.WorktreeRoot(ctx)
	if rootErr != nil || rawRoot == "" {
		// Can't determine the current worktree, so we can't reliably tell whether
		// a branch is checked out *elsewhere*. Don't report a clash (which would
		// falsely flag the current checkout); let the normal checkout proceed and
		// surface any real conflict via git.
		return "", false
	}
	currentRoot := normalizeWorktreePath(rawRoot)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	gitCmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	gitCmd.Dir = rawRoot
	out, err := gitCmd.Output()
	if err != nil {
		return "", false
	}

	return parseWorktreeForBranch(string(out), branch, currentRoot)
}

// parseWorktreeForBranch scans `git worktree list --porcelain` output and returns
// the path of a worktree (other than currentRoot) that has branch checked out.
//
// Each worktree is a block beginning with a `worktree <path>` line and separated
// by a blank line; a `branch <ref>` line only appears for non-detached worktrees.
// curPath is reset at each block boundary and a branch line is only considered
// when a worktree line was seen in the same block, so a detached worktree (no
// branch line) can never pair a branch with a stale path or return an empty one.
func parseWorktreeForBranch(porcelain, branch, currentRoot string) (string, bool) {
	var curPath string
	for _, line := range strings.Split(porcelain, "\n") {
		switch {
		case line == "":
			curPath = "" // block boundary
		case strings.HasPrefix(line, "worktree "):
			curPath = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch ") && curPath != "":
			name := strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
			if name == branch && normalizeWorktreePath(curPath) != currentRoot {
				return curPath, true
			}
		}
	}
	return "", false
}
