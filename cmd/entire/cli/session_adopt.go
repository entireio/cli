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

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

type adoptOptions struct {
	FromWorktree string
	Force        bool
	AllowForeign bool
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
session state file to the current worktree and requires --force or --yes.

Adoption only proceeds silently for your own session. Because it moves the
named session and resets its checkpoint bookkeeping, a session Entire cannot
confirm belongs to this command is refused: you are asked to confirm at a
terminal, and without one the command fails rather than acting. Pass
--allow-foreign-session to adopt a session that is deliberately not yours.`,
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
	// Deliberately NOT folded into --force/--yes, which already carry two
	// meanings (replace local state; confirm same-store adoption). This one
	// waives a safety check on someone else's running session, so it says so
	// in its own name and cannot be granted by an agent reaching for the
	// familiar flag.
	cmd.Flags().BoolVar(&opts.AllowForeign, "allow-foreign-session", false,
		"adopt a session that is not this command's own caller (overrides the ownership check)")

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
	overridden, err := ensureAdoptSourceIsCaller(ctx, w, sourceStore, sourceState, opts)
	if err != nil {
		return err
	}

	var adopted *session.State
	var filesTouched []string
	if sameSessionStore {
		adopted, filesTouched, err = adoptFromSameSessionStore(ctx, sourceStore, sourceWorktree, sourceState, opts, overridden)
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
			overridden,
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

// ensureAdoptSourceIsCaller refuses to move a session that is not this
// command's own, which is the hazard the whole command carries: adoption
// rewrites the session's worktree and resets its checkpoint bookkeeping
// (StepCount, CheckpointTranscriptStart, LastCheckpointID, FilesTouched), so
// naming the wrong session mutates a third party's RUNNING session and
// silently detaches their next commit from it.
//
// The pre-existing checks do not cover this and cannot: sessionBelongsToSourceWorktree
// only asks whether the ID and the worktree agree with EACH OTHER, which any
// pair read out of `entire session current` satisfies by construction, and
// isAdoptableSourceSession only asks whether the session is still live —
// being live is what makes hijacking it harmful. Neither asks "is this mine".
//
// Verified adoption is silent, because it is the case the command exists for:
// an agent whose session moved to another worktree adopting its own session.
// Everything else needs a human to say yes, or the explicit flag.
func ensureAdoptSourceIsCaller(ctx context.Context, w io.Writer, sourceStore *session.StateStore, source *session.State, opts adoptOptions) (overridden bool, err error) {
	if opts.AllowForeign {
		return true, nil
	}
	// One call, both results. Asking twice — once for the verdict, once for
	// the reason — reads the mutable session store and walks the process tree
	// twice, and the second answer can differ from the first: a false-to-true
	// flip returns no reason, so a legitimate session gets refused with an
	// empty explanation.
	owned, reason := adoptSourceIsOwned(ctx, sourceStore, source)
	if owned {
		return false, nil
	}
	if err := refuseForeignAdoption(ctx, w, source, reason); err != nil {
		return false, err
	}
	// A human said yes to a session we could not prove is theirs, so from here
	// on this adoption rests on that answer rather than on ownership — which
	// is what revalidateAdoptOwnership needs to know so it does not re-ask.
	return true, nil
}

// adoptSourceIsOwned is the ownership decision with no interaction and no side
// effects, so it can be taken twice: once to decide whether to ask, and again
// under the state lock to confirm the answer still holds. Returns the reason
// when it does not, for whichever of those two callers needs to say why.
//
// The source repository's sessions are weighed as FIRST-CLASS candidates, not
// as a fallback, and adopt applies no ownership rule of its own — the shared
// resolver's policy is the whole policy. This is the fourth shape of this
// function; the previous three each authorized on something that looked like
// proof:
//
//   - The environment names the source session, so it is ours. No: an
//     environment variable says only that SOME ancestor published it, and an
//     inner agent that publishes nothing (Gemini CLI, opencode) forwards the
//     outer agent's variable verbatim.
//   - The source session's owner is somewhere in our ancestry, so it is ours.
//     No: the outer agent of a nested pair really is an ancestor, several hops
//     up.
//   - Consult the source store only when the resolver identifies nobody. No:
//     a matching inherited claim makes the resolver identify the OUTER
//     session, so the nearer inner owner in the source store is never
//     examined.
//
// Each of those was an adopt-local rule layered on the shared one, and each
// diverged from it somewhere. Asking one question over every candidate at once
// removes the divergence rather than correcting it again: adoption is
// authorized exactly when the session being adopted IS the caller the shared
// resolver identifies.
//
// That inherits the resolver's acknowledged limit — a session equally near the
// winner and invisible to the ranking. If mutation ever needs a stricter
// standard than display does, that belongs in the shared vocabulary (an
// explicit IsSafeToMutate alongside IsCaller), not in an ordering private to
// this file.
func adoptSourceIsOwned(ctx context.Context, sourceStore *session.StateStore, source *session.State) (bool, string) {
	// Listed fresh on every call, so the re-check under the state lock sees
	// sessions created since the first decision.
	sourceStates, err := sourceStore.List(ctx)
	if err != nil {
		return false, fmt.Sprintf("the source repository's session store could not be read, so %s cannot be confirmed as yours",
			shortSessionID(source.SessionID))
	}

	caller, ok := strategy.IdentifyCallerSession(ctx, sourceStates)
	switch {
	case !ok:
		// Nothing claimed this command and no session's owner places us, in
		// either repository. Every cross-machine --from lands here too, since
		// ancestry cannot speak to another host.
		return false, fmt.Sprintf("this command's own session could not be identified, so %s cannot be confirmed as yours",
			shortSessionID(source.SessionID))

	case caller.Resolution == strategy.ResolutionCallerAmbiguous:
		return false, "several agent sessions claim this command and none could be ordered, so none of them can be confirmed as its caller"

	case caller.SessionID == source.SessionID:
		return true, ""

	default:
		return false, fmt.Sprintf("this command is running inside session %s, not %s",
			shortSessionID(caller.SessionID), shortSessionID(source.SessionID))
	}
}

// revalidateAdoptOwnership re-takes the ownership decision against the state
// as it exists UNDER THE LOCK, immediately before the mutation.
//
// The first decision was made on an unlocked snapshot, and both mutation paths
// already re-check every other precondition after reloading — adoptability,
// worktree membership, transcript ownership — because the window between
// reading and locking is real. Ownership was the one precondition left outside
// that pattern, and it is not stable across the window either: a turn start
// re-records SessionState.Owner (captureSessionOwner), and a session created
// in the source store meanwhile can be a NEARER owner than the one this
// adoption was authorized against.
//
// It fails rather than asks. An override — the flag, or a human who already
// answered — is honoured without re-checking, because that answer was about
// this session and re-prompting for it would be asking the same person the
// same question twice. Everything else must still be provable, and if it is
// not, the caller retries rather than being walked through a second dialogue
// mid-mutation.
func revalidateAdoptOwnership(ctx context.Context, sourceStore *session.StateStore, locked *session.State, overridden bool) error {
	if overridden {
		return nil
	}
	owned, reason := adoptSourceIsOwned(ctx, sourceStore, locked)
	if owned {
		return nil
	}
	return fmt.Errorf(
		"session %s could no longer be confirmed as this command's caller once its state was locked (%s); rerun, or pass --allow-foreign-session if adopting it is intended",
		shortSessionID(locked.SessionID), reason)
}

// refuseForeignAdoption asks a human, or fails when there is none to ask.
func refuseForeignAdoption(ctx context.Context, w io.Writer, source *session.State, reason string) error {
	confirmed, err := confirmAdoptForeignSession(ctx, w, reason, source)
	if err != nil {
		return err
	}
	if !confirmed {
		return NewSilentError(errors.New("adoption cancelled"))
	}
	return nil
}

// confirmAdoptForeignSession asks before moving a session we could not confirm
// is ours, and refuses outright when nobody can be asked.
//
// Failing closed without a terminal is the point rather than an inconvenience:
// the reported incident was an AGENT running this non-interactively on an ID it
// had read out of `session current`, so a prompt nobody sees must not be
// treated as consent. A human gets the question; a script states its intent
// with the flag.
func confirmAdoptForeignSession(ctx context.Context, w io.Writer, reason string, source *session.State) (bool, error) {
	fmt.Fprintf(w, "Session %s is recorded in %s and is still active.\n",
		shortSessionID(source.SessionID), adoptSessionWorktreeLabel(source))
	fmt.Fprintf(w, "Cannot confirm it is yours: %s.\n", reason)
	fmt.Fprintln(w, "Adopting it moves that session here and resets its checkpoint bookkeeping.")

	if !interactive.CanPromptInteractively() {
		// The remedy has to be PRINTED, not just carried on the error. This
		// returns a SilentError, and main.go's SilentError branch prints
		// nothing — it assumes the command already spoke. Leaving the flag
		// name only inside the error meant the one audience this branch exists
		// for, a non-interactive caller, never saw it in any output.
		fmt.Fprintln(w, "There is no terminal to confirm on, so the adoption was refused.")
		fmt.Fprintln(w, "Pass --allow-foreign-session if adopting a session that is not this command's own is intended.")
		return false, NewSilentError(fmt.Errorf("refusing to adopt session %s: %s",
			shortSessionID(source.SessionID), reason))
	}

	confirmed := false
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewConfirm().
			Title(fmt.Sprintf("Adopt session %s anyway?", shortSessionID(source.SessionID))).
			Value(&confirmed),
	))
	if err := form.RunWithContext(ctx); err != nil {
		// Route aborts through the shared classifier, like every other huh
		// prompt in this package, so Ctrl-C prints "Adoption cancelled."
		// rather than a raw "confirm: user aborted".
		//
		// An abort returns (false, nil) so it behaves exactly like answering
		// "no": the caller refuses. An interrupted prompt must never be the
		// path by which a mutation proceeds.
		if failed := handleFormCancellation(w, "Adoption", err); failed != nil {
			return false, failed
		}
		return false, nil
	}
	return confirmed, nil
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
	overridden bool,
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
		if err := revalidateAdoptOwnership(ctx, sourceStore, sourceState, overridden); err != nil {
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

func adoptFromSameSessionStore(ctx context.Context, sourceStore *session.StateStore, sourceWorktree string, sourceState *session.State, opts adoptOptions, overridden bool) (*session.State, []string, error) {
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
		if err := revalidateAdoptOwnership(ctx, sourceStore, current, overridden); err != nil {
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
	adopted.ClearCondensationAttempt()
	adopted.LastCheckpointCommitHash = ""
	adopted.CheckpointTokenUsage = nil
	// Re-baseline the subagent cumulative for the fresh target-local window. The
	// cloned TokenUsage carries the SOURCE session's full cumulative subagent
	// total; without re-baselining here, the first post-adopt checkpoint would
	// subtract the source's (stale or nil) baseline and over-report — potentially
	// the source session's entire subagent usage. Mirrors resetCheckpointWindow's
	// baseline capture so the first adopted checkpoint only counts target-side
	// subagent growth, consistent with the PromptWindowBase reset below.
	adopted.RebaselineSubagentTokens()

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
	// Unbounded walk: adopt is a user-attended command, so a slow-but-healthy
	// repo should finish rather than fail at the hook-path status budget.
	changes, err := detectFileChangesUnbounded(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect current file changes: %w", err)
	}
	files := mergeUnique(nil, changes.Modified)
	files = mergeUnique(files, changes.New)
	files = mergeUnique(files, changes.Deleted)
	sort.Strings(files)
	return files, nil
}
