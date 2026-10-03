package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func canonicalAgentHomeForTest(t *testing.T, home string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	return canonical
}

// TestInitializeSession_RecordsAgentHome_IndependentOfWorktreeCwd verifies
// that AgentHome is recorded as the agent's per-user state directory
// (AgentHomeProvider.SessionHome), not as GetSessionDir(worktreeRoot).
//
// Claude Code names a session's project directory after the cwd the agent
// was actually launched from (SanitizePathForClaude(launchCwd)), which need
// not match the worktree root initializeSession resolves via
// paths.WorktreeRoot. Recording GetSessionDir(worktreeRoot) as "home" would
// therefore record a directory that does not contain this session's
// transcript at all — this test fails under that regression.
func TestInitializeSession_RecordsAgentHome_IndependentOfWorktreeCwd(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	configDir := canonicalAgentHomeForTest(t, t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")

	// launchCwd stands in for "wherever the agent process actually ran from",
	// deliberately different from the worktree root (dir) so the two
	// candidate "homes" diverge.
	launchCwd := t.TempDir()
	transcriptPath := filepath.Join(configDir, "projects", claudecode.SanitizePathForClaude(launchCwd), "session-abc.jsonl")

	s := &ManualCommitStrategy{}
	sessionID := "test-session-cwd-mismatch"
	require.NoError(t, s.InitializeSession(context.Background(), sessionID, agent.AgentTypeClaudeCode, transcriptPath, "", ""))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Equal(t, configDir, state.AgentHome,
		"AgentHome must be the resolved CLAUDE_CONFIG_DIR (the true home), not a session-specific project subdirectory")

	claude := &claudecode.ClaudeCodeAgent{}
	assert.True(t, claude.SessionPathUnder(state.AgentHome, state.TranscriptPath),
		"recorded AgentHome must contain the transcript even though it sits under a cwd-derived project dir different from the worktree root")

	// Sanity check for the regression this test guards against: the worktree
	// root's own session dir must NOT contain this transcript, since the
	// transcript was launched from a different cwd. If AgentHome were ever
	// swapped for GetSessionDir(worktreeRoot), the assertion above would fail.
	wrongHome, err := claude.GetSessionDir(dir)
	require.NoError(t, err)
	assert.False(t, claude.SessionPathUnder(wrongHome, state.TranscriptPath),
		"sanity check: GetSessionDir(worktreeRoot) must not contain a transcript launched from a different cwd")
}

func TestInitializeSession_PreservesHomeAfterTranscriptRedirection(t *testing.T) {
	for _, redirect := range []string{"leaf", "directory"} {
		t.Run(redirect, func(t *testing.T) {
			dir := setupGitRepo(t)
			t.Chdir(dir)
			home := canonicalAgentHomeForTest(t, t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
			name := filepath.Join("projects", "proj", "session.jsonl")
			testutil.WriteFile(t, home, name, "{}\n")
			path := filepath.Join(home, name)
			s := &ManualCommitStrategy{}
			const sessionID = "test-home-redirection"
			require.NoError(t, s.InitializeSession(context.Background(), sessionID, agent.AgentTypeClaudeCode, path, "", ""))

			secretHome := t.TempDir()
			testutil.WriteFile(t, secretHome, "session.jsonl", "private")
			link, target := path, filepath.Join(secretHome, "session.jsonl")
			require.NoError(t, os.Remove(path))
			if redirect == "directory" {
				link, target = filepath.Dir(path), secretHome
				require.NoError(t, os.Remove(link))
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}
			require.NoError(t, s.InitializeSession(context.Background(), sessionID, agent.AgentTypeClaudeCode, path, "", ""))
			state, err := s.loadSessionState(context.Background(), sessionID)
			require.NoError(t, err)
			require.Equal(t, home, state.AgentHome)
			_, err = agent.ReadTranscriptFileUnderHome(state.TranscriptPath, state.AgentHome)
			require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
		})
	}
}

// TestInitializeSession_TurnStartGuard_DoesNotClobberAgentHomeFromDifferentInstance
// verifies the guard in the turn-start refresh path: a hook firing under a
// different resolved home (e.g. a second CLAUDE_CONFIG_DIR instance of the
// same agent) must not overwrite an AgentHome record that still contains the
// session's transcript.
func TestInitializeSession_TurnStartGuard_DoesNotClobberAgentHomeFromDifferentInstance(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	dirA := canonicalAgentHomeForTest(t, t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", dirA)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")

	transcriptPath := filepath.Join(dirA, "projects", claudecode.SanitizePathForClaude(dir), "session-xyz.jsonl")

	s := &ManualCommitStrategy{}
	sessionID := "test-session-turn-start-guard"
	require.NoError(t, s.InitializeSession(context.Background(), sessionID, agent.AgentTypeClaudeCode, transcriptPath, "first prompt", ""))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, dirA, state.AgentHome)

	// A second turn fires under a DIFFERENT instance of Claude Code (a second
	// wclaude/pclaude-style CLAUDE_CONFIG_DIR). The transcript path supplied
	// by the hook is unchanged — it still names the file the owning instance
	// (dirA) wrote — so the newly resolved dirB must not overwrite the
	// correct recorded home.
	dirB := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dirB)

	require.NoError(t, s.InitializeSession(context.Background(), sessionID, agent.AgentTypeClaudeCode, transcriptPath, "second prompt", ""))

	state2, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, dirA, state2.AgentHome,
		"turn-start refresh under a different instance's home must not clobber a record that still contains the transcript")
}

// TestRefreshAgentHome_ClearsHomeAfterAgentTypeCorrection covers the
// Claude-to-Cursor repair: the turn start rewrites AgentType and
// TranscriptPath, so a Claude home left behind would make every confined read
// of the Cursor transcript fail.
func TestInitializeSession_RefreshRegistersOnlyIndependentHome(t *testing.T) {
	for _, activeMatches := range []bool{true, false} {
		t.Run(map[bool]string{true: "pre-upgrade", false: "recorded-home-is-not-authority"}[activeMatches], func(t *testing.T) {
			t.Chdir(setupGitRepo(t))
			home := canonicalAgentHomeForTest(t, t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
			t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
			path := filepath.Join(home, "projects", "proj", "session.jsonl")
			s := &ManualCommitStrategy{}
			ctx := context.Background()
			require.NoError(t, s.InitializeSession(ctx, "test-refresh-registry", agent.AgentTypeClaudeCode, path, "", ""))
			state, err := s.loadSessionState(ctx, "test-refresh-registry")
			require.NoError(t, err)
			if activeMatches {
				state.AgentHome = "" // session written before AgentHome existed
			} else {
				t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			}
			require.NoError(t, s.saveSessionState(ctx, state))
			// Simulate a home that has not yet been recorded in this installation.
			t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
			require.NoError(t, s.InitializeSession(ctx, state.SessionID, agent.AgentTypeClaudeCode, path, "", ""))
			if activeMatches {
				require.Equal(t, []string{home}, agent.KnownAgentHomes(agent.AgentTypeClaudeCode))
			} else {
				require.Empty(t, agent.KnownAgentHomes(agent.AgentTypeClaudeCode))
			}
		})
	}
}

func TestInitializeSession_ProvisionalHomeAllowsLinkedTranscriptDirectory(t *testing.T) {
	t.Chdir(setupGitRepo(t))
	home := canonicalAgentHomeForTest(t, t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	s := &ManualCommitStrategy{}
	ctx := context.Background()
	require.NoError(t, s.InitializeSession(ctx, "test-provisional-home", agent.AgentTypeClaudeCode, "", "", ""))
	state, err := s.loadSessionState(ctx, "test-provisional-home")
	require.NoError(t, err)
	require.Equal(t, home, state.AgentHome)

	projects := t.TempDir()
	testutil.WriteFile(t, projects, "proj/session.jsonl", "{}\n")
	testutil.SkipWithoutSymlinks(t)
	require.NoError(t, os.Symlink(projects, filepath.Join(home, "projects")))
	path := filepath.Join(home, "projects", "proj", "session.jsonl")
	require.NoError(t, s.InitializeSession(ctx, state.SessionID, agent.AgentTypeClaudeCode, path, "", ""))
	state, err = s.loadSessionState(ctx, state.SessionID)
	require.NoError(t, err)
	require.Empty(t, state.AgentHome)
	data, err := agent.ReadTranscriptFileUnderHome(state.TranscriptPath, state.AgentHome)
	require.NoError(t, err)
	require.Equal(t, "{}\n", string(data))
}

func TestInitializeSession_PartialRepairPreservesBoundary(t *testing.T) {
	for _, agentType := range []types.AgentType{agent.AgentTypeCodex, agent.AgentTypeClaudeCode} {
		t.Run(string(agentType), func(t *testing.T) {
			t.Chdir(setupGitRepo(t))
			home := canonicalAgentHomeForTest(t, t.TempDir())
			t.Setenv("CODEX_HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", "")
			t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
			t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
			path := filepath.Join(home, "sessions", "2026", "01", "01", "rollout-session.jsonl")
			if agentType == agent.AgentTypeClaudeCode {
				path = filepath.Join(home, "projects", "project", "session.jsonl")
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
			s := &ManualCommitStrategy{}
			state := &SessionState{SessionID: "test-partial-boundary", StartedAt: time.Now(), AgentType: agentType, AgentHome: home, TranscriptPath: path}
			require.NoError(t, s.saveSessionState(context.Background(), state))
			outside := t.TempDir()
			testutil.WriteFile(t, outside, "private.jsonl", "private\n")
			testutil.SkipWithoutSymlinks(t)
			require.NoError(t, os.Symlink(filepath.Join(outside, "private.jsonl"), path))
			before, loadErr := s.loadSessionState(context.Background(), state.SessionID)
			require.NoError(t, loadErr)
			require.NotNil(t, before)
			require.Equal(t, home, before.AgentHome)
			require.NoError(t, s.InitializeSession(context.Background(), state.SessionID, agentType, path, "", ""))
			state, err := s.loadSessionState(context.Background(), state.SessionID)
			require.NoError(t, err)
			require.Equal(t, home, state.AgentHome)
			_, err = agent.ReadTranscriptFileUnderHome(state.TranscriptPath, state.AgentHome)
			require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
			require.Empty(t, agent.KnownAgentHomes(agentType), "repair must not grant trust to a retained metadata home")
		})
	}
}

func TestInitializeSession_NewHomeAliasCannotDowngradeBoundary(t *testing.T) {
	for _, replacement := range []string{"linked-leaf", "regular"} {
		t.Run(replacement, func(t *testing.T) {
			t.Chdir(setupGitRepo(t))
			home := canonicalAgentHomeForTest(t, t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
			t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
			name := filepath.Join("projects", "project", "session.jsonl")
			testutil.WriteFile(t, home, name, "{}\n")
			s := &ManualCommitStrategy{}
			ctx := context.Background()
			const sessionID = "test-changed-home-alias"
			require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, filepath.Join(home, name), "", ""))
			otherHome := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(otherHome, filepath.Dir(name)), 0o750))
			testutil.SkipWithoutSymlinks(t)
			if replacement == "linked-leaf" {
				outside := t.TempDir()
				testutil.WriteFile(t, outside, "private.jsonl", "private\n")
				require.NoError(t, os.Symlink(filepath.Join(outside, "private.jsonl"), filepath.Join(otherHome, name)))
			} else {
				testutil.WriteFile(t, otherHome, name, "{}\n")
			}
			alias := filepath.Join(t.TempDir(), "home")
			require.NoError(t, os.Symlink(otherHome, alias))
			t.Setenv("CLAUDE_CONFIG_DIR", alias)
			for range 2 {
				err := s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, filepath.Join(alias, name), "", "")
				if replacement == "linked-leaf" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			}
			state, err := s.loadSessionState(ctx, sessionID)
			require.NoError(t, err)
			require.NotEmpty(t, state.AgentHome, "a repeated turn must not erase the boundary")
			data, err := agent.ReadTranscriptFileUnderHome(state.TranscriptPath, state.AgentHome)
			if replacement == "linked-leaf" {
				require.Equal(t, home, state.AgentHome)
				require.Equal(t, filepath.Join(home, name), state.TranscriptPath)
			}
			require.NoError(t, err)
			require.Equal(t, "{}\n", string(data))
		})
	}
}

func TestRefreshAgentHome_ClearsHomeAfterAgentTypeCorrection(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		AgentType:      agent.AgentTypeCursor,
		AgentHome:      t.TempDir(),
		TranscriptPath: filepath.Join(t.TempDir(), "agent-transcripts", "s.jsonl"),
	}
	refreshAgentHome(state)
	assert.Empty(t, state.AgentHome)
}

func TestInitializeSession_SnapshotsLinkedHome(t *testing.T) {
	testutil.SkipWithoutSymlinks(t)
	dir := setupGitRepo(t)
	t.Chdir(dir)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	alias := filepath.Join(t.TempDir(), "active-home")
	require.NoError(t, os.Symlink(home, alias))
	t.Setenv("CLAUDE_CONFIG_DIR", alias)
	name := filepath.Join("projects", claudecode.SanitizePathForClaude(dir), "session.jsonl")
	testutil.WriteFile(t, home, name, "original\n")
	s := &ManualCommitStrategy{}
	require.NoError(t, s.InitializeSession(t.Context(), "session", agent.AgentTypeClaudeCode, filepath.Join(alias, name), "", ""))
	state, err := s.loadSessionState(t.Context(), "session")
	require.NoError(t, err)
	require.Equal(t, home, state.AgentHome)
	require.Equal(t, filepath.Join(home, name), state.TranscriptPath)
	require.NoError(t, os.Remove(alias))
	other := t.TempDir()
	require.NoError(t, os.Symlink(other, alias))
	testutil.WriteFile(t, other, name, "replacement\n")
	refreshAgentHome(state)
	data, err := agent.ReadTranscriptFileUnderHome(state.TranscriptPath, state.AgentHome)
	require.NoError(t, err)
	require.Equal(t, "original\n", string(data))
}

func TestRefreshAgentHome_LinkedHomeCannotDowngradeCanonicalBoundary(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	linkedHome := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, linkedHome); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", linkedHome)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	name := filepath.Join("projects", "proj", "session.jsonl")
	testutil.WriteFile(t, home, "projects/proj/target.jsonl", "private")
	if err := os.Symlink("target.jsonl", filepath.Join(home, name)); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	// Adoption stores canonical coordinates, but the next hook can spell the
	// same transcript through the agent's linked home.
	state := &SessionState{AgentType: agent.AgentTypeClaudeCode, AgentHome: home, TranscriptPath: filepath.Join(linkedHome, name)}
	refreshAgentHome(state)
	require.Equal(t, home, state.AgentHome)
	require.Equal(t, filepath.Join(home, name), state.TranscriptPath)
	_, err = agent.ReadTranscriptFileUnderHome(state.TranscriptPath, state.AgentHome)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
}

// TestRefreshAgentHome_ClearsHomeNotContainingTranscript covers a home
// recorded before any transcript path was known: when the path that arrives
// lies under neither the active nor the recorded home, the record is dropped
// and the session keeps the legacy read protocol.
func TestRefreshAgentHome_ClearsHomeNotContainingTranscript(t *testing.T) {
	active := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", active)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")

	state := &SessionState{
		AgentType:      agent.AgentTypeClaudeCode,
		AgentHome:      active,
		TranscriptPath: filepath.Join(t.TempDir(), "override", "s.jsonl"),
	}
	refreshAgentHome(state)
	assert.Empty(t, state.AgentHome)

	// The provisional record is kept while no transcript path is known.
	state = &SessionState{AgentType: agent.AgentTypeClaudeCode, AgentHome: active}
	refreshAgentHome(state)
	assert.Equal(t, active, state.AgentHome)
}

// TestInitializeSession_DoesNotRecordHomeBehindLinkedProjectsDir covers a
// relocated ~/.claude/projects. Confined reads refuse links below the home,
// so recording it would break every later transcript read; the session keeps
// the legacy read protocol instead.
func TestInitializeSession_DoesNotRecordHomeBehindLinkedProjectsDir(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	configDir := canonicalAgentHomeForTest(t, t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	if err := os.Symlink(t.TempDir(), filepath.Join(configDir, "projects")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	project := filepath.Join(configDir, "projects", claudecode.SanitizePathForClaude(dir))
	require.NoError(t, os.MkdirAll(project, 0o750))
	transcriptPath := filepath.Join(project, "session-linked.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte("{}\n"), 0o600))

	s := &ManualCommitStrategy{}
	sessionID := "test-session-linked-projects"
	require.NoError(t, s.InitializeSession(context.Background(), sessionID, agent.AgentTypeClaudeCode, transcriptPath, "", ""))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Empty(t, state.AgentHome)
}
