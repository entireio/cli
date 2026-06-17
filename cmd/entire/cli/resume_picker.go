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
const resumePickerCancel = "cancel"

// resumableSession pairs a stopped session with the branch we resolved for it.
// branch is empty when no local branch could be mapped to the session (e.g. it
// was stopped before any checkpoint was committed) — such entries are shown but
// cannot be selected for resume.
type resumableSession struct {
	state  *strategy.SessionState
	branch string
}

// runResumePicker lists stopped sessions across all worktrees and lets the user
// pick one to resume. Selecting a session checks out its branch (or, when the
// branch is already checked out in another worktree, points there) and prints
// the command to continue the agent.
func runResumePicker(ctx context.Context, cmd *cobra.Command, force bool) error {
	w := cmd.OutOrStdout()

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
		fmt.Fprintln(w, "Found stopped session(s) but none could be mapped to a branch to resume.")
		fmt.Fprintln(w, "They had no committed checkpoints to locate. Pass a branch directly to resume.")
		return nil
	}

	var selected string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Resume a session").
				Description("Checks out the branch and prints the command to continue the agent").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
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

	if chosen.branch == "" {
		fmt.Fprintf(w, "Session %s has no branch to resume — it has no committed checkpoints.\n", chosen.state.SessionID)
		return nil
	}

	// If the branch is already checked out in another worktree, git won't allow
	// a second checkout here — point the user at that worktree instead.
	if otherPath, ok := branchCheckedOutElsewhere(ctx, chosen.branch); ok {
		fmt.Fprintf(w, "Branch '%s' is already checked out in another worktree:\n", chosen.branch)
		fmt.Fprintf(w, "  %s\n\n", otherPath)
		fmt.Fprintln(w, "Resume it there with:")
		fmt.Fprintf(w, "  cd %q && entire resume %s\n", otherPath, chosen.branch)
		return nil
	}

	return runResume(ctx, cmd, chosen.branch, force)
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
		if s.Branch != "" && branchExistsLocally(repo, s.Branch) {
			items = append(items, resumableSession{state: s, branch: s.Branch})
			continue
		}
		if index == nil {
			index = buildCheckpointBranchIndex(repo)
		}
		items = append(items, resumableSession{state: s, branch: resolveSessionBranch(repo, s, index)})
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

// buildCheckpointBranchIndex maps committed checkpoint IDs to the local branch
// whose recent history carries them. Each non-default branch is walked from its
// tip back a bounded depth; the default branch is skipped so the index favors
// feature branches, and the first branch to claim a checkpoint wins.
//
// It deliberately does NOT compute merge bases to scope to branch-only commits:
// go-git's MergeBase walks full history and, run once per branch, turns this into
// an O(branches × history) operation that hangs on large repos. A session's last
// checkpoint sits near its branch tip, so a shallow tip walk finds it; the only
// cost of skipping merge-base scoping is that a checkpoint shared with the base
// branch may be attributed to a feature branch, which is harmless for lookups
// keyed on a specific session's checkpoint ID.
func buildCheckpointBranchIndex(repo *git.Repository) map[string]string {
	index := map[string]string{}

	defaultBranch := getDefaultBranchFromRemote(repo)
	if defaultBranch == "" {
		for _, name := range []string{defaultBaseBranch, masterBaseBranch} {
			if _, err := repo.Reference(plumbing.NewBranchReferenceName(name), true); err == nil {
				defaultBranch = name
				break
			}
		}
	}

	iter, err := repo.Branches()
	if err != nil {
		return index
	}
	forEachErr := iter.ForEach(func(ref *plumbing.Reference) error {
		branchName := ref.Name().Short()
		// Skip the default branch and Entire's own internal refs (the
		// entire/checkpoints/* metadata branches and the per-base shadow
		// branches): they are not resumable user branches, and the shadow
		// branches number in the hundreds — scanning them is wasted work and
		// could mis-attribute a checkpoint to an internal ref.
		if branchName == defaultBranch || strings.HasPrefix(branchName, "entire/") {
			return nil
		}
		headCommit, err := repo.CommitObject(ref.Hash())
		if err != nil {
			return nil //nolint:nilerr // skip unreadable branch, keep indexing others
		}
		indexBranchCheckpoints(headCommit, branchName, index)
		return nil
	})
	if forEachErr != nil {
		return index
	}

	return index
}

// indexBranchCheckpoints walks history from start back a bounded depth and
// records each checkpoint trailer it finds under branch (first writer wins).
func indexBranchCheckpoints(start *object.Commit, branch string, index map[string]string) {
	const maxCommits = 50
	current := start
	for i := 0; current != nil && i < maxCommits; i++ {
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
		if item.branch != "" {
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
		agentLabel = "(unknown agent)"
	}

	prompt := strings.TrimSpace(s.LastPrompt)
	if prompt == "" {
		prompt = "(no prompt recorded)"
	} else {
		prompt = stringutil.TruncateRunes(stringutil.CollapseWhitespace(prompt), 50, "...")
	}

	when := timeAgo(sessionLastActiveTime(s))

	if item.branch == "" {
		return fmt.Sprintf("(no branch) · \"%s\" · %s · last active %s — can't resume", prompt, agentLabel, when)
	}
	return fmt.Sprintf("%s · \"%s\" · %s · last active %s", item.branch, prompt, agentLabel, when)
}

// branchCheckedOutElsewhere reports whether branch is checked out in a worktree
// other than the current one, returning that worktree's path.
func branchCheckedOutElsewhere(ctx context.Context, branch string) (string, bool) {
	rawRoot, rootErr := paths.WorktreeRoot(ctx)
	if rootErr != nil {
		rawRoot = ""
	}
	currentRoot := normalizeWorktreePath(rawRoot)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	gitCmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	if rawRoot != "" {
		gitCmd.Dir = rawRoot
	}
	out, err := gitCmd.Output()
	if err != nil {
		return "", false
	}

	var curPath string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			curPath = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			name := strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
			if name == branch && normalizeWorktreePath(curPath) != currentRoot {
				return curPath, true
			}
		}
	}
	return "", false
}
