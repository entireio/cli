package strategy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
)

// foreignCommitFixture is a session homed in A, identified by its caller
// variable, whose live Claude transcript writes <B>/b.txt by absolute path,
// with b.txt staged in B (the CWD). A has opted in to cross-repo commits, and
// both origins belong to the same GitHub owner.
func foreignCommitFixture(t *testing.T, mutate func(*SessionState)) foreignRepos {
	t.Helper()
	r := newForeignRepos(t)
	testutil.RunGit(t, r.a, "remote", "add", "origin", "git@github.com:acme/a.git")
	testutil.RunGit(t, r.b, "remote", "add", "origin", "git@github.com:acme/b.git")
	r.setCrossRepoCommits(t, "on\n")
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	transcript := filepath.Join(t.TempDir(), foreignTestSessionID+".jsonl")
	bPath, err := json.Marshal(filepath.Join(r.b, "b.txt")) // backslashes on Windows
	require.NoError(t, err)
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":` +
		string(bPath) + `,"content":"x"}}]}}`
	require.NoError(t, os.WriteFile(transcript, []byte(`{"type":"human","message":{"content":"make b"}}`+"\n"+line+"\n"), 0o600))
	r.saveForeignState(t, func(s *SessionState) {
		s.TranscriptPath = transcript
		if mutate != nil {
			mutate(s)
		}
	})
	t.Setenv("CLAUDE_CODE_SESSION_ID", foreignTestSessionID)
	testutil.WriteFile(t, r.b, "b.txt", "b\n")
	testutil.GitAdd(t, r.b, "b.txt")
	return r
}

// setCrossRepoCommits writes A's opt-in file with content, or removes it
// when content is empty.
func (r foreignRepos) setCrossRepoCommits(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(r.a, ".git", filepath.FromSlash(settings.CrossRepoCommitsFile))
	if content == "" {
		require.NoError(t, os.RemoveAll(path))
		return
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// prepareForeign runs PrepareCommitMsg on text and returns the message.
func prepareForeign(t *testing.T, text, source string) string {
	t.Helper()
	msgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgFile, []byte(text), 0o600))
	require.NoError(t, NewManualCommitStrategy().PrepareCommitMsg(context.Background(), msgFile, source))
	got, err := os.ReadFile(msgFile)
	require.NoError(t, err)
	return string(got)
}

// commitForeign prepares, commits and post-commits text in B, returning the
// stamped checkpoint ID (empty when none was stamped).
func commitForeign(t *testing.T, r foreignRepos, text string) id.CheckpointID {
	t.Helper()
	msg := prepareForeign(t, text, commitSourceMessage)
	testutil.GitCommit(t, r.b, msg)
	require.NoError(t, NewManualCommitStrategy().PostCommit(context.Background()))
	cpID, _ := trailers.ParseCheckpoint(msg)
	return cpID
}

func loadForeignTestState(t *testing.T, r foreignRepos) *SessionState {
	t.Helper()
	state, err := r.aStore(t).LoadReadOnly(context.Background(), foreignTestSessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	return state
}

// readBCheckpoint returns the checkpoint's content in B's store, or nil.
func readBCheckpoint(t *testing.T, r foreignRepos, cpID id.CheckpointID) *checkpoint.SessionContent {
	t.Helper()
	repo, err := gitrepo.OpenPath(r.b)
	require.NoError(t, err)
	defer repo.Close()
	content, err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		return nil
	}
	return content
}

func TestPrepareCommitMsg_ForeignAgentCommit(t *testing.T) {
	t.Run("stamped", func(t *testing.T) {
		foreignCommitFixture(t, nil)
		_, found := trailers.ParseCheckpoint(prepareForeign(t, "add b\n", commitSourceMessage))
		assert.True(t, found)
	})
	t.Run("stamped beside a local session without content", func(t *testing.T) {
		r := foreignCommitFixture(t, nil)
		saveIdentitySession(t, "local-empty", func(st *SessionState) { st.WorktreePath = r.b })
		_, found := trailers.ParseCheckpoint(prepareForeign(t, "add b\n", commitSourceMessage))
		assert.True(t, found)
	})
	t.Run("terminal is never prompted", func(t *testing.T) {
		foreignCommitFixture(t, nil)
		t.Setenv("ENTIRE_TEST_TTY", "1")
		assert.NotContains(t, prepareForeign(t, "add b\n", commitSourceMessage), trailers.CheckpointTrailerKey)
	})
	t.Run("terminal under commit_linking always", func(t *testing.T) {
		foreignCommitFixture(t, nil)
		t.Setenv("ENTIRE_TEST_TTY", "1")
		require.NoError(t, saveCommitLinkingAlways(context.Background()))
		assert.Contains(t, prepareForeign(t, "add b\n", commitSourceMessage), trailers.CheckpointTrailerKey)
	})
	t.Run("amend", func(t *testing.T) {
		foreignCommitFixture(t, nil)
		assert.Equal(t, "amended\n", prepareForeign(t, "amended\n", "commit"))
	})
	t.Run("pasted trailer gets a fresh one", func(t *testing.T) {
		foreignCommitFixture(t, nil)
		got := prepareForeign(t, "typed\n\nEntire-Checkpoint: aabbccdd0003\n", commitSourceMessage)
		ids := trailers.ParseAllCheckpoints(got)
		require.Len(t, ids, 2, "a trailer Entire did not stamp names someone else's checkpoint, so a fresh one is added")
		assert.Equal(t, "aabbccdd0003", ids[0].String())
	})
	t.Run("message already stamped", func(t *testing.T) {
		foreignCommitFixture(t, nil)
		// Prepare ran before for this commit and recorded its trailer.
		require.True(t, recordStampedTrailer(context.Background(), id.MustCheckpointID("aabbccdd0003")))
		text := "typed\n\nEntire-Checkpoint: aabbccdd0003\n"
		assert.Equal(t, text, prepareForeign(t, text, commitSourceMessage))
	})
}

func TestPostCommit_ForeignAgentCommitCondensed(t *testing.T) {
	r := foreignCommitFixture(t, func(s *SessionState) {
		s.LastCheckpointID = id.MustCheckpointID("a0a0a0a0a0a0")
		s.FilesTouched = []string{"README.md"} // A's path; B's commit has one too
		s.TurnCheckpointIDs = []string{"a0a0a0a0a0a0"}
	})
	testutil.WriteFile(t, r.b, "README.md", "b readme\n")
	testutil.GitAdd(t, r.b, "README.md")
	before := loadForeignTestState(t, r)

	cpID := commitForeign(t, r, "add b\n")
	require.False(t, cpID.IsEmpty())

	content := readBCheckpoint(t, r, cpID)
	require.NotNil(t, content)
	assert.Equal(t, foreignTestSessionID, content.Metadata.SessionID)
	assert.Equal(t, []string{"b.txt"}, content.Metadata.FilesTouched, "only what the transcript wrote under B")

	after := loadForeignTestState(t, r)
	assert.Greater(t, after.CheckpointTranscriptStart, before.CheckpointTranscriptStart, "the transcript window advanced")
	assert.Equal(t, before.LastCheckpointID, after.LastCheckpointID)
	assert.Equal(t, before.BaseCommit, after.BaseCommit)
	assert.Equal(t, before.FilesTouched, after.FilesTouched)
	assert.Equal(t, before.TurnCheckpointIDs, after.TurnCheckpointIDs)
	assert.False(t, after.HomeNotHere)
}

// A trailer typed by hand has no marker and is not condensed.
func TestPostCommit_ForeignTrailerWithoutMarker(t *testing.T) {
	r := foreignCommitFixture(t, nil)
	cpID := id.MustCheckpointID("aabbccdd0005")
	testutil.GitCommit(t, r.b, "typed\n\nEntire-Checkpoint: "+cpID.String()+"\n")
	require.NoError(t, NewManualCommitStrategy().PostCommit(context.Background()))
	assert.Nil(t, readBCheckpoint(t, r, cpID))
}

// A's lock held by A's own work: the commit gives up within the bound and is
// left unlinked rather than blocked.
func TestPostCommit_ForeignLockHeld(t *testing.T) {
	r := foreignCommitFixture(t, nil)
	orig := foreignLockWait
	foreignLockWait = 200 * time.Millisecond
	t.Cleanup(func() { foreignLockWait = orig })
	before := loadForeignTestState(t, r)
	lock, err := stateLockInCommonDir(filepath.Join(r.a, ".git"), foreignTestSessionID)
	require.NoError(t, err)
	held, err := flock.AcquireIn(lock.root, lock.name)
	require.NoError(t, err)
	defer held()

	start := time.Now()
	cpID := commitForeign(t, r, "add b\n")
	assert.Less(t, time.Since(start), 10*time.Second)
	require.False(t, cpID.IsEmpty())
	assert.Nil(t, readBCheckpoint(t, r, cpID))
	assert.Equal(t, before.CheckpointTranscriptStart, loadForeignTestState(t, r).CheckpointTranscriptStart)
}

// After the B commit, a trailer-less amend at home restores A's own ID, not B's.
func TestForeignAgentCommit_HomeAmendKeepsOwnCheckpoint(t *testing.T) {
	homeID := id.MustCheckpointID("a0a0a0a0a0a1")
	var aHead string
	r := foreignCommitFixture(t, func(s *SessionState) {
		s.LastCheckpointID = homeID
		aHead = strings.TrimSpace(testutil.RunGit(t, s.WorktreePath, "rev-parse", "HEAD"))
		s.BaseCommit = aHead
	})
	require.False(t, commitForeign(t, r, "add b\n").IsEmpty())

	t.Chdir(r.a)
	paths.ClearWorktreeRootCache()
	cpID, found := trailers.ParseCheckpoint(prepareForeign(t, "home amend\n", "commit"))
	require.True(t, found)
	assert.Equal(t, homeID, cpID)
	assert.Equal(t, aHead, loadForeignTestState(t, r).BaseCommit)
}

// The transcript wrote none of the committed files (here only A's README.md,
// as a shell edit in B would look): the agent's own commit is attributed whole,
// never re-resolved against A's paths.
func TestPostCommit_ForeignAgentCommitWithoutTranscriptOverlap(t *testing.T) {
	r := foreignCommitFixture(t, nil)
	state := loadForeignTestState(t, r)
	aPath, err := json.Marshal(filepath.Join(r.a, "README.md"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(state.TranscriptPath, []byte(`{"type":"human","message":{"content":"x"}}`+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":`+string(aPath)+`,"content":"x"}}]}}`+"\n"), 0o600))
	testutil.WriteFile(t, r.b, "README.md", "b readme\n")
	testutil.GitAdd(t, r.b, "README.md")

	cpID := commitForeign(t, r, "add b and readme\n")
	require.False(t, cpID.IsEmpty())
	content := readBCheckpoint(t, r, cpID)
	require.NotNil(t, content)
	assert.ElementsMatch(t, []string{"README.md", "b.txt"}, content.Metadata.FilesTouched)
}

// stampedOrNotice prepares a commit in B and returns whether it was stamped
// and the notice commit-msg would print.
func stampedOrNotice(t *testing.T) (bool, string) {
	t.Helper()
	_, found := trailers.ParseCheckpoint(prepareForeign(t, "add b\n", commitSourceMessage))
	return found, takeForeignNotice(context.Background())
}

// Linking is off unless A's developer turned it on in A's clone.
func TestPrepareCommitMsg_ForeignAgentCommitOptIn(t *testing.T) {
	cases := []struct {
		name    string
		content string
		stamped bool
	}{
		{"on", "on\n", true},
		{"missing", "", false},
		{"off", "off\n", false},
		{"garbage", "yes\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := foreignCommitFixture(t, nil)
			r.setCrossRepoCommits(t, tc.content)

			stamped, notice := stampedOrNotice(t)
			assert.Equal(t, tc.stamped, stamped)
			if tc.stamped {
				assert.Empty(t, notice)
				return
			}
			assert.Contains(t, notice, foreignTestSessionID)
			assert.Contains(t, notice, "off for a")
			assert.Contains(t, notice, "from a terminal in a")
			assert.NotContains(t, notice, "entire configure", "an agent reads this notice; it names no command")
			assert.NotContains(t, notice, r.a, "path-free")
		})
	}
}

// A's transcript may go only where A's origin's owner can read it: every
// place B pushes checkpoints must belong to that owner.
func TestPrepareCommitMsg_ForeignAgentCommitDestinationGate(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, r foreignRepos)
		stamped bool
	}{
		{"same owner over https", func(t *testing.T, r foreignRepos) {
			testutil.RunGit(t, r.b, "remote", "set-url", "origin", "https://github.com/ACME/b.git")
		}, true},
		{"different owner", func(t *testing.T, r foreignRepos) {
			testutil.RunGit(t, r.b, "remote", "set-url", "origin", "git@github.com:other/b.git")
		}, false},
		{"home has no origin", func(t *testing.T, r foreignRepos) {
			testutil.RunGit(t, r.a, "remote", "remove", "origin")
		}, false},
		{"this repo has no remote", func(t *testing.T, r foreignRepos) {
			testutil.RunGit(t, r.b, "remote", "remove", "origin")
		}, false},
		{"unknown forge", func(t *testing.T, r foreignRepos) {
			testutil.RunGit(t, r.a, "remote", "set-url", "origin", "git@git.example.com:acme/a.git")
			testutil.RunGit(t, r.b, "remote", "set-url", "origin", "git@git.example.com:acme/b.git")
		}, false},
		{"push URL to another owner", func(t *testing.T, r foreignRepos) {
			testutil.RunGit(t, r.b, "remote", "set-url", "--push", "origin", "git@github.com:other/b.git")
		}, false},
		{"one of two push URLs to another owner", func(t *testing.T, r foreignRepos) {
			testutil.RunGit(t, r.b, "remote", "set-url", "--add", "--push", "origin", "git@github.com:acme/b.git")
			testutil.RunGit(t, r.b, "remote", "set-url", "--add", "--push", "origin", "git@github.com:other/b.git")
		}, false},
		{"checkpoint remote of another owner", func(t *testing.T, r foreignRepos) {
			testutil.WriteFile(t, r.b, ".entire/settings.local.json",
				`{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"other/checkpoints"}}}`)
		}, false},
		{"checkpoint remote of the same owner", func(t *testing.T, r foreignRepos) {
			testutil.WriteFile(t, r.b, ".entire/settings.local.json",
				`{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := foreignCommitFixture(t, nil)
			tc.setup(t, r)

			stamped, notice := stampedOrNotice(t)
			assert.Equal(t, tc.stamped, stamped)
			if tc.stamped {
				assert.Empty(t, notice)
				return
			}
			assert.Contains(t, notice, "This commit is not linked to agent session "+foreignTestSessionID)
			assert.NotContains(t, notice, r.a)
		})
	}
}

// A's transcript is redacted by B's rules alone, so B's must cover A's.
func TestPrepareCommitMsg_ForeignAgentCommitRedactionGate(t *testing.T) {
	const codename = `{"redaction":{"custom_redactions":{"codename":"zeta-[0-9]+"}}}`
	const pack = "name: internal\nversion: \"1\"\nrules:\n  - id: ticket\n    regex: \"TKT-[0-9]{6}\"\n"
	cases := []struct {
		name    string
		a, b    map[string]string
		stamped bool
	}{
		{"neither customizes", nil, nil, true},
		{"home rule missing here", map[string]string{".entire/settings.json": codename}, nil, false},
		{"home rule here too", map[string]string{".entire/settings.json": codename},
			map[string]string{".entire/settings.json": `{"redaction":{"custom_redactions":{"other-label":"zeta-[0-9]+"}}}`}, true},
		{"home pack rule missing here", map[string]string{".entire/redactors/internal.yaml": pack}, nil, false},
		{"home pack rule here too", map[string]string{".entire/redactors/internal.yaml": pack},
			map[string]string{".entire/redactors/internal.yaml": pack}, true},
		{"home PII off here", map[string]string{".entire/settings.json": `{"redaction":{"pii":{"enabled":true}}}`}, nil, false},
		{"home PII category off here", map[string]string{".entire/settings.json": `{"redaction":{"pii":{"enabled":true,"address":true}}}`},
			map[string]string{".entire/settings.json": `{"redaction":{"pii":{"enabled":true}}}`}, false},
		{"here redacts more", nil, map[string]string{".entire/settings.json": `{"redaction":{"pii":{"enabled":true}}}`}, true},
		{"home settings unreadable", map[string]string{".entire/settings.json": `{`}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := foreignCommitFixture(t, nil)
			for name, content := range tc.a {
				testutil.WriteFile(t, r.a, name, content)
			}
			for name, content := range tc.b {
				testutil.WriteFile(t, r.b, name, content)
			}

			stamped, notice := stampedOrNotice(t)
			assert.Equal(t, tc.stamped, stamped)
			if !tc.stamped {
				assert.Contains(t, notice, "redaction")
			}
		})
	}
}

// Turning the opt-in off between prepare-commit-msg and post-commit leaves
// the trailer without a checkpoint, as any refusal under A's lock does.
func TestPostCommit_ForeignOptInRecheckedUnderLock(t *testing.T) {
	r := foreignCommitFixture(t, nil)
	before := loadForeignTestState(t, r)
	msg := prepareForeign(t, "add b\n", commitSourceMessage)
	cpID, found := trailers.ParseCheckpoint(msg)
	require.True(t, found)
	r.setCrossRepoCommits(t, "off\n")
	testutil.GitCommit(t, r.b, msg)
	require.NoError(t, NewManualCommitStrategy().PostCommit(context.Background()))

	assert.Nil(t, readBCheckpoint(t, r, cpID))
	assert.Equal(t, before.CheckpointTranscriptStart, loadForeignTestState(t, r).CheckpointTranscriptStart)
}

// B's summary provider never sees A's transcript.
func TestPostCommit_ForeignAgentCommitSkipsSummary(t *testing.T) {
	r := foreignCommitFixture(t, nil)
	testutil.WriteFile(t, r.b, ".entire/settings.json", `{"strategy_options":{"summarize":{"enabled":true}}}`)
	require.True(t, settings.IsSummarizeEnabled(context.Background()), "control: B summarizes its own checkpoints")
	orig := generateCheckpointSummary
	asked := false
	generateCheckpointSummary = func(context.Context, redact.RedactedBytes, []string, *SessionState) *checkpoint.Summary {
		asked = true
		return nil
	}
	t.Cleanup(func() { generateCheckpointSummary = orig })

	cpID := commitForeign(t, r, "add b\n")
	require.NotNil(t, readBCheckpoint(t, r, cpID))
	assert.False(t, asked)
}

// B has a session of its own without content: prepare-commit-msg stamps for
// the foreign session, and post-commit, finding a local session that does
// not claim the trailer, still condenses the foreign one.
func TestPostCommit_ForeignAgentCommitBesideLocalSessionWithoutContent(t *testing.T) {
	r := foreignCommitFixture(t, nil)
	saveIdentitySession(t, "local-empty", func(st *SessionState) { st.WorktreePath = r.b })

	cpID := commitForeign(t, r, "add b\n")
	require.False(t, cpID.IsEmpty())
	content := readBCheckpoint(t, r, cpID)
	require.NotNil(t, content)
	assert.Equal(t, foreignTestSessionID, content.Metadata.SessionID)
}

// A squash inherits the squashed commits' checkpoint trailers: those link
// existing checkpoints, and no foreign trailer is added beside them.
func TestPrepareCommitMsg_ForeignAgentCommitSquashInherits(t *testing.T) {
	r := foreignCommitFixture(t, nil)
	testutil.RunGit(t, r.b, "checkout", "-b", "feature")
	cpID := commitForeign(t, r, "add b\n")
	require.NotNil(t, readBCheckpoint(t, r, cpID))
	testutil.RunGit(t, r.b, "checkout", "-")
	testutil.RunGit(t, r.b, "merge", "--squash", "feature")
	// More work in A's turn, so the session could link another commit.
	state := loadForeignTestState(t, r)
	f, err := os.OpenFile(state.TranscriptPath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"type":"human","message":{"content":"more"}}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	_, eligible := resolveForeignCallerSession(context.Background(), nil)
	require.True(t, eligible, "control: the session could link a new commit")

	got := prepareForeign(t, "squashed\n", "squash")
	assert.Equal(t, []id.CheckpointID{cpID}, trailers.ParseAllCheckpoints(got))
}

// A mutation of A's state nested in another of the same session would share
// a state pointer loaded from a different store; it is refused.
func TestMutateForeignSessionState_RefusesNested(t *testing.T) {
	r := foreignCommitFixture(t, nil)
	commonDir := filepath.Join(r.a, ".git")
	var inner error
	err := mutateForeignSessionState(context.Background(), r.aStore(t), commonDir, foreignTestSessionID, func(*SessionState) error {
		inner = mutateForeignSessionState(context.Background(), r.aStore(t), commonDir, foreignTestSessionID,
			func(*SessionState) error { return nil }, nil)
		return ErrMutationSkip
	}, nil)
	require.NoError(t, err)
	require.ErrorIs(t, inner, errNestedSessionMutation)
}
