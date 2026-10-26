package strategy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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
	if err != nil || state == nil || state.SessionID != sessionID || state.Kind.IsImported() ||
		state.AdoptedIntoWorktreePath != "" || state.Phase == session.PhaseEnded {
		return nil
	}
	return state
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
	if notice := foreignOwnerNotice(ctx, fs); notice != "" {
		recordForeignNotice(ctx, fs.state.SessionID, notice)
		return
	}
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // commitMsgFile is provided by git hook
	if err != nil {
		return
	}
	if _, found := stampedTrailer(ctx, string(content), inherited); found {
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
	if err := os.WriteFile(commitMsgFile, []byte(addCheckpointTrailer(string(content), cpID)), 0o600); err != nil { //nolint:gosec // path from git hook arg
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

// repoOwner returns the host/owner of the origin remote of the repository at
// dir (the current one when dir is empty), and whether it has an origin at all.
func repoOwner(ctx context.Context, dir string, env []string) (owner string, hasOrigin bool) {
	url, err := gitremote.GetRemoteURLInDirEnv(ctx, dir, env, "origin")
	if err != nil {
		return "", false
	}
	info, err := gitremote.ParseURL(url)
	if err != nil || info.Owner == "" {
		return "", true
	}
	return strings.ToLower(info.CanonicalHost() + "/" + info.Owner), true
}

// foreignOwnerNotice returns why the foreign session must not link this
// commit, or "" when it may. Linking copies the session's transcript into this
// repository's checkpoints, which are pushed with it (now, or once a remote is
// added), so it is allowed only when neither repository has an origin or both
// origins have the same owner: a session in a private repository must not
// publish into someone else's.
func foreignOwnerNotice(ctx context.Context, fs *foreignSession) string {
	here, hereHasOrigin := repoOwner(ctx, "", nil)
	home, homeHasOrigin := repoOwner(ctx, fs.state.WorktreePath, gitrepo.EnvWithoutRepoOverrides())
	if !hereHasOrigin && !homeHasOrigin {
		return ""
	}
	if here != "" && here == home {
		return ""
	}
	return fmt.Sprintf("[entire] This commit is not linked to agent session %s: the session started in a repository owned by %s, "+
		"and this repository (owned by %s) would publish its transcript in its checkpoints.",
		fs.state.SessionID, ownerLabel(home, homeHasOrigin), ownerLabel(here, hereHasOrigin))
}

func ownerLabel(owner string, hasOrigin bool) string {
	switch {
	case !hasOrigin:
		return "no one (no origin remote)"
	case owner == "":
		return "an unknown owner"
	}
	return owner
}

func recordForeignNotice(ctx context.Context, sessionID, notice string) {
	logging.Info(logging.WithComponent(ctx, "checkpoint"), "prepare-commit-msg: foreign agent commit not linked: different owner",
		slog.String("session_id", sessionID))
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
// repository can claim: the trailer may be a foreign session's. eligible is
// false when listing sessions failed or the target checkpoint already existed.
func (s *ManualCommitStrategy) postCommitWithoutLocalSessions(ctx context.Context, repo *git.Repository, head *plumbing.Reference,
	commit *object.Commit, checkpointID id.CheckpointID, worktreePath string, eligible bool,
) {
	if s.linkForeignAgentCommit(ctx, repo, head, commit, checkpointID, worktreePath, nil, eligible && !isGitSequenceOperation(ctx)) {
		return
	}
	logging.Warn(logging.WithComponent(ctx, "checkpoint"), "post-commit: no active sessions despite trailer",
		slog.String("strategy", "manual-commit"),
		slog.String("checkpoint_id", checkpointID.String()),
	)
}

// linkForeignAgentCommit condenses the foreign session prepare-commit-msg
// stamped this commit for into cpID, in this repository's store, and saves the
// session back to its home under the home's lock. committed is the commit's
// file set, or nil to compute it here. eligible is false when no foreign
// session may own this trailer (a sequence operation, an existing checkpoint);
// the marker is consumed either way. It reports whether it condensed.
//
// The home state advances as for a guest-linked commit (HomeNotHere): the
// transcript window moves past this checkpoint, while the home's BaseCommit,
// files, turn checkpoints and LastCheckpointID stay as they were.
func (s *ManualCommitStrategy) linkForeignAgentCommit(ctx context.Context, repo *git.Repository, head *plumbing.Reference, commit *object.Commit,
	cpID id.CheckpointID, worktreePath string, committed map[string]struct{}, eligible bool,
) bool {
	link, ok := takeForeignLink(ctx, cpID)
	if !ok || !eligible {
		return false
	}
	if s.condenseForeignLink(ctx, repo, head, commit, cpID, worktreePath, committed, link) {
		return true
	}
	// The trailer is already in the commit; say so rather than leave it silent.
	logging.Warn(logging.WithComponent(ctx, "checkpoint"), "post-commit: foreign agent commit trailer has no checkpoint",
		slog.String("session_id", link.SessionID),
		slog.String("checkpoint_id", cpID.String()))
	return false
}

func (s *ManualCommitStrategy) condenseForeignLink(ctx context.Context, repo *git.Repository, head *plumbing.Reference, commit *object.Commit,
	cpID id.CheckpointID, worktreePath string, committed map[string]struct{}, link foreignLink,
) bool {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	store, commonDir, err := openForeignStore(ctx, link.WorktreePath)
	if err != nil {
		return false
	}
	lock, err := stateLockInCommonDir(commonDir, link.SessionID)
	if err != nil {
		return false
	}
	acqCtx, cancel := context.WithTimeout(ctx, foreignLockWait)
	release, err := flock.AcquireContextIn(acqCtx, lock.root, lock.name)
	cancel()
	if err != nil {
		logging.Debug(logCtx, "post-commit: foreign session lock unavailable",
			slog.String("session_id", link.SessionID))
		return false
	}
	defer release()

	state := loadForeignState(ctx, store, link.SessionID)
	if state == nil || foreignIneligibility(state) != "" {
		return false
	}
	if committed == nil {
		committed = filesChangedInCommit(ctx, worktreePath, commit, nil, nil)
	}

	// The session's own files are its home's paths; this checkpoint gets the
	// committed files its transcript wrote under this worktree.
	homeFiles, homeLast := state.FilesTouched, state.LastCheckpointID
	state.FilesTouched = s.foreignEditedFiles(ctx, state, worktreePath, committed)
	state.HomeNotHere = true
	condensed, _, _ := s.condenseAndUpdateState(ctx, repo, cpID, state, head, committed,
		condenseOpts{repoDir: worktreePath, noStoredFallback: true})
	state.FilesTouched, state.HomeNotHere, state.LastCheckpointID = homeFiles, false, homeLast
	if !condensed {
		return false
	}
	state.ForeignTurnCheckpoints = append(state.ForeignTurnCheckpoints,
		session.ForeignTurnCheckpoint{CheckpointID: cpID.String(), WorktreePath: worktreePath})
	if err := store.Save(ctx, state); err != nil {
		logging.Warn(logCtx, "post-commit: could not save foreign session state",
			slog.String("session_id", link.SessionID),
			slog.String("error", err.Error()))
	}
	return true
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

// foreignFinalizeWait bounds finalize-foreign when the turn end set no deadline.
const foreignFinalizeWait = time.Minute

// foreignFinalizeMargin is how much earlier than the parent's kill the child's
// own deadline falls, so the child cancels its git fetches itself: the kill
// reaches only the child's process group, not the groups its fetches run in.
const foreignFinalizeMargin = 3 * time.Second

// runForeignFinalize runs args with this executable in worktreePath, with the
// caller's repo-selecting git variables scrubbed. A var so tests can stand in
// for the subprocess.
var runForeignFinalize = func(ctx context.Context, worktreePath string, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve entire executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	// The child may start git fetches; kill the whole tree at the deadline and
	// bound the wait for pipes they hold.
	execx.TerminateOnCancel(cmd)
	cmd.Dir = worktreePath
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// finalizeForeignTurnCheckpoints finalizes the checkpoints this turn wrote in
// other repositories, by running `entire hooks git finalize-foreign` in each:
// that repository's settings, store and redaction then apply, as they did at
// the commit. Entries that fail stay for a repeated turn end; turn start
// clears them. It reports whether all of them were finalized: only then does
// every foreign checkpoint hold the rest of the turn.
func (s *ManualCommitStrategy) finalizeForeignTurnCheckpoints(ctx context.Context, state *SessionState, deadline time.Time) bool {
	if len(state.ForeignTurnCheckpoints) == 0 {
		return false
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(foreignFinalizeWait)
	}
	runCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var trees []string
	byTree := make(map[string][]string)
	for _, cp := range state.ForeignTurnCheckpoints {
		if _, seen := byTree[cp.WorktreePath]; !seen {
			trees = append(trees, cp.WorktreePath)
		}
		byTree[cp.WorktreePath] = append(byTree[cp.WorktreePath], cp.CheckpointID)
	}
	var failed []session.ForeignTurnCheckpoint
	for _, tree := range trees {
		args := append([]string{"hooks", "git", "finalize-foreign", "--session", state.SessionID, "--home", state.WorktreePath,
			"--deadline", strconv.FormatInt(deadline.Add(-foreignFinalizeMargin).UnixMilli(), 10)}, byTree[tree]...)
		if err := runForeignFinalize(runCtx, tree, args); err != nil {
			logging.Warn(logging.WithComponent(ctx, "checkpoint"), "turn end: could not finalize checkpoints in another repository",
				slog.String("session_id", state.SessionID),
				slog.Int("checkpoints", len(byTree[tree])),
				slog.String("error", err.Error()))
			for _, cpID := range byTree[tree] {
				failed = append(failed, session.ForeignTurnCheckpoint{CheckpointID: cpID, WorktreePath: tree})
			}
		}
	}
	state.ForeignTurnCheckpoints = failed
	return len(failed) == 0
}

// FinalizeForeignTurnCheckpoints is finalize-foreign's work, run in the
// repository holding checkpointIDs: it rewrites them with the full turn
// transcript of sessionID, whose state lives in the repository of
// homeWorktree. The state is read there and never written back; the turn end
// that ran this saves its own.
//
// Only checkpoints that hold sessionID are written: the backfill would
// otherwise overwrite another session's transcript. deadline bounds the
// store's network phase, as at a local turn end; zero means none.
func (s *ManualCommitStrategy) FinalizeForeignTurnCheckpoints(ctx context.Context, sessionID, homeWorktree string, checkpointIDs []string, deadline time.Time) error {
	store, _, err := openForeignStore(ctx, homeWorktree)
	if err != nil {
		return fmt.Errorf("open home session store: %w", err)
	}
	state, err := store.LoadReadOnly(ctx, sessionID)
	if err != nil || state == nil || state.SessionID != sessionID {
		return fmt.Errorf("session %s has no state in %s", sessionID, homeWorktree)
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve worktree: %w", err)
	}
	// Read as this worktree's session, so nothing resolves to the home's
	// .entire, and finalize exactly the checkpoints asked for.
	state.WorktreePath, state.InWorktreeView = root, false
	state.TurnCheckpointIDs = s.checkpointsHoldingSession(ctx, sessionID, checkpointIDs)
	if len(state.TurnCheckpointIDs) != len(checkpointIDs) {
		return fmt.Errorf("session %s is not in every checkpoint asked for", sessionID)
	}
	// An interrupted finalize returns no errors but leaves the unwritten IDs.
	if n := s.finalizeAllTurnCheckpoints(ctx, state, deadline); n > 0 || len(state.TurnCheckpointIDs) > 0 {
		return fmt.Errorf("checkpoints of session %s not finalized (%d errors, %d unwritten)", sessionID, n, len(state.TurnCheckpointIDs))
	}
	return nil
}

// checkpointsHoldingSession returns the IDs whose checkpoint in this
// repository's store holds a session sessionID.
func (s *ManualCommitStrategy) checkpointsHoldingSession(ctx context.Context, sessionID string, checkpointIDs []string) []string {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil
	}
	defer repo.Close()
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		return nil
	}
	var held []string
	for _, raw := range checkpointIDs {
		cpID, err := id.NewCheckpointID(raw)
		if err != nil {
			continue
		}
		summary, err := store.Read(ctx, cpID)
		if err != nil || summary == nil {
			continue
		}
		for i := range summary.Sessions {
			// Metadata only: a files/prompt-only checkpoint has no transcript yet.
			if meta, err := store.ReadSessionMetadata(ctx, cpID, i); err == nil && meta != nil && meta.SessionID == sessionID {
				held = append(held, raw)
				break
			}
		}
	}
	return held
}
