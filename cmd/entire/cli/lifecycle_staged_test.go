package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestHandleLifecycleTurnEnd_CondenseDuringPublication(t *testing.T) {
	// Serial: lifecycle and strategy resolve the fixture from CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "init.txt", "initial")
	testutil.GitAdd(t, dir, "init.txt")
	testutil.GitCommit(t, dir, "initial")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	const sid = "stop-condense-race"
	ctx := t.Context()
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{SessionID: sid, Phase: session.PhaseIdle, StartedAt: time.Now(), StepCount: 1, FilesTouched: []string{"init.txt"}, AgentType: agent.AgentTypeClaudeCode}))
	metadata := paths.SessionMetadataDirFromSessionID(sid)
	staged := filepath.Join(dir, metadata, paths.TranscriptFileName)
	testutil.WriteFile(t, dir, "next.go", "package next")
	transcript := "{\"type\":\"human\",\"message\":{\"content\":\"next turn\"}}\n"
	native := filepath.Join(dir, "native.jsonl")
	testutil.WriteFile(t, dir, "native.jsonl", transcript)
	called := false
	ag := &mockAnalyzerAgent{mockLifecycleAgent: &mockLifecycleAgent{name: "mock-lifecycle", agentType: agent.AgentTypeClaudeCode, transcriptData: []byte(transcript)}, analyzerFiles: []string{"next.go"}}
	ag.onExtract = func() {
		if called {
			return
		}
		if _, err := os.Stat(staged); os.IsNotExist(err) {
			return
		}
		called = true
		require.FileExists(t, staged)
		require.NoError(t, (&strategy.ManualCommitStrategy{}).CondenseSessionByID(ctx, sid))
		require.FileExists(t, staged, "condensation must retain the active Stop's publication")
	}
	require.NoError(t, handleLifecycleTurnEnd(ctx, ag, &agent.Event{Type: agent.TurnEnd, SessionID: sid, SessionRef: native, Timestamp: time.Now()}))
	require.True(t, called)
	state, err := strategy.LoadSessionState(ctx, sid)
	require.NoError(t, err)
	require.Contains(t, state.FilesTouched, "next.go")
	require.FileExists(t, staged, "the later saved step retains its stored transcript")
}

func TestHandleLifecycleTurnEnd_ReleasesPublicationOnReturn(t *testing.T) {
	// Serial: each handler resolves its temporary repository from CWD.
	for _, readFails := range []bool{false, true} {
		name := "no pending files"
		if readFails {
			name = "transcript read error"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			testutil.WriteFile(t, dir, "init.txt", "initial")
			testutil.WriteFile(t, dir, "native.jsonl", `{ "type": "user", "message": "test" }`+"\n")
			testutil.GitAdd(t, dir, "init.txt", "native.jsonl")
			testutil.GitCommit(t, dir, "initial")
			t.Chdir(dir)
			paths.ClearWorktreeRootCache()
			t.Cleanup(paths.ClearWorktreeRootCache)
			ag := newMockAgent()
			if readFails {
				ag.transcriptErr = errors.New("read failed")
			}
			err := handleLifecycleTurnEnd(t.Context(), ag, &agent.Event{Type: agent.TurnEnd, SessionID: "release-on-return", SessionRef: filepath.Join(dir, "native.jsonl"), Timestamp: time.Now()})
			if readFails {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			release, err := strategy.AcquireSessionMetadataPublication(ctx, "release-on-return")
			require.NoError(t, err, "a returned Stop must not strand the publication lease")
			release()
		})
	}
}

type stagedPromptAgent struct {
	*mockLifecycleAgent

	onType func()
}

func (a *stagedPromptAgent) Type() types.AgentType {
	a.onType()
	return a.mockLifecycleAgent.Type()
}

func TestHandleLifecycleTurnStart_CondenseBeforeInitialization(t *testing.T) {
	// Serial: the handler and condensation resolve the fixture from CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "init.txt", "initial")
	testutil.GitAdd(t, dir, "init.txt")
	testutil.GitCommit(t, dir, "initial")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	const sid = "prompt-condense-race"
	ctx := t.Context()
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{SessionID: sid, BaseCommit: strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD")), Phase: session.PhaseIdle, StartedAt: time.Now(), StepCount: 1, FilesTouched: []string{"init.txt"}, AgentType: agent.AgentTypeClaudeCode}))
	transcript := "{\"type\":\"human\",\"message\":{\"content\":\"previous turn\"}}\n"
	testutil.WriteFile(t, dir, "native.jsonl", transcript)
	metadata := paths.SessionMetadataDirFromSessionID(sid)
	testutil.WriteFile(t, dir, filepath.Join(metadata, paths.TranscriptFileName), transcript)
	promptPath := filepath.Join(dir, metadata, paths.PromptFileName)
	called := false
	ag := &stagedPromptAgent{mockLifecycleAgent: &mockLifecycleAgent{name: "mock-lifecycle", agentType: agent.AgentTypeClaudeCode}}
	ag.onType = func() {
		if called {
			return
		}
		if _, err := os.Stat(promptPath); os.IsNotExist(err) {
			return
		}
		called = true
		require.NoError(t, (&strategy.ManualCommitStrategy{}).CondenseSessionByID(ctx, sid))
	}
	require.NoError(t, handleLifecycleTurnStart(ctx, ag, &agent.Event{Type: agent.TurnStart, SessionID: sid, SessionRef: filepath.Join(dir, "native.jsonl"), Prompt: "next turn prompt", Timestamp: time.Now()}))
	require.True(t, called, "fixture must condense after prompt publication and before initialization")
	data, err := os.ReadFile(promptPath)
	require.NoError(t, err, "the newer turn must retain its prompt")
	require.Contains(t, string(data), "next turn prompt")
}

func TestSessionMetadataPublication_HonorsTurnStartWait(t *testing.T) {
	// Serial: publication resolves the temporary repository from CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	const sid = "bounded-prompt-publication"
	release, err := strategy.AcquireSessionMetadataPublication(t.Context(), sid)
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	unexpectedRelease, err := strategy.AcquireSessionMetadataPublication(strategy.WithSessionLockWait(ctx, 10*time.Millisecond), sid)
	if err == nil {
		unexpectedRelease()
		t.Fatal("publication acquired a lease held by another hook")
	}
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, ctx.Err(), "publication must use TurnStart's budget before the caller's deadline")
}

func TestSessionMetadataPublication_ExpiredWaitCanTakeFreeLock(t *testing.T) {
	// Serial: a fresh CWD also ensures repository resolution is not cached.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	ctx := strategy.WithSessionLockWait(t.Context(), time.Nanosecond)
	release, err := strategy.AcquireSessionMetadataPublication(ctx, "free-expired-budget")
	require.NoError(t, err, "an elapsed wait budget must not cancel repository resolution for a free lock")
	release()
}
