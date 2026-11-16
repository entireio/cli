package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const foreignTestSessionID = "foreign-session-1"

// foreignRepos is the cross-repository shape: session state homed in repo A,
// the commit happening in repo B (the process CWD).
type foreignRepos struct {
	a, b string
}

// newForeignRepos creates A and B, isolates the per-user cache and the
// caller-session environment, and chdirs into B. Not parallel.
func newForeignRepos(t *testing.T) foreignRepos {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	clearCallerSessionEnv(t)

	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	r := foreignRepos{a: filepath.Join(base, "a"), b: filepath.Join(base, "b")}
	for _, dir := range []string{r.a, r.b} {
		require.NoError(t, os.MkdirAll(dir, 0o750))
		testutil.InitRepo(t, dir)
		testutil.WriteFile(t, dir, "init.txt", "init\n")
		testutil.GitAdd(t, dir, "init.txt")
		testutil.GitCommit(t, dir, "init")
	}
	t.Chdir(r.b)
	return r
}

func (r foreignRepos) aStore(t *testing.T) *session.StateStore {
	t.Helper()
	store, err := session.NewStateStoreForCommonDir(filepath.Join(r.a, ".git"))
	require.NoError(t, err)
	return store
}

// saveForeignState saves an eligible ACTIVE session with condensable content
// in A's store and indexes it, after letting mutate adjust it.
func (r foreignRepos) saveForeignState(t *testing.T, mutate func(*SessionState)) {
	t.Helper()
	r.saveForeignStateAs(t, foreignTestSessionID, mutate)
}

func (r foreignRepos) saveForeignStateAs(t *testing.T, sessionID string, mutate func(*SessionState)) {
	t.Helper()
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(`{"type":"user"}`+"\n"), 0o600))
	now := time.Now()
	state := &SessionState{
		SessionID:           sessionID,
		BaseCommit:          "abc1234",
		WorktreePath:        r.a,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseActive,
		AgentType:           agent.AgentTypeClaudeCode,
		TranscriptPath:      transcript,
	}
	if mutate != nil {
		mutate(state)
	}
	require.NoError(t, r.aStore(t).Save(context.Background(), state))
	require.NoError(t, session.RecordSessionHome(sessionHomeOf(state)))
}

func TestResolveForeignCallerSession_EnvClaim(t *testing.T) {
	r := newForeignRepos(t)
	r.saveForeignState(t, nil)

	_, ok := resolveForeignCallerSession(context.Background(), nil)
	assert.False(t, ok, "nothing names the session as the caller")

	t.Setenv("CLAUDE_CODE_SESSION_ID", foreignTestSessionID)
	fs, ok := resolveForeignCallerSession(context.Background(), nil)
	require.True(t, ok)
	assert.Equal(t, foreignTestSessionID, fs.state.SessionID)
	assert.True(t, sameDir(filepath.Join(r.a, ".git"), fs.commonDir))
}

func TestResolveForeignCallerSession_Ancestry(t *testing.T) {
	r := newForeignRepos(t)
	ancestry, ok := proclive.CurrentAncestry()
	if !ok || len(ancestry.Chain()) == 0 {
		t.Skip("platform cannot introspect processes")
	}
	owner := ancestry.Chain()[0]
	r.saveForeignState(t, func(s *SessionState) { s.Owner = &owner })

	fs, ok := resolveForeignCallerSession(context.Background(), nil)
	require.True(t, ok, "an owner in our ancestry identifies the session without any env claim")
	assert.Equal(t, foreignTestSessionID, fs.state.SessionID)
}

func TestResolveForeignCallerSession_Refused(t *testing.T) {
	cases := map[string]func(*SessionState){
		"ended":                  func(s *SessionState) { s.Phase = session.PhaseEnded },
		"imported":               func(s *SessionState) { s.Kind = session.KindImported },
		"adopted":                func(s *SessionState) { s.AdoptedIntoWorktreePath = "/elsewhere" },
		"idle without live task": func(s *SessionState) { s.Phase = session.PhaseIdle },
		"no content":             func(s *SessionState) { s.TranscriptPath = "" },
		"pending condensation": func(s *SessionState) {
			s.BeginCondensationAttempt(id.MustCheckpointID("111111111111"))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := newForeignRepos(t)
			r.saveForeignState(t, mutate)
			t.Setenv("CLAUDE_CODE_SESSION_ID", foreignTestSessionID)
			_, ok := resolveForeignCallerSession(context.Background(), nil)
			assert.False(t, ok)
		})
	}
}

// A record pointing at this repository is the local path's business.
func TestResolveForeignCallerSession_RecordForThisRepoIgnored(t *testing.T) {
	r := newForeignRepos(t)
	r.saveForeignState(t, nil)
	require.NoError(t, session.RecordSessionHome(session.SessionHome{SessionID: foreignTestSessionID, WorktreePath: r.b}))
	t.Setenv("CLAUDE_CODE_SESSION_ID", foreignTestSessionID)

	_, ok := resolveForeignCallerSession(context.Background(), nil)
	assert.False(t, ok)
}

// Nesting: an inner agent homed in B, nearer in our ancestry, ran us; the
// outer foreign session's variable was merely passed through.
func TestResolveForeignCallerSession_NearerLocalSessionWins(t *testing.T) {
	r := newForeignRepos(t)
	ancestry, ok := proclive.CurrentAncestry()
	if !ok || len(ancestry.Chain()) == 0 {
		t.Skip("platform cannot introspect processes")
	}
	nearest := ancestry.Chain()[0]
	r.saveForeignState(t, nil)
	t.Setenv("CLAUDE_CODE_SESSION_ID", foreignTestSessionID)
	now := time.Now()
	local := &SessionState{
		SessionID:           "local-inner",
		BaseCommit:          "abc1234",
		WorktreePath:        r.b,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseActive,
		Owner:               &nearest,
	}

	_, ok = resolveForeignCallerSession(context.Background(), []*SessionState{local})
	assert.False(t, ok, "the nearer B-homed session is the caller")
	_, ok = resolveForeignCallerSession(context.Background(), nil)
	assert.True(t, ok, "control: without the local session the foreign one resolves")
}

// Git exports GIT_DIR and GIT_INDEX_FILE to hooks; inherited, they would make
// the lookup of A's store answer for B.
func TestResolveForeignCallerSession_IgnoresHookRepoSelectors(t *testing.T) {
	r := newForeignRepos(t)
	r.saveForeignState(t, nil)
	t.Setenv("CLAUDE_CODE_SESSION_ID", foreignTestSessionID)
	t.Setenv("GIT_DIR", filepath.Join(r.b, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(r.b, ".git", "index"))

	fs, ok := resolveForeignCallerSession(context.Background(), nil)
	require.True(t, ok)
	assert.True(t, sameDir(filepath.Join(r.a, ".git"), fs.commonDir))
}

// Two sessions owned by the same agent process (a /clear, say): the one that
// interacted most recently is the caller, as the ranking decides locally.
func TestResolveForeignCallerSession_SameOwnerMostRecentWins(t *testing.T) {
	r := newForeignRepos(t)
	ancestry, ok := proclive.CurrentAncestry()
	if !ok || len(ancestry.Chain()) == 0 {
		t.Skip("platform cannot introspect processes")
	}
	owner := ancestry.Chain()[0]
	older := time.Now().Add(-time.Minute)
	r.saveForeignStateAs(t, "foreign-older", func(s *SessionState) {
		s.Owner = &owner
		s.LastInteractionTime = &older
	})
	r.saveForeignState(t, func(s *SessionState) { s.Owner = &owner })

	fs, ok := resolveForeignCallerSession(context.Background(), nil)
	require.True(t, ok)
	assert.Equal(t, foreignTestSessionID, fs.state.SessionID)
}
