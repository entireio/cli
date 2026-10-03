package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/validation"
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

// validateAdoptSourceTranscript authorizes the source transcript and persists
// a canonical home/path pair when the recorded home is independently trusted.
// Task paths are validated against the original home before it is canonicalized.
func validateAdoptSourceTranscript(source *session.State, sourceWorktree string) error {
	authorizer := newAdoptPathAuthorizer(sourceWorktree)
	if source == nil || strings.TrimSpace(source.TranscriptPath) == "" {
		authorizer.validateTaskRecords(source)
		return nil
	}

	resolved, home, ok := authorizer.authorizesSessionPath(source.TranscriptPath, source.AgentHome, source.AgentType, source.SessionID)
	if !ok {
		return unexpectedAdoptTranscriptPathError(source, sourceWorktree)
	}
	authorizer.validateTaskRecords(source)
	source.TranscriptPath = resolved
	source.AgentHome = home
	return nil
}

// adoptPathAuthorizer caches positive and negative home provenance for a single
// adoption. Individual transcript paths still receive their own confinement check.
type adoptPathAuthorizer struct {
	sourceWorktree string
	homes          map[adoptHomeKey]*agent.TrustedTranscriptResolver
}

type adoptHomeKey struct {
	home      string
	agentType types.AgentType
}

func newAdoptPathAuthorizer(sourceWorktree string) *adoptPathAuthorizer {
	return &adoptPathAuthorizer{sourceWorktree: sourceWorktree, homes: make(map[adoptHomeKey]*agent.TrustedTranscriptResolver)}
}

// authorizesPath prefers an independently trusted recorded home. Legacy
// paths are authorized through the agent's current session store, and are
// upgraded to the active home only when they resolve beneath it. A legacy
// session (no recorded home) whose transcript is reached through a linked
// directory below the home, such as a relocated projects directory, keeps the
// legacy read protocol. A recorded home, or a linked transcript leaf, never
// falls back: that is the redirection confinement exists to refuse.
func (a *adoptPathAuthorizer) authorizesPath(path, agentHome string, agentType types.AgentType) (resolvedPath, resolvedHome string, ok bool) {
	if resolved, home, ok := a.recordedHomeAcceptsPath(agentHome, agentType, path); ok {
		return resolved, home, true
	}
	if strings.TrimSpace(agentHome) != "" {
		owner, err := agent.GetByAgentType(agentType)
		if err != nil {
			return "", "", false
		}
		if _, supported := agent.AsAgentHomeProvider(owner); supported {
			return "", "", false
		}
	}

	owner, ok := agent.AgentForTranscriptPath(path, a.sourceWorktree)
	if !ok {
		return "", "", false
	}
	if agentType != "" && owner.Type() != agentType {
		return "", "", false
	}
	if provider, hasHome := agent.AsAgentHomeProvider(owner); hasHome {
		if home, err := provider.SessionHome(); err == nil && provider.SessionPathUnder(home, path) {
			if resolved, canonicalHome, accepted := a.recordedHomeAcceptsPath(home, owner.Type(), path); accepted {
				return resolved, canonicalHome, true
			}
			if strings.TrimSpace(agentHome) != "" || transcriptLeafIsLink(path) {
				return "", "", false
			}
		}
	}
	return path, "", true
}

// transcriptLeafIsLink reports whether path itself is a symbolic link. A
// missing or unreadable leaf is not one; later reads report it.
func transcriptLeafIsLink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// validateTaskRecords clears unauthorized task transcript paths.
// Missing task transcripts do not prevent adopting the parent session.
func (a *adoptPathAuthorizer) validateTaskRecords(source *session.State) {
	if source == nil {
		return
	}
	for i := range source.TaskRecords {
		record := &source.TaskRecords[i]
		if record.DeclaredTranscriptPath == "" {
			continue
		}
		resolved, _, ok := a.authorizesSessionPath(record.DeclaredTranscriptPath, source.AgentHome, source.AgentType, record.AgentID)
		// Claude and Droid also name child transcripts agent-<id>.jsonl.
		if !ok && record.AgentID != "" && (source.AgentType == agent.AgentTypeClaudeCode || source.AgentType == agent.AgentTypeFactoryAIDroid) {
			resolved, _, ok = a.authorizesSessionPath(record.DeclaredTranscriptPath, source.AgentHome, source.AgentType, "agent-"+record.AgentID)
		}
		if !ok || !a.taskPathMatchesParentLayout(source, resolved) {
			record.DeclaredTranscriptPath = ""
			continue
		}
		record.DeclaredTranscriptPath = resolved
	}
}

// Project-scoped children stay beside their actual parent transcript or in its
// subagents directory. This uses transcript coordinates, never the worktree
// association that adoption changes. Flat stores keep agent-ID-based lookup.
func (a *adoptPathAuthorizer) taskPathMatchesParentLayout(source *session.State, task string) bool {
	if source.TranscriptPath == "" {
		return true
	}
	switch source.AgentType {
	case agent.AgentTypeClaudeCode, agent.AgentTypeFactoryAIDroid, agent.AgentTypePi:
		parent, _, ok := a.authorizesPath(source.TranscriptPath, source.AgentHome, source.AgentType)
		if !ok {
			return false
		}
		dir := filepath.Dir(parent)
		if sameTranscriptLocation(filepath.Dir(task), dir) {
			return true
		}
		return source.AgentType != agent.AgentTypePi &&
			sameTranscriptLocation(filepath.Dir(task), paths.SubagentsDir(dir, source.SessionID))
	default:
		return true
	}
}

// recordedHomeAcceptsPath applies the shared home authorization and read policy.
func (a *adoptPathAuthorizer) recordedHomeAcceptsPath(agentHome string, agentType types.AgentType, path string) (resolvedPath, resolvedHome string, ok bool) {
	key := adoptHomeKey{home: strings.TrimSpace(agentHome), agentType: agentType}
	resolver, cached := a.homes[key]
	if !cached {
		owner, err := agent.GetByAgentType(agentType)
		if err == nil {
			if provider, supported := agent.AsAgentHomeProvider(owner); supported {
				resolver, err = agent.NewTrustedTranscriptResolver(provider, key.home)
				if err != nil {
					resolver = nil
				}
			}
		}
		a.homes[key] = resolver
	}
	if resolver == nil {
		return "", "", false
	}
	resolvedPath, resolvedHome, err := resolver.Resolve(path)
	return resolvedPath, resolvedHome, err == nil
}

// authorizesSessionPath checks the agent's session naming in addition to home
// provenance and confinement. Adoption intentionally preserves transcripts
// from an earlier project; the current worktree is not their owner.
func (a *adoptPathAuthorizer) authorizesSessionPath(path, home string, kind types.AgentType, id string) (string, string, bool) {
	if validation.ValidateSessionID(id) != nil {
		return "", "", false
	}
	resolved, canonicalHome, ok := a.authorizesPath(path, home, kind)
	if !ok {
		return "", "", false
	}
	owner, err := agent.GetByAgentType(kind)
	if err != nil {
		return "", "", false
	}
	var store *agent.SessionStore
	if canonicalHome != "" {
		store, err = agent.OpenSessionStoreAt(owner, canonicalHome)
	} else {
		store, err = agent.OpenSessionStore(owner, a.sourceWorktree)
	}
	if err != nil {
		return "", "", false
	}
	if _, err := store.Name(resolved); err != nil {
		return "", "", false
	}
	if matcher, ok := owner.(agent.SessionFileNameMatcher); ok {
		return resolved, canonicalHome, matcher.SessionFileNameMatches(filepath.Base(resolved), id)
	}
	// Most agents resolve within the transcript's directory. Nested layouts
	// such as Copilot's <id>/events.jsonl resolve from its parent instead.
	dir := filepath.Dir(resolved)
	for range 2 {
		candidates, candidateErr := store.SessionFileCandidatesIn(dir, id)
		if candidateErr == nil {
			for _, candidate := range candidates {
				if _, nameErr := store.Name(candidate); nameErr == nil && sameTranscriptLocation(candidate, resolved) {
					return resolved, canonicalHome, true
				}
			}
		}
		dir = filepath.Dir(dir)
	}
	return "", "", false
}

func sameTranscriptLocation(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || (runtime.GOOS == windowsGOOS && strings.EqualFold(a, b))
}

// unexpectedAdoptTranscriptPathError identifies the rejected path and relocation
// variables that may select the correct active home.
func unexpectedAdoptTranscriptPathError(source *session.State, sourceWorktree string) error {
	msg := fmt.Sprintf("unexpected transcript path for session %s: %s is not owned by a registered agent for %s",
		source.SessionID, source.TranscriptPath, sourceWorktree)
	if home := strings.TrimSpace(source.AgentHome); home != "" {
		msg += " or recognized under its recorded agent home " + home
	}
	msg += "; if the source session ran with a relocated agent home, check one of: " + strings.Join(agent.RelocationEnvVars(), ", ")
	return errors.New(msg)
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
	// TranscriptPath and AgentHome carry the source validation's canonical
	// coordinates; checkpoint bookkeeping below remains local to the target.
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
