package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Uses t.Chdir — do NOT add t.Parallel().

// saveSiblingSession saves a session homed in dir with the given phase, last
// interaction and task records.
func saveSiblingSession(t *testing.T, s *ManualCommitStrategy, dir, id string, phase session.Phase, last *time.Time, tasks ...session.TaskRecord) {
	t.Helper()
	ctx := context.Background()
	state := &SessionState{SessionID: id, WorktreePath: dir, LastInteractionTime: last, TaskRecords: tasks}
	saveSessionMatchState(ctx, t, s, dir, state)
	state.Phase = phase
	require.NoError(t, s.saveSessionState(ctx, state))
}

// A commit from a worktree with no session of its own falls back to the
// sessions of a sibling worktree. One whose last interaction is older than
// recentSessionWindow is not plausibly the commit's session, so it is not
// linked: post-commit would otherwise condense its whole transcript into every
// commit made here (cli issue 2979). Kept: a session with no recorded
// interaction, as before, one mid-turn whose turn started within
// activeSessionInteractionThreshold, and one whose background subagent is
// still running.
func TestCommitLinking_FallbackSkipsSiblingSessionsIdleBeyondTheWindow(t *testing.T) {
	now := time.Now()
	twoDaysAgo := now.Add(-48 * time.Hour)
	dayAgo := now.Add(-24 * time.Hour)
	justPast := now.Add(-recentSessionWindow - time.Minute)
	recent := now.Add(-time.Minute)
	type sess struct {
		phase session.Phase
		last  *time.Time
		tasks []session.TaskRecord
	}
	inFlight := []session.TaskRecord{{ToolUseID: "call_bg", AgentID: "child", StartedAt: justPast}}
	finished := []session.TaskRecord{{ToolUseID: "call_bg", AgentID: "child", StartedAt: dayAgo, CompletedAt: justPast}}
	for _, tt := range []struct {
		name     string
		sessions map[string]sess
		want     []string
	}{
		{name: "idle for a day", sessions: map[string]sess{"stale": {session.PhaseIdle, &dayAgo, nil}}},
		{name: "idle just past the window", sessions: map[string]sess{"stale": {session.PhaseIdle, &justPast, nil}}},
		{name: "recent", sessions: map[string]sess{"recent": {session.PhaseIdle, &recent, nil}}, want: []string{"recent"}},
		{name: "no recorded interaction", sessions: map[string]sess{"undated": {session.PhaseIdle, nil, nil}}, want: []string{"undated"}},
		{name: "mid-turn past the window", sessions: map[string]sess{"long-turn": {session.PhaseActive, &justPast, nil}}, want: []string{"long-turn"}},
		{name: "left active for two days", sessions: map[string]sess{"abandoned": {session.PhaseActive, &twoDaysAgo, nil}}},
		{name: "idle with a subagent in flight", sessions: map[string]sess{"bg": {session.PhaseIdle, &dayAgo, inFlight}}, want: []string{"bg"}},
		{name: "idle with a finished subagent", sessions: map[string]sess{"bg": {session.PhaseIdle, &dayAgo, finished}}},
		{
			name: "stale beside recent and undated",
			sessions: map[string]sess{
				"stale":   {session.PhaseIdle, &dayAgo, nil},
				"recent":  {session.PhaseIdle, &recent, nil},
				"undated": {session.PhaseIdle, nil, nil},
			},
			want: []string{"recent", "undated"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mainDir, linkedDir := twoWorktrees(t)
			s := &ManualCommitStrategy{}
			for id, st := range tt.sessions {
				saveSiblingSession(t, s, mainDir, id, st.phase, st.last, st.tasks...)
			}
			assert.ElementsMatch(t, tt.want, linkingIDs(t, linkedDir))
		})
	}
}

// An idle sibling session that recorded edits in the committing worktree still
// joins the commit, held to those edits: the idle check only narrows the
// fallback, not the sessions that edited this worktree from elsewhere.
func TestCommitLinking_IdleSiblingSessionThatEditedHereStillJoins(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	dayAgo := time.Now().Add(-24 * time.Hour)
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:           "edited-here",
		WorktreePath:        mainDir,
		LastInteractionTime: &dayAgo,
		OtherWorktrees: map[string]*session.WorktreeWork{
			linkedKey(t, linkedDir): {Path: linkedDir, FilesTouched: []string{"feature.go"}, Incarnation: mark(t, linkedDir)},
		},
	})

	linked := linkingSessions(t, linkedDir)
	require.Contains(t, linked, "edited-here")
	assert.True(t, linked["edited-here"].LinkedByWorktreeEntry, "held to the edits it recorded here")
}

// A session prepare-commit-msg linked is kept by the idle check however idle
// it has become since; the other fallback sessions are still judged.
func TestCommitLinking_FallbackKeepsTheSessionsPrepareLinked(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	dayAgo := time.Now().Add(-24 * time.Hour)
	saveSiblingSession(t, s, mainDir, "linked", session.PhaseIdle, &dayAgo)
	saveSiblingSession(t, s, mainDir, "stale", session.PhaseIdle, &dayAgo)

	t.Chdir(linkedDir)
	clearSessionMatchCaches()
	sessions, err := s.findSessionsForCommitLinking(withLinkedSessions(context.Background(), []string{"linked"}), linkedDir)
	require.NoError(t, err)
	assert.Equal(t, []string{"linked"}, linkedSessionIDs(sessions))
}

// On an amend, the owner of the checkpoint the amended commit links is kept by
// the idle check however idle it is; the other fallback sessions are still
// judged.
func TestCommitLinking_FallbackKeepsTheOwnerOfAnAmendedCheckpoint(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	dayAgo := time.Now().Add(-24 * time.Hour)
	target := id.MustCheckpointID("a1b2c3d4e5f6")
	saveSiblingSession(t, s, mainDir, "owner", session.PhaseIdle, &dayAgo)
	saveSiblingSession(t, s, mainDir, "stale", session.PhaseIdle, &dayAgo)
	owner, err := s.loadSessionState(ctx, "owner")
	require.NoError(t, err)
	owner.LastCheckpointID = target
	require.NoError(t, s.saveSessionState(ctx, owner))

	t.Chdir(linkedDir)
	clearSessionMatchCaches()
	sessions, err := s.findSessionsForCommitLinking(withAmendedCheckpoint(ctx, amendedLink{checkpointID: target}), linkedDir)
	require.NoError(t, err)
	assert.Equal(t, []string{"owner"}, linkedSessionIDs(sessions))
}

// Prepare links a sibling worktree's session that is not idle when it runs,
// and stamps the trailer. The commit can then wait on the TTY prompt or the
// message editor, and by post-commit the session looks idle: past
// recentSessionWindow, or with its background subagent finished. Post-commit
// keeps the sessions prepare recorded for the trailer, so it still condenses
// that session into the stamped checkpoint. Judged afresh, it dropped the
// session and left the trailer naming a checkpoint that is never written.
func TestCommitHooks_LinkedSiblingSessionCondensesAfterALongWait(t *testing.T) {
	now := time.Now()
	recent := now.Add(-recentSessionWindow + time.Minute)
	justPast := now.Add(-recentSessionWindow - time.Minute)
	dayAgo := now.Add(-24 * time.Hour)
	for _, tt := range []struct {
		name             string
		atPrepare        func(*SessionState)
		beforePostCommit func(*SessionState)
	}{
		{
			name:             "aged past the window",
			atPrepare:        func(st *SessionState) { st.LastInteractionTime = &recent },
			beforePostCommit: func(st *SessionState) { st.LastInteractionTime = &justPast },
		},
		{
			name: "its subagent finished",
			atPrepare: func(st *SessionState) {
				st.LastInteractionTime = &dayAgo
				st.TaskRecords = []session.TaskRecord{{ToolUseID: "call_bg", AgentID: "child", StartedAt: justPast}}
			},
			beforePostCommit: func(st *SessionState) { st.TaskRecords[0].CompletedAt = time.Now() },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			homeDir, err := filepath.EvalSymlinks(setupGitRepo(t))
			require.NoError(t, err, "the recorded home must match git's worktree list")
			t.Chdir(homeDir)
			s := NewManualCommitStrategy()
			setupSessionWithCheckpointAndFile(t, s, homeDir, "sess-sibling", "notes.txt")
			updateSession := func(change func(*SessionState)) *SessionState {
				t.Helper()
				state, err := s.loadSessionState(ctx, "sess-sibling")
				require.NoError(t, err)
				state.WorktreePath = homeDir
				state.Phase = session.PhaseIdle
				change(state)
				require.NoError(t, s.saveSessionState(ctx, state))
				return state
			}
			updateSession(tt.atPrepare)

			siblingDir := addSiblingWorktree(t, homeDir, "commit-wt")
			t.Chdir(siblingDir)
			testutil.WriteFile(t, siblingDir, "notes.txt", "agent content for notes.txt")
			testutil.GitAdd(t, siblingDir, "notes.txt")
			msg := prepareMessage(t, "add notes\n")
			cps := checkpointIDs(msg)
			require.Len(t, cps, 1, "prepare links the session, not idle when it runs: %q", msg)

			state := updateSession(tt.beforePostCommit)
			require.True(t, idleBeyondRecentWindow(state, time.Now()), "sanity: by post-commit the session looks idle")
			testutil.GitCommit(t, siblingDir, msg)
			require.NoError(t, NewManualCommitStrategy().PostCommit(ctx))

			state, err = s.loadSessionState(ctx, "sess-sibling")
			require.NoError(t, err)
			assert.Equal(t, cps[0], state.LastCheckpointID.String(), "post-commit condenses the session prepare linked")
		})
	}
}

// Post-commit reads the sessions prepare recorded for the trailer it condenses
// into, and the ones among them it trusted on their shape alone, and consumes
// the record with the stamp record.
func TestStampedSessions_RecordedWithTheTrailerAndTakenOnce(t *testing.T) {
	t.Chdir(setupGitRepo(t))
	reserved, other := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b1b2c3d4e5f6")
	require.True(t, recordStampedTrailer(withLinkedSessions(context.Background(), []string{"first"}), reserved))
	later := withTrustedSessions(withLinkedSessions(context.Background(), []string{"second", "third"}), []string{"third"})
	require.True(t, recordStampedTrailer(later, reserved), "a later prepare stamping the same reservation")
	require.True(t, recordStampedTrailer(context.Background(), other), "a stamp with no linked sessions records none")

	assert.Equal(t, map[id.CheckpointID]stampedSessions{
		reserved: {CheckpointID: reserved, SessionIDs: []string{"second", "third"}, TrustedSessionIDs: []string{"third"}},
	}, takeStampedSessions(context.Background()))
	assert.Empty(t, takeStampedSessions(context.Background()), "post-commit consumes the record")
}

// The idle check is for linking new commits only. Post-rewrite in the
// worktree still finds an idle sibling session and remaps its BaseCommit, as
// it keeps an existing link rather than making a new one.
func TestPostRewrite_StillRemapsAnIdleSiblingSession(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	dayAgo := time.Now().Add(-24 * time.Hour)
	saveSiblingSession(t, s, mainDir, "idle", session.PhaseIdle, &dayAgo)
	before, err := s.loadSessionState(ctx, "idle")
	require.NoError(t, err)
	newSHA := strings.Repeat("b", 40)

	t.Chdir(linkedDir)
	clearSessionMatchCaches()
	require.NoError(t, s.PostRewrite(ctx, "amend", strings.NewReader(before.BaseCommit+" "+newSHA+"\n")))

	after, err := s.loadSessionState(ctx, "idle")
	require.NoError(t, err)
	assert.Equal(t, newSHA, after.BaseCommit)
}

// An amend keeps the amended commit's trailer and stamps nothing, so
// post-commit condenses the amended-in work into that existing checkpoint for
// the session that owns it. From a worktree with no session of its own, the
// owner is kept however idle it has become: the trailer already links it, and
// the idle check only narrows new links. Judged as a new link, the owner was
// dropped and its work never reached the checkpoint. A message reused with
// `commit -C` keeps the trailer the same way, but makes a new commit: there
// the idle owner is still dropped, and the old checkpoint is left as it was.
// So is it when an amend of a later commit reuses the first one's message: that
// trailer never linked the owner to the commit being amended. A trailer prepare
// restores for a session based on that commit does link it, as does one the
// checkpoint records the amended commit for (moved out of its final trailer
// block, then put back by the amend). Without a reflog
// to tell an amend from a new commit, the owner is kept, as it was before the
// idle check. A commit started while the amend's post-commit runs can land
// between post-commit reading HEAD and reading HEAD's reflog: its entry is
// then the newest, and the amend is still told by its own entry.
func TestCommitHooks_AmendFromSiblingKeepsTheIdleOwner(t *testing.T) {
	for _, tt := range []struct {
		name       string
		commitArgs []string
		noReflog   bool
		resetAfter bool
		// amendOther amends a later commit with the first one's message.
		amendOther bool
		// restore edits the trailer out, and bases the session on the commit
		// being amended, for prepare to restore its trailer.
		restore bool
		// movedTrailer commits the first commit with its trailer moved out of
		// the final trailer block, and the amend puts it back.
		movedTrailer bool
		// laterCommit lands another commit after the amend, before its
		// post-commit reads HEAD's reflog.
		laterCommit bool
		// dropEntry removes the commit's own entry from HEAD's reflog,
		// leaving only older ones.
		dropEntry bool
		condensed bool
	}{
		{name: "amend", commitArgs: []string{"--amend"}, condensed: true},
		{name: "message reused with commit -C", condensed: false},
		{name: "amend without a reflog", commitArgs: []string{"--amend"}, noReflog: true, condensed: true},
		{name: "commit -C, then a hook's git reset HEAD", resetAfter: true, condensed: false},
		{name: "amend of another commit reusing the message", commitArgs: []string{"--amend"}, amendOther: true, condensed: false},
		{name: "amend restoring the trailer of a session based on it", commitArgs: []string{"--amend"}, amendOther: true, restore: true, condensed: true},
		{name: "amend of a commit whose trailer was moved", commitArgs: []string{"--amend"}, movedTrailer: true, condensed: true},
		{name: "amend, then another commit lands before post-commit reads the reflog", commitArgs: []string{"--amend"}, laterCommit: true, condensed: true},
		{name: "commit -C, then another commit lands before post-commit reads the reflog", laterCommit: true, condensed: false},
		{name: "amend with no reflog entry of its own", commitArgs: []string{"--amend"}, dropEntry: true, condensed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			homeDir, err := filepath.EvalSymlinks(setupGitRepo(t))
			require.NoError(t, err, "the recorded home must match git's worktree list")
			t.Chdir(homeDir)
			s := NewManualCommitStrategy()
			setupSessionWithCheckpointAndFile(t, s, homeDir, "sess-sibling", "notes.txt")
			setLastInteraction := func(last time.Time) {
				t.Helper()
				state, err := s.loadSessionState(ctx, "sess-sibling")
				require.NoError(t, err)
				state.WorktreePath = homeDir
				state.Phase = session.PhaseIdle
				state.LastInteractionTime = &last
				require.NoError(t, s.saveSessionState(ctx, state))
			}
			setLastInteraction(time.Now().Add(-time.Minute))

			siblingDir := addSiblingWorktree(t, homeDir, "commit-wt")
			t.Chdir(siblingDir)
			testutil.WriteFile(t, siblingDir, "notes.txt", "agent content for notes.txt")
			testutil.GitAdd(t, siblingDir, "notes.txt")
			msg := prepareMessage(t, "add notes\n")
			cps := checkpointIDs(msg)
			require.Len(t, cps, 1, "prepare links the recent sibling session: %q", msg)
			committed := msg
			if tt.movedTrailer {
				committed = msg + "\nA note written after the trailer.\n"
			}
			if tt.dropEntry {
				// Through git, so HEAD's reflog keeps an older entry that moved
				// HEAD once the amend's own is gone.
				testutil.RunGit(t, siblingDir, "commit", "-q", "-m", committed)
			} else {
				testutil.GitCommit(t, siblingDir, committed)
			}
			require.NoError(t, NewManualCommitStrategy().PostCommit(ctx))
			state, err := s.loadSessionState(ctx, "sess-sibling")
			require.NoError(t, err)
			require.Equal(t, cps[0], state.LastCheckpointID.String(), "sanity: the session owns the commit's checkpoint")
			if tt.movedTrailer {
				head := strings.TrimSpace(testutil.RunGit(t, siblingDir, "log", "-1", "--format=%B"))
				require.Empty(t, LinkedCheckpoints(ctx, head), "sanity: the commit's final trailer block names no checkpoint")
			}

			// The agent's next turn adds more.txt; then the session sits idle
			// for a day before a commit naming the checkpoint takes it in.
			t.Chdir(homeDir)
			metadataDir := paths.EntireMetadataDir + "/sess-sibling"
			testutil.WriteFile(t, homeDir, "more.txt", "agent content for more.txt")
			testutil.WriteFile(t, homeDir, filepath.Join(metadataDir, paths.TranscriptFileName),
				testTranscriptPromptResponse+`{"type":"human","message":{"content":"add more"}}`+"\n")
			require.NoError(t, s.SaveStep(ctx, StepContext{
				SessionID:     "sess-sibling",
				NewFiles:      []string{"more.txt"},
				MetadataDir:   metadataDir,
				CommitMessage: "Checkpoint 2",
				AuthorName:    "Test",
				AuthorEmail:   "test@test.com",
			}))
			setLastInteraction(time.Now().Add(-24 * time.Hour))
			t.Chdir(siblingDir)
			if tt.amendOther {
				testutil.WriteFile(t, siblingDir, "other.txt", "work of no session")
				testutil.GitAdd(t, siblingDir, "other.txt")
				testutil.RunGit(t, siblingDir, "commit", "-q", "-m", "other work")
				require.NoError(t, NewManualCommitStrategy().PostCommit(ctx))
			}
			testutil.WriteFile(t, siblingDir, "more.txt", "agent content for more.txt")
			testutil.GitAdd(t, siblingDir, "more.txt")
			// git reports source "commit" for both `--amend` and `-C <rev>`.
			draft := msg
			if tt.restore {
				draft = "add notes\n"
				state, err := s.loadSessionState(ctx, "sess-sibling")
				require.NoError(t, err)
				state.BaseCommit = strings.TrimSpace(testutil.RunGit(t, siblingDir, "rev-parse", "HEAD"))
				require.NoError(t, s.saveSessionState(ctx, state))
			}
			msgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
			require.NoError(t, os.WriteFile(msgFile, []byte(draft), 0o600))
			require.NoError(t, NewManualCommitStrategy().PrepareCommitMsg(ctx, msgFile, "commit"))
			kept, err := os.ReadFile(msgFile)
			require.NoError(t, err)
			require.Equal(t, cps, checkpointIDs(string(kept)), "prepare keeps or restores the trailer")
			if tt.noReflog {
				testutil.RunGit(t, siblingDir, "config", "core.logAllRefUpdates", "false")
				for _, dirFlag := range []string{"--absolute-git-dir", "--git-common-dir"} {
					gitDir := strings.TrimSpace(testutil.RunGit(t, siblingDir, "rev-parse", "--path-format=absolute", dirFlag))
					require.NoError(t, os.RemoveAll(filepath.Join(gitDir, "logs")))
				}
			}
			testutil.RunGit(t, siblingDir, append([]string{"commit", "-q", "-F", msgFile}, tt.commitArgs...)...)
			if tt.noReflog {
				require.Empty(t, headReflog(ctx), "sanity: the amend left no reflog")
			}
			if tt.resetAfter {
				// An earlier post-commit hook's reset records an entry that
				// does not move HEAD.
				testutil.RunGit(t, siblingDir, "reset", "-q", "HEAD")
			}
			if tt.dropEntry {
				dropNewestReflogEntry(t, siblingDir)
			}
			if tt.laterCommit {
				// The next commit lands after post-commit has read HEAD (still
				// this commit) and before it reads HEAD's reflog, whose newest
				// entry is then that commit's.
				landLaterCommitInReflog(t, siblingDir)
			}
			require.NoError(t, NewManualCommitStrategy().PostCommit(ctx))

			repo, err := OpenRepository(ctx)
			require.NoError(t, err)
			defer repo.Close()
			store, err := s.getPersistentStore(ctx, repo)
			require.NoError(t, err)
			summary, err := store.Read(ctx, id.MustCheckpointID(cps[0]))
			require.NoError(t, err)
			require.NotNil(t, summary)
			if tt.condensed {
				assert.Contains(t, summary.FilesTouched, "more.txt", "the amend's agent work reaches the existing checkpoint")
			} else {
				assert.NotContains(t, summary.FilesTouched, "more.txt", "a new commit does not fold the idle session's work into the old checkpoint")
			}
		})
	}
}

// landLaterCommitInReflog records in HEAD's reflog a commit on top of HEAD, as
// git does once that commit lands, but leaves HEAD where it is: what a hook
// that read HEAD just before sees when it reads the reflog.
func landLaterCommitInReflog(t *testing.T, dir string) {
	t.Helper()
	head := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	next := strings.TrimSpace(testutil.RunGit(t, dir, "commit-tree", "-p", head, "-m", "next commit", head+"^{tree}"))
	gitDir := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "--path-format=absolute", "--absolute-git-dir"))
	logFile, err := os.OpenFile(filepath.Join(gitDir, "logs", "HEAD"), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	defer logFile.Close()
	_, err = fmt.Fprintf(logFile, "%s %s Test <test@test.com> %d +0000\tcommit: next commit\n", head, next, time.Now().Unix())
	require.NoError(t, err)
	require.Equal(t, next, headReflog(context.Background())[0].newHash.String(), "sanity: the later commit's entry is the newest")
}

// dropNewestReflogEntry removes the newest entry of HEAD's reflog, leaving
// HEAD as it is.
func dropNewestReflogEntry(t *testing.T, dir string) {
	t.Helper()
	gitDir := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "--path-format=absolute", "--absolute-git-dir"))
	logPath := filepath.Join(gitDir, "logs", "HEAD")
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	lines := strings.SplitAfter(strings.TrimSuffix(string(data), "\n"), "\n")
	require.Greater(t, len(lines), 1, "sanity: older entries stay")
	require.NoError(t, os.WriteFile(logPath, []byte(strings.Join(lines[:len(lines)-1], "")), 0o600))
	head := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	entries := headReflog(context.Background())
	require.False(t, slices.ContainsFunc(entries, func(e reflogEntry) bool { return e.newHash.String() == head }),
		"sanity: no entry names the commit")
	require.True(t, slices.ContainsFunc(entries, func(e reflogEntry) bool { return !e.oldHash.Equal(e.newHash) }),
		"sanity: an older entry that moved HEAD is left")
}
