package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Agent commits in another repository: a session homed in repository A whose
// agent commits in repository B. B has no state for the session, so B's hooks
// find it through the per-user session-home index (session/home_index.go),
// which turn start writes. See "Agent commits in another repository" in
// docs/architecture/sessions-and-checkpoints.md.

// foreignLockWait bounds the wait for A's state lock. The agent is inside its
// own commit, so A's lock is normally free; this covers parallel subagents'
// hooks without letting a commit in B hang. Giving up leaves the trailer
// without a checkpoint (logged). A var so tests can shorten it.
var foreignLockWait = 30 * time.Second

// foreignLinkFile carries a foreign session's link from prepare-commit-msg to
// post-commit, in the per-worktree git dir like inheritedTrailersFile.
const foreignLinkFile = "entire-foreign-link.json"

// foreignLink is the checkpoint prepare-commit-msg stamped and the foreign
// session it stamped it for.
type foreignLink struct {
	CheckpointID id.CheckpointID `json:"checkpoint_id"`
	SessionID    string          `json:"session_id"`
	WorktreePath string          `json:"worktree_path"`
}

func sessionHomeOf(state *SessionState) session.SessionHome {
	return session.SessionHome{SessionID: state.SessionID, WorktreePath: state.WorktreePath, Owner: state.Owner}
}

// recordSessionHome writes the session's index record after turn start saved
// its state. Best-effort: a missing record only means a commit in another
// repository does not link.
func recordSessionHome(ctx context.Context, h session.SessionHome) {
	if h.SessionID == "" {
		return
	}
	if err := session.RecordSessionHome(h); err != nil {
		logging.Debug(logging.WithComponent(ctx, "hooks"), "failed to record session home",
			slog.String("session_id", h.SessionID),
			slog.String("error", err.Error()))
	}
}

// foreignSession is a session's state loaded from its home repository's store.
type foreignSession struct {
	state     *SessionState
	store     *session.StateStore
	commonDir string
}

// openForeignStore opens the session state store of the repository that
// worktreePath belongs to. Git exports GIT_DIR and friends to hooks, so the
// lookup scrubs them; otherwise it would answer for this repository.
func openForeignStore(ctx context.Context, worktreePath string) (*session.StateStore, string, error) {
	commonDir, err := gitdir.CommonDirForForeignWorktree(ctx, worktreePath)
	if err != nil {
		return nil, "", err //nolint:wrapcheck // logged by callers
	}
	store, err := session.NewStateStoreForCommonDir(commonDir)
	if err != nil {
		return nil, "", err //nolint:wrapcheck // logged by callers
	}
	return store, commonDir, nil
}

// loadForeignState loads sessionID from store without changing the store, and
// refuses a state that cannot be a foreign caller.
func loadForeignState(ctx context.Context, store *session.StateStore, sessionID string) *SessionState {
	state, err := store.LoadReadOnly(ctx, sessionID)
	if err != nil || state == nil || !canBeForeignCaller(state, sessionID) {
		return nil
	}
	return state
}

func canBeForeignCaller(state *SessionState, sessionID string) bool {
	return state.SessionID == sessionID && !state.Kind.IsImported() &&
		state.AdoptedIntoWorktreePath == "" && state.Phase != session.PhaseEnded
}

// foreignIneligibility returns why state may not link this commit, or "" when
// it may: the same gates the local fast path uses for an agent commit.
func foreignIneligibility(state *SessionState) string {
	switch {
	case !state.Phase.IsActive() && !idleWithLiveTaskRecord(state, time.Now()):
		return "session is not mid-turn"
	case sessionLacksCondensableContent(state):
		return "session has no condensable content"
	case state.PendingCondensationID() != id.EmptyCheckpointID:
		return "session has a pending condensation"
	}
	return ""
}

// resolveForeignCallerSession identifies a session homed in another repository
// as the one running this process. Identification is resolveCallerIdentity
// over this repository's sessions plus the indexed foreign ones, so a nearer
// local agent wins over a passed-through foreign variable, as it would locally.
// Most commits match no record and cost one directory read.
func resolveForeignCallerSession(ctx context.Context, localStates []*SessionState) (*foreignSession, bool) {
	logCtx := logging.WithComponent(ctx, "hooks")
	homes, err := session.ListSessionHomes(activeSessionInteractionThreshold)
	if err != nil {
		logging.Debug(logCtx, "foreign session: cannot list session homes", slog.String("error", err.Error()))
		return nil, false
	}
	homes = homesNamingCaller(homes)
	if len(homes) == 0 {
		return nil, false
	}
	localCommonDir, err := gitdir.CommonDir(ctx)
	if err != nil {
		return nil, false
	}

	candidates := make(map[string]*foreignSession, len(homes))
	states := slices.Clone(localStates)
	for _, home := range homes {
		if slices.ContainsFunc(localStates, func(s *SessionState) bool { return s.SessionID == home.SessionID }) {
			continue // the local path owns it
		}
		store, commonDir, err := openForeignStore(ctx, home.WorktreePath)
		if err != nil || sameDir(commonDir, localCommonDir) {
			continue
		}
		if state := loadForeignState(ctx, store, home.SessionID); state != nil {
			candidates[home.SessionID] = &foreignSession{state: state, store: store, commonDir: commonDir}
			states = append(states, state)
		}
	}
	if len(candidates) == 0 {
		return nil, false
	}

	resolved, ok := resolveCallerIdentity(ctx, states)
	if !ok || !resolved.Resolution.IsCaller() || !resolved.Tracked {
		return nil, false
	}
	fs, ok := candidates[resolved.SessionID]
	if !ok {
		return nil, false
	}
	if reason := foreignIneligibility(fs.state); reason != "" {
		logging.Debug(logCtx, "foreign session: not eligible to link",
			slog.String("session_id", fs.state.SessionID),
			slog.String("reason", reason))
		return nil, false
	}
	return fs, true
}

// homesNamingCaller keeps the records whose session could be the caller:
// named by an agent's caller-session variable, or owned by a process in our
// ancestry. It spawns nothing.
func homesNamingCaller(homes []session.SessionHome) []session.SessionHome {
	if len(homes) == 0 {
		return nil
	}
	claimed := make(map[string]bool)
	for _, claim := range agent.CallerSessionCandidates() {
		claimed[claim.SessionID] = true
	}
	ancestry, haveAncestry := proclive.CurrentAncestry()
	var kept []session.SessionHome
	for _, h := range homes {
		if claimed[h.SessionID] || (haveAncestry && h.Owner != nil && ancestry.Depth(*h.Owner) >= 0) {
			kept = append(kept, h)
		}
	}
	return kept
}

// prepareWithoutLocalLink is prepare-commit-msg's exit when this repository's
// sessions link nothing. canLink is false when listing them failed.
func (s *ManualCommitStrategy) prepareWithoutLocalLink(ctx context.Context, commitMsgFile, source string, inherited []id.CheckpointID, local []*SessionState, canLink bool) {
	logging.Debug(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: no session content to link",
		slog.String("strategy", "manual-commit"),
		slog.String("source", source),
		slog.Int("sessions_found", len(local)),
	)
	if canLink {
		s.tryForeignAgentCommit(ctx, commitMsgFile, source, inherited, local)
	}
}

// tryForeignAgentCommit stamps a commit the committing agent's own session
// makes here while homed in another repository. It runs only after this
// repository's sessions linked nothing, and never prompts: with a person at a
// terminal it runs only under commit_linking "always". The trailer is a fresh
// checkpoint ID; the session's own reservation belongs to its home.
func (s *ManualCommitStrategy) tryForeignAgentCommit(ctx context.Context, commitMsgFile, source string, inherited []id.CheckpointID, local []*SessionState) {
	if len(inherited) > 0 {
		return // squash or redo: those link existing checkpoints
	}
	if interactive.CanPromptInteractively() {
		stngs, err := settings.Load(ctx)
		if err != nil || stngs.GetCommitLinking() != settings.CommitLinkingAlways {
			return
		}
	}
	fs, ok := resolveForeignCallerSession(ctx, local)
	if !ok {
		return
	}
	if reason := foreignLinkRefusal(ctx, fs); reason != "" {
		recordForeignNotice(ctx, fs.state.SessionID, reason)
		return
	}
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // commitMsgFile is provided by git hook
	if err != nil {
		return
	}
	message := string(content)
	rules := messageCommentRules(ctx, message)
	if _, found := stampedTrailer(ctx, message, rules, inherited); found {
		return
	}
	cpID, err := checkpoint.GenerateCheckpointID(ctx)
	if err != nil {
		return
	}
	// The marker goes first: a trailer post-commit cannot condense would dangle.
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return
	}
	data, err := json.Marshal(foreignLink{CheckpointID: cpID, SessionID: fs.state.SessionID, WorktreePath: fs.state.WorktreePath})
	if err == nil {
		err = jsonutil.WriteFileAtomicIn(root, foreignLinkFile, data, 0o600)
	}
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: could not record foreign link",
			slog.String("error", err.Error()))
		return
	}
	// Post-commit condenses only into a trailer prepare recorded as its own.
	if !recordStampedTrailer(ctx, cpID) {
		_ = osroot.RemoveNoSymlinks(root, foreignLinkFile) //nolint:errcheck // best-effort; a marker never matches another commit's fresh ID
		return
	}
	if err := os.WriteFile(commitMsgFile, []byte(addCheckpointTrailerAboveComments(message, cpID, rules)), 0o600); err != nil { //nolint:gosec // path from git hook arg
		_ = osroot.RemoveNoSymlinks(root, foreignLinkFile) //nolint:errcheck // best-effort; a marker never matches another commit's fresh ID
		return
	}
	logging.Info(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: foreign agent commit trailer added",
		slog.String("source", source),
		slog.String("checkpoint_id", cpID.String()),
		slog.String("session_id", fs.state.SessionID))
}

// foreignNoticeFile carries a notice from prepare-commit-msg to commit-msg,
// whose stderr reaches the committer (the installed prepare-commit-msg hook
// discards its own).
const foreignNoticeFile = "entire-foreign-notice.txt"

// foreignLinkRefusal returns why the foreign session must not link this
// commit, or "" when it may. Linking copies the session's transcript into this
// repository's checkpoints, which are pushed with it, so it needs all of: the
// home clone's opt-in, this repository's checkpoints going to the home
// origin's owner, and this repository's redaction covering the home's. Each
// reason is one path-free line that names no command, since the committer
// reading it may be the agent.
func foreignLinkRefusal(ctx context.Context, fs *foreignSession) string {
	home := filepath.Base(fs.state.WorktreePath)
	if !settings.LoadCrossRepoCommitsAt(fs.commonDir) {
		return fmt.Sprintf("linking commits in other repositories is off for %s; a developer can turn it on from a terminal in %s", home, home)
	}
	if reason := foreignDestinationRefusal(ctx, fs.state.WorktreePath); reason != "" {
		return reason
	}
	return foreignRedactionRefusal(ctx, fs.state.WorktreePath)
}

// foreignDestinationRefusal returns why this repository's checkpoints may not
// carry the transcript of a session homed at homeWorktree, or "". Every place
// they are pushed to (each push URL of the checkpoint sync remote, and the
// checkpoint_remote when one is in use) must be a repository of the forge
// account that owns the home's origin. Anything unresolvable refuses.
func foreignDestinationRefusal(ctx context.Context, homeWorktree string) string {
	home, ok := homeOriginRepository(ctx, homeWorktree)
	if !ok {
		return "the session's home repository has no origin on a known forge"
	}
	dests, ok := checkpointDestinations(ctx)
	if !ok {
		return "this repository's checkpoint destination is not a repository on a known forge"
	}
	for _, d := range dests {
		if d.Forge != home.Forge || d.Owner != home.Owner {
			return "this repository's checkpoints go to a different owner than the session's home repository's origin"
		}
	}
	return ""
}

// homeOriginRepository resolves the origin of the repository at dir. Hooks
// export git's repo selectors, so they are scrubbed: inherited, they would
// answer for this repository.
func homeOriginRepository(ctx context.Context, dir string) (gitremote.Repository, bool) {
	env := gitrepo.EnvWithoutRepoOverrides()
	url, err := gitremote.GetRemoteURLInDirEnv(ctx, dir, env, "origin")
	if err != nil {
		return gitremote.Repository{}, false
	}
	return gitremote.ResolveRepository(ctx, dir, env, url)
}

// checkpointDestinations returns the forge repositories this repository's
// checkpoints are pushed to; false when any of them does not resolve.
func checkpointDestinations(ctx context.Context) ([]gitremote.Repository, bool) {
	syncRemote, err := ResolveCheckpointSyncRemote(ctx)
	if err != nil || syncRemote.Name == "" {
		return nil, false
	}
	pushed, ok := remotePushForgeRepository(ctx, syncRemote.Name)
	if !ok {
		return nil, false
	}
	dests := []gitremote.Repository{pushed}
	url, enabled, err := remote.PushURL(ctx, syncRemote.Name)
	if err != nil {
		return nil, false
	}
	if enabled {
		checkpointRepo, ok := gitremote.ResolveRepository(ctx, "", nil, url)
		if !ok {
			return nil, false
		}
		dests = append(dests, checkpointRepo)
	}
	return dests, true
}

// recordForeignNotice leaves reason for commit-msg to print.
func recordForeignNotice(ctx context.Context, sessionID, reason string) {
	logging.Info(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: foreign agent commit not linked",
		slog.String("session_id", sessionID),
		slog.String("reason", reason))
	notice := fmt.Sprintf("[entire] This commit is not linked to agent session %s: %s.", sessionID, reason)
	if root, err := perWorktreeGitRoot(ctx); err == nil {
		_ = jsonutil.WriteFileAtomicIn(root, foreignNoticeFile, []byte(notice+"\n"), 0o600) //nolint:errcheck // best-effort notice
	}
}

// takeForeignNotice returns and removes the notice prepare-commit-msg left.
func takeForeignNotice(ctx context.Context) string {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return ""
	}
	data, err := osroot.ReadFileNoFollow(root, foreignNoticeFile)
	if err != nil {
		return ""
	}
	_ = osroot.RemoveNoSymlinks(root, foreignNoticeFile) //nolint:errcheck // a leftover only repeats the notice once
	return string(data)
}

// takeForeignLink consumes the marker and returns it when prepare-commit-msg
// recorded it for cpID.
func takeForeignLink(ctx context.Context, cpID id.CheckpointID) (foreignLink, bool) {
	root, err := perWorktreeGitRoot(ctx)
	if err != nil {
		return foreignLink{}, false
	}
	data, err := osroot.ReadFileNoFollow(root, foreignLinkFile)
	if err != nil {
		return foreignLink{}, false
	}
	_ = osroot.RemoveNoSymlinks(root, foreignLinkFile) //nolint:errcheck // a stale marker never matches a fresh checkpoint ID
	var l foreignLink
	if json.Unmarshal(data, &l) != nil || cpID.IsEmpty() || l.CheckpointID != cpID {
		return foreignLink{}, false
	}
	return l, true
}

// postCommitWithoutLocalSessions handles a stamped commit no session of this
// repository can claim: the trailer may be a foreign session's. listed is
// false when listing sessions failed.
func (s *ManualCommitStrategy) postCommitWithoutLocalSessions(ctx context.Context, repo *git.Repository, head *plumbing.Reference,
	commit *object.Commit, checkpointID id.CheckpointID, worktreePath string, listed, targetPreexisting bool,
) {
	emitter := newCommitCondensedEmitter(worktreePath, head.Hash().String())
	if s.linkForeignAgentCommit(ctx, repo, head, commit, checkpointID, worktreePath, nil, emitter,
		listed && !targetPreexisting && !isGitSequenceOperation(ctx)) {
		return
	}
	logging.Warn(logging.WithComponent(ctx, "checkpoint"), "post-commit: no active sessions despite trailer",
		slog.String("strategy", "manual-commit"),
		slog.String("checkpoint_id", checkpointID.String()),
	)
}

// claimTrailerForForeignSession is PostCommit's last chance for a trailer no
// session of this repository claimed: it may be a foreign session's
// (tryForeignAgentCommit). It reports whether the trailer is claimed now.
func (s *ManualCommitStrategy) claimTrailerForForeignSession(ctx context.Context, repo *git.Repository, head *plumbing.Reference,
	commit *object.Commit, cpID id.CheckpointID, worktreePath string, committed map[string]struct{},
	emitter *commitCondensedEmitter, claimed, isRebase, targetPreexisting bool,
) bool {
	if claimed {
		return true
	}
	return s.linkForeignAgentCommit(ctx, repo, head, commit, cpID, worktreePath, committed, emitter, !isRebase && !targetPreexisting)
}

// linkForeignAgentCommit condenses the foreign session prepare-commit-msg
// stamped this commit for into cpID, in this repository's store, and saves the
// session back to its home under the home's lock. committed is the commit's
// file set, or nil to compute it here. eligible is false when no foreign
// session may own this trailer (a sequence operation, an existing checkpoint);
// the marker is consumed either way. It reports whether it condensed.
//
// The home state advances as for a guest-linked commit (condenseOpts.foreignHome):
// the transcript window moves past this checkpoint, while the home's
// BaseCommit, files, turn checkpoints and LastCheckpointID stay as they were.
func (s *ManualCommitStrategy) linkForeignAgentCommit(ctx context.Context, repo *git.Repository, head *plumbing.Reference, commit *object.Commit,
	cpID id.CheckpointID, worktreePath string, committed map[string]struct{}, emitter *commitCondensedEmitter, eligible bool,
) bool {
	link, ok := takeForeignLink(ctx, cpID)
	if !ok || !eligible {
		return false
	}
	if s.condenseForeignLink(ctx, repo, head, commit, cpID, worktreePath, committed, emitter, link) {
		return true
	}
	// The trailer is already in the commit; say so rather than leave it silent.
	logging.Warn(logging.WithComponent(ctx, "checkpoint"), "post-commit: foreign agent commit trailer has no checkpoint",
		slog.String("session_id", link.SessionID),
		slog.String("checkpoint_id", cpID.String()))
	return false
}

func (s *ManualCommitStrategy) condenseForeignLink(ctx context.Context, repo *git.Repository, head *plumbing.Reference, commit *object.Commit,
	cpID id.CheckpointID, worktreePath string, committed map[string]struct{}, emitter *commitCondensedEmitter, link foreignLink,
) bool {
	store, commonDir, err := openForeignStore(ctx, link.WorktreePath)
	if err != nil {
		return false
	}
	var condensed bool
	var newSkillEvents []agent.SkillEvent
	var condensedSignal *commitCondensedSignal
	err = mutateForeignSessionState(ctx, store, commonDir, link.SessionID, func(state *SessionState) error {
		// Judged again under the home's lock: the opt-in may have been turned
		// off, or the session may have moved on, since prepare-commit-msg.
		if !settings.LoadCrossRepoCommitsAt(commonDir) || !canBeForeignCaller(state, link.SessionID) || foreignIneligibility(state) != "" {
			return ErrMutationSkip
		}
		if committed == nil {
			committed = filesChangedInCommit(ctx, worktreePath, commit, nil, nil)
		}
		// The session's own files are its home's paths; this checkpoint gets
		// the committed files its transcript wrote under this worktree.
		homeFiles := state.FilesTouched
		defer func() { state.FilesTouched = homeFiles }()
		state.FilesTouched = s.foreignEditedFiles(ctx, state, worktreePath, committed)
		condensed, newSkillEvents, condensedSignal = s.condenseAndUpdateState(ctx, repo, cpID, state, head, committed,
			condenseOpts{repoDir: worktreePath, noStoredFallback: true, foreignHome: true,
				searchProbeAllowed: func() bool { return emitter.searchProbeAllowed(ctx) }})
		if !condensed {
			return ErrMutationSkip
		}
		return nil
	}, func() {
		EmitSkillInvocationTelemetry(ctx, newSkillEvents)
		emitter.emit(ctx, condensedSignal)
	})
	if err != nil {
		logging.Warn(logging.WithComponent(ctx, "checkpoint"), "post-commit: foreign session state not updated",
			slog.String("session_id", link.SessionID),
			slog.String("error", err.Error()))
	}
	return condensed
}

// mutateForeignSessionState is MutateSessionStateOnSaved for a session homed
// in the repository whose git common dir is commonDir: its lock, read without
// cleanup, and saved to store. The lock wait is bounded by foreignLockWait,
// and a call nested in a mutation of the same session is refused.
func mutateForeignSessionState(ctx context.Context, store *session.StateStore, commonDir, sessionID string,
	fn func(*SessionState) error, onSaved func(),
) error {
	return mutateSessionStateIn(WithSessionLockWait(ctx, foreignLockWait), sessionID, sessionStateAccess{
		lock:      func() (stateLock, error) { return stateLockInCommonDir(commonDir, sessionID) },
		load:      store.LoadReadOnly,
		save:      store.Save,
		outerOnly: true,
	}, fn, onSaved)
}

// foreignEditedFiles returns the committed files the session's transcript
// wrote under worktreePath since its last checkpoint, sorted. When it wrote
// none of them (the agent edited through a shell, say), the agent's own
// commit is attributed whole, as filterFilesTouched does for a session with
// evidence of work. Never empty for a non-empty commit, so condensation never
// re-resolves files against the home repository.
func (s *ManualCommitStrategy) foreignEditedFiles(ctx context.Context, state *SessionState, worktreePath string, committed map[string]struct{}) []string {
	view := *state
	view.WorktreePath = worktreePath
	prepareTranscriptForState(ctx, &view)
	var out []string
	for _, f := range s.extractModifiedFilesFromLiveTranscript(ctx, &view, uncheckpointedTranscriptStart(state)) {
		if _, ok := committed[f]; ok && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return committedFilesExcludingMetadata(committed)
	}
	slices.Sort(out)
	return out
}
