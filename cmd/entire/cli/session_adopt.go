package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

type adoptOptions struct {
	FromWorktree string
	Force        bool
}

const adoptRecentWindow = 12 * time.Hour

func newAdoptCmd() *cobra.Command {
	var opts adoptOptions

	cmd := &cobra.Command{
		Use:   "adopt [session-id]",
		Short: "Adopt an active session from another worktree",
		Long: `Adopt an active session from another worktree into the current repository.

This is useful when an agent starts in one repository or worktree, then moves
and makes changes in another. Adoption moves the live session state into the
current repo and seeds it with the current repo's uncommitted file changes so
the next commit can be linked normally.

When the source and target share a Git session store, adoption moves the same
session state file to the current worktree and requires --force or --yes.`,
		Example: `  entire session adopt 019ed5fe-ec49-7a72-89fd-f38e323f5448 --from ../cli
  entire session adopt --from /path/to/source/worktree
  entire session adopt --from ../source-worktree --yes`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := ""
			if len(args) > 0 {
				sessionID = args[0]
			}
			return runAdopt(cmd.Context(), cmd.OutOrStdout(), sessionID, opts)
		},
	}

	cmd.Flags().StringVar(&opts.FromWorktree, "from", "", "source worktree that already tracks the session")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "replace an existing local state file for the same session")
	cmd.Flags().BoolVar(&opts.Force, "yes", false, "confirm same-store adoption and replacement without prompting")

	return cmd
}

func runAdopt(ctx context.Context, w io.Writer, sessionID string, opts adoptOptions) error {
	if strings.TrimSpace(opts.FromWorktree) == "" {
		return errors.New("source worktree is required; pass --from <path>")
	}

	sourceStore, sourceWorktree, sourceCommonDir, err := stateStoreForWorktree(ctx, opts.FromWorktree)
	if err != nil {
		return err
	}

	targetStore, targetWorktree, targetCommonDir, err := stateStoreForWorktree(ctx, ".")
	if err != nil {
		return fmt.Errorf("open current session store: %w", err)
	}
	sameSessionStore := sameAdoptStore(sourceCommonDir, targetCommonDir)
	if sameSessionStore && sameAdoptPath(sourceWorktree, targetWorktree) {
		return errors.New("source and target are the same worktree; no session adoption is needed")
	}

	sourceState, err := selectAdoptSourceSession(ctx, sourceStore, sourceWorktree, sessionID)
	if err != nil {
		return err
	}
	if err := validateAdoptSourceTranscript(sourceState, sourceWorktree); err != nil {
		return err
	}

	var adopted *session.State
	var filesTouched []string
	if sameSessionStore {
		adopted, filesTouched, err = adoptFromSameSessionStore(ctx, sourceWorktree, sourceState, opts)
	} else {
		adopted, filesTouched, err = adoptFromExternalSessionStore(
			ctx,
			sourceStore,
			sourceWorktree,
			sourceCommonDir,
			targetStore,
			targetCommonDir,
			sourceState.SessionID,
			opts,
		)
	}
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "Adopted session %s from %s\n", shortSessionID(adopted.SessionID), sourceWorktree)
	if len(filesTouched) == 0 {
		fmt.Fprintln(w, "No current file changes were detected, so the next commit may not link until hooks record changes.")
		return nil
	}
	fmt.Fprintf(w, "Tracking %d file(s): %s\n", len(filesTouched), strings.Join(filesTouched, ", "))
	fmt.Fprintln(w, "Review tracked files before committing; adoption attributes current changes in this repo to the adopted session.")
	return nil
}

func adoptFromExternalSessionStore(
	ctx context.Context,
	sourceStore *session.StateStore,
	sourceWorktree string,
	sourceCommonDir string,
	targetStore *session.StateStore,
	targetCommonDir string,
	sessionID string,
	opts adoptOptions,
) (*session.State, []string, error) {
	sourceWorktreeID, worktreeIDErr := paths.GetWorktreeID(sourceWorktree)
	if worktreeIDErr != nil {
		sourceWorktreeID = ""
	}

	var adopted *session.State
	var filesTouched []string
	err := strategy.WithSessionStateLocks(ctx, sessionID, []string{sourceCommonDir, targetCommonDir}, func() error {
		sourceState, err := sourceStore.Load(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("load source session state: %w", err)
		}
		if sourceState == nil {
			return fmt.Errorf("session %s was not found in %s", sessionID, sourceWorktree)
		}
		if !isAdoptableSourceSession(sourceState) {
			return fmt.Errorf("session %s is ended or fully condensed and cannot be adopted", sessionID)
		}
		if !sessionBelongsToSourceWorktree(sourceState, sourceWorktree, sourceWorktreeID) {
			return fmt.Errorf("session %s belongs to %s, not %s",
				sessionID, adoptSessionWorktreeLabel(sourceState), sourceWorktree)
		}
		if err := validateAdoptSourceTranscript(sourceState, sourceWorktree); err != nil {
			return err
		}

		next, touched, err := buildAdoptedSessionState(ctx, sourceState)
		if err != nil {
			return err
		}
		existing, err := targetStore.Load(ctx, next.SessionID)
		if err != nil {
			return fmt.Errorf("load current session state: %w", err)
		}
		if existing != nil && !opts.Force {
			return fmt.Errorf("session %s is already tracked in this repo; rerun with --force to replace it", next.SessionID)
		}
		if err := targetStore.Save(ctx, next); err != nil {
			return fmt.Errorf("save adopted session state: %w", err)
		}
		retired := retireAdoptedSourceSession(sourceState, next)
		if err := sourceStore.Save(ctx, &retired); err != nil {
			if rollbackErr := rollbackExternalAdoptTarget(ctx, targetStore, next.SessionID, existing); rollbackErr != nil {
				return fmt.Errorf("retire source session state: %w; rollback adopted target session state: %w", err, rollbackErr)
			}
			return fmt.Errorf("retire source session state: %w", err)
		}
		adopted = next
		filesTouched = touched
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("adopt external session state: %w", err)
	}
	return adopted, filesTouched, nil
}

func rollbackExternalAdoptTarget(ctx context.Context, targetStore *session.StateStore, sessionID string, previous *session.State) error {
	if previous == nil {
		if err := targetStore.Clear(ctx, sessionID); err != nil {
			return fmt.Errorf("clear adopted target session state: %w", err)
		}
		return nil
	}
	if err := targetStore.Save(ctx, previous); err != nil {
		return fmt.Errorf("restore previous target session state: %w", err)
	}
	return nil
}

func retireAdoptedSourceSession(source, target *session.State) session.State {
	now := time.Now()
	retired := cloneAdoptSourceState(source)
	retired.Phase = session.PhaseEnded
	retired.EndedAt = &now
	retired.FullyCondensed = true
	retired.Owner = nil
	retired.FilesTouched = nil
	retired.TurnID = ""
	retired.TurnCheckpointIDs = nil
	retired.AdoptedIntoWorktreePath = target.WorktreePath
	retired.AdoptedIntoWorktreeID = target.WorktreeID
	return retired
}

func adoptFromSameSessionStore(ctx context.Context, sourceWorktree string, sourceState *session.State, opts adoptOptions) (*session.State, []string, error) {
	if !opts.Force {
		return nil, nil, fmt.Errorf("session %s is already tracked in this repo; rerun with --force to replace it", sourceState.SessionID)
	}

	sourceWorktreeID, worktreeIDErr := paths.GetWorktreeID(sourceWorktree)
	if worktreeIDErr != nil {
		sourceWorktreeID = ""
	}

	var adopted *session.State
	var filesTouched []string
	err := strategy.MutateSessionState(ctx, sourceState.SessionID, func(current *strategy.SessionState) error {
		if !isAdoptableSourceSession(current) {
			return fmt.Errorf("session %s is ended or fully condensed and cannot be adopted", sourceState.SessionID)
		}
		if !sessionBelongsToSourceWorktree(current, sourceWorktree, sourceWorktreeID) {
			return fmt.Errorf("session %s belongs to %s, not %s",
				sourceState.SessionID, adoptSessionWorktreeLabel(current), sourceWorktree)
		}
		if err := validateAdoptSourceTranscript(current, sourceWorktree); err != nil {
			return err
		}

		next, touched, err := buildAdoptedSessionState(ctx, current)
		if err != nil {
			return err
		}
		*current = *next
		snapshot := cloneAdoptSourceState(next)
		adopted = &snapshot
		filesTouched = touched
		return nil
	})
	if errors.Is(err, strategy.ErrStateNotFound) {
		return nil, nil, fmt.Errorf("session %s was not found in %s", sourceState.SessionID, sourceWorktree)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("adopt same-store session state: %w", err)
	}
	return adopted, filesTouched, nil
}

func validateAdoptSourceTranscript(source *session.State, sourceWorktree string) error {
	if source == nil || strings.TrimSpace(source.TranscriptPath) == "" {
		return nil
	}

	owner, ok := agent.AgentForTranscriptPath(source.TranscriptPath, sourceWorktree)
	if !ok {
		return fmt.Errorf("unexpected transcript path for session %s: %s is not owned by a registered agent for %s",
			source.SessionID, source.TranscriptPath, sourceWorktree)
	}
	if source.AgentType != "" && owner.Type() != source.AgentType {
		return fmt.Errorf("unexpected transcript path for session %s: %s belongs to %s, but source state says %s",
			source.SessionID, source.TranscriptPath, owner.Type(), source.AgentType)
	}
	return nil
}

func stateStoreForWorktree(ctx context.Context, worktreePath string) (*session.StateStore, string, string, error) {
	absWorktree, err := filepath.Abs(worktreePath)
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve source worktree: %w", err)
	}

	cmd := exec.CommandContext(ctx, "git", "-C", absWorktree, "rev-parse", "--show-toplevel", "--git-common-dir")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, "", "", fmt.Errorf("resolve source git directory: %s: %w", msg, err)
		}
		return nil, "", "", fmt.Errorf("resolve source git directory: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return nil, "", "", fmt.Errorf("resolve source git directory: unexpected git output %q", strings.TrimSpace(string(output)))
	}
	sourceRoot := strings.TrimSpace(lines[0])
	commonDir := strings.TrimSpace(lines[1])
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(absWorktree, commonDir)
	}
	commonDir = filepath.Clean(commonDir)

	return session.NewStateStoreWithDir(filepath.Join(commonDir, session.SessionStateDirName)), sourceRoot, commonDir, nil
}

func selectAdoptSourceSession(ctx context.Context, store *session.StateStore, sourceWorktree, sessionID string) (*session.State, error) {
	sourceWorktreeID, worktreeIDErr := paths.GetWorktreeID(sourceWorktree)
	if worktreeIDErr != nil {
		sourceWorktreeID = ""
	}
	if sessionID != "" {
		sourceState, err := store.Load(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("load source session state: %w", err)
		}
		if sourceState == nil {
			return nil, fmt.Errorf("session %s was not found in %s", sessionID, sourceWorktree)
		}
		if !isAdoptableSourceSession(sourceState) {
			return nil, fmt.Errorf("session %s is ended or fully condensed and cannot be adopted", sessionID)
		}
		if !sessionBelongsToSourceWorktree(sourceState, sourceWorktree, sourceWorktreeID) {
			return nil, fmt.Errorf("session %s belongs to %s, not %s",
				sessionID, adoptSessionWorktreeLabel(sourceState), sourceWorktree)
		}
		return sourceState, nil
	}

	states, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list source sessions: %w", err)
	}
	candidates := make([]*session.State, 0, len(states))
	for _, state := range states {
		if isRecentAdoptCandidate(state) && sessionBelongsToSourceWorktree(state, sourceWorktree, sourceWorktreeID) {
			candidates = append(candidates, state)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return sessionLastSeen(candidates[i]).After(sessionLastSeen(candidates[j]))
	})

	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf("no recent active sessions found in %s", sourceWorktree)
	case 1:
		return candidates[0], nil
	default:
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.SessionID)
		}
		return nil, fmt.Errorf("multiple recent active sessions found in %s; pass one of: %s",
			sourceWorktree, strings.Join(ids, ", "))
	}
}

func sessionBelongsToSourceWorktree(state *session.State, sourceWorktree, sourceWorktreeID string) bool {
	if state == nil {
		return false
	}
	if state.WorktreeID != "" && sourceWorktreeID != "" {
		return state.WorktreeID == sourceWorktreeID
	}
	if state.WorktreePath != "" {
		return sameAdoptPath(state.WorktreePath, sourceWorktree)
	}
	return false
}

func adoptSessionWorktreeLabel(state *session.State) string {
	if state == nil {
		return unknownPlaceholder
	}
	if state.WorktreePath != "" {
		return state.WorktreePath
	}
	if state.WorktreeID != "" {
		return state.WorktreeID
	}
	return unknownPlaceholder
}

func isRecentAdoptCandidate(state *session.State) bool {
	if !isAdoptableSourceSession(state) {
		return false
	}
	lastSeen := sessionLastSeen(state)
	if lastSeen.IsZero() {
		return false
	}
	return time.Since(lastSeen) <= adoptRecentWindow
}

func isAdoptableSourceSession(state *session.State) bool {
	return state != nil &&
		state.Phase != session.PhaseEnded &&
		state.EndedAt == nil &&
		!state.FullyCondensed
}

func sessionLastSeen(state *session.State) time.Time {
	if state.LastInteractionTime != nil {
		return *state.LastInteractionTime
	}
	return state.StartedAt
}

func buildAdoptedSessionState(ctx context.Context, source *session.State) (*session.State, []string, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open current repository: %w", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve current HEAD: %w", err)
	}

	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve current worktree root: %w", err)
	}
	worktreeID, err := paths.GetWorktreeID(worktreeRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve current worktree ID: %w", err)
	}

	branch, branchErr := GetCurrentBranch(ctx)
	if branchErr != nil {
		branch = ""
	}
	filesTouched, err := currentFilesTouched(ctx)
	if err != nil {
		return nil, nil, err
	}
	untrackedFiles, err := strategy.CollectUntrackedFiles(ctx)
	if err != nil {
		untrackedFiles = nil
	}

	now := time.Now()
	adopted := cloneAdoptSourceState(source)

	// Keep the source live transcript path. In cross-repo adoption the transcript
	// belongs to the continuing agent session, not the target repository; clearing
	// or recomputing it from the target repo would drop live transcript capture.
	adopted.CLIVersion = versioninfo.Version
	adopted.TranscriptPath = source.TranscriptPath
	adopted.BaseCommit = head.Hash().String()
	adopted.RealignAttributionBase(head.Hash().String())
	adopted.WorktreePath = worktreeRoot
	adopted.WorktreeID = worktreeID
	adopted.AdoptedIntoWorktreePath = ""
	adopted.AdoptedIntoWorktreeID = ""
	adopted.Branch = branch
	adopted.LastInteractionTime = &now
	adopted.Phase = session.PhaseActive
	adopted.EndedAt = nil
	adopted.FilesTouched = filesTouched

	// Reset target-local checkpoint bookkeeping. Source checkpoint IDs can point
	// at metadata in another repository or checkpoint branch; carrying them into
	// this repo would let amend and turn-finalization paths operate on unrelated
	// checkpoints.
	adopted.StepCount = 0
	adopted.CheckpointTranscriptStart = 0
	adopted.CheckpointTranscriptSize = 0
	adopted.TranscriptIdentifierAtStart = ""
	adopted.ClearLegacyTranscriptOffsets()
	adopted.TurnID = ""
	adopted.TurnCheckpointIDs = nil
	adopted.LastCheckpointID = id.EmptyCheckpointID
	adopted.LastCheckpointCommitHash = ""
	adopted.CheckpointTokenUsage = nil

	adopted.FullyCondensed = false
	adopted.UntrackedFilesAtStart = untrackedFiles
	adopted.PromptAttributions = nil
	adopted.PendingPromptAttribution = nil
	// Preserve cumulative turn/context metrics for the continuing agent session,
	// but start the target checkpoint prompt window at the current turn count so
	// the first adopted checkpoint only counts target-side turns.
	adopted.PromptWindowBase = adopted.SessionTurnCount
	adopted.PromptWindowResetPending = false
	adopted.AttachedManually = false
	// The source process owner may already be gone; a new turn will capture the
	// current owner, and until then liveness should fall back to the timeout.
	adopted.Owner = nil

	return &adopted, filesTouched, nil
}

func cloneAdoptSourceState(source *session.State) session.State {
	adopted := *source
	adopted.EndedAt = cloneTimePtr(source.EndedAt)
	adopted.LastInteractionTime = cloneTimePtr(source.LastInteractionTime)
	adopted.ReviewSkills = slices.Clone(source.ReviewSkills)
	adopted.TurnCheckpointIDs = slices.Clone(source.TurnCheckpointIDs)
	adopted.UntrackedFilesAtStart = slices.Clone(source.UntrackedFilesAtStart)
	adopted.FilesTouched = slices.Clone(source.FilesTouched)
	adopted.TokenUsage = cloneTokenUsage(source.TokenUsage)
	adopted.SkillEvents = cloneSkillEvents(source.SkillEvents)
	adopted.PromptAttributions = clonePromptAttributions(source.PromptAttributions)
	if source.PendingPromptAttribution != nil {
		pending := clonePromptAttribution(*source.PendingPromptAttribution)
		adopted.PendingPromptAttribution = &pending
	}
	return adopted
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	cloned := *t
	return &cloned
}

func cloneTokenUsage(usage *agent.TokenUsage) *agent.TokenUsage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	cloned.SubagentTokens = cloneTokenUsage(usage.SubagentTokens)
	return &cloned
}

func cloneSkillEvents(events []agent.SkillEvent) []agent.SkillEvent {
	cloned := slices.Clone(events)
	for i := range cloned {
		if events[i].TranscriptAnchor != nil {
			anchor := *events[i].TranscriptAnchor
			anchor.EntryIDs = slices.Clone(events[i].TranscriptAnchor.EntryIDs)
			cloned[i].TranscriptAnchor = &anchor
		}
		cloned[i].Native = maps.Clone(events[i].Native)
	}
	return cloned
}

func clonePromptAttributions(attrs []session.PromptAttribution) []session.PromptAttribution {
	cloned := slices.Clone(attrs)
	for i := range cloned {
		cloned[i] = clonePromptAttribution(attrs[i])
	}
	return cloned
}

func clonePromptAttribution(attr session.PromptAttribution) session.PromptAttribution {
	attr.UserAddedPerFile = maps.Clone(attr.UserAddedPerFile)
	attr.UserRemovedPerFile = maps.Clone(attr.UserRemovedPerFile)
	return attr
}

func sameAdoptPath(a, b string) bool {
	return canonicalAdoptPath(a) == canonicalAdoptPath(b)
}

func sameAdoptStore(a, b string) bool {
	return canonicalAdoptPath(a) == canonicalAdoptPath(b)
}

func canonicalAdoptPath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path
}

func currentFilesTouched(ctx context.Context) ([]string, error) {
	changes, err := DetectFileChanges(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("detect current file changes: %w", err)
	}
	files := mergeUnique(nil, changes.Modified)
	files = mergeUnique(files, changes.New)
	files = mergeUnique(files, changes.Deleted)
	sort.Strings(files)
	return files, nil
}
