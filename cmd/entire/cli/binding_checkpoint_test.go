package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// No test in this file may call t.Parallel: replay routing uses process env and
// the package-level runner seam so the real subprocess is never launched.

func TestBindingTurnCollector_MostRecentSuccessIsPrimary(t *testing.T) {
	c := newBindingTurnCollector()
	repoA := binding.Evidence{Repo: binding.RepoIdentity{CommonDir: "/a/.git", WorktreeRoot: "/a"}, Enabled: true}
	repoB := binding.Evidence{Repo: binding.RepoIdentity{CommonDir: "/b/.git", WorktreeRoot: "/b"}, Enabled: true}

	c.recordSuccessfulReplay(repoA)
	c.recordSuccessfulReplay(repoB)
	c.recordSuccessfulReplay(repoA)

	targets, primary := c.replaySnapshot()
	if primary != repoA.Repo.CommonDir {
		t.Fatalf("primary = %q, want most recently active %q", primary, repoA.Repo.CommonDir)
	}
	if len(targets) != 2 || targets[0].Repo.CommonDir != repoB.Repo.CommonDir || targets[1].Repo.CommonDir != repoA.Repo.CommonDir {
		t.Fatalf("targets = %+v, want last-activity order [B A]", targets)
	}
}

func TestReplayBindingTurn_PrimaryFlagAndRawPayload(t *testing.T) {
	t.Setenv(bindingReplayEnv, "")
	c := newBindingTurnCollector()
	repoA := binding.Evidence{Repo: binding.RepoIdentity{CommonDir: "/a/.git", WorktreeRoot: "/a"}, Enabled: true}
	repoB := binding.Evidence{Repo: binding.RepoIdentity{CommonDir: "/b/.git", WorktreeRoot: "/b"}, Enabled: true}
	c.recordSuccessfulReplay(repoA)
	c.recordSuccessfulReplay(repoB)

	original := runBindingReplayHook
	t.Cleanup(func() { runBindingReplayHook = original })
	type call struct {
		root    string
		primary bool
		payload string
	}
	var calls []call
	var callsMu sync.Mutex
	runBindingReplayHook = func(_ context.Context, targetRoot string, _ string, _ string, payload []byte, primary bool) error {
		callsMu.Lock()
		defer callsMu.Unlock()
		calls = append(calls, call{root: targetRoot, primary: primary, payload: string(payload)})
		return nil
	}

	replayBindingTurn(context.Background(), c, "claude-code", "stop", []byte(`{"session_id":"s"}`))

	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want two repo replays", calls)
	}
	primaryByRoot := make(map[string]bool, len(calls))
	for _, got := range calls {
		primaryByRoot[got.root] = got.primary
		if got.payload != `{"session_id":"s"}` {
			t.Fatalf("payload changed for %s: %q", got.root, got.payload)
		}
	}
	if primaryByRoot[repoA.Repo.WorktreeRoot] || !primaryByRoot[repoB.Repo.WorktreeRoot] {
		t.Fatalf("primary flags = %+v, want A=false B=true", primaryByRoot)
	}
}

func TestReplayBindingTurn_RecursionSentinelSkipsFanout(t *testing.T) {
	t.Setenv(bindingReplayEnv, "1")
	c := newBindingTurnCollector()
	c.recordSuccessfulReplay(binding.Evidence{Repo: binding.RepoIdentity{CommonDir: "/a/.git", WorktreeRoot: "/a"}, Enabled: true})

	original := runBindingReplayHook
	t.Cleanup(func() { runBindingReplayHook = original })
	called := false
	runBindingReplayHook = func(context.Context, string, string, string, []byte, bool) error {
		called = true
		return nil
	}

	replayBindingTurn(context.Background(), c, "claude-code", "stop", []byte(`{}`))
	if called {
		t.Fatal("replayed child recursively fanned out")
	}
}

func TestSelectBindingTurnPrimary_UsesLatestEligiblePath(t *testing.T) {
	t.Setenv(bindingReplayEnv, "")
	rootA := newBindingRepo(t)
	rootB := newBindingRepo(t)
	c := newBindingTurnCollector()
	repoB, ok := binding.ResolveRepoForPath(context.Background(), filepath.Join(rootB, ".git"))
	if !ok {
		t.Fatal("resolve repo B")
	}
	c.recordSuccessfulReplay(binding.Evidence{Repo: repoB, Enabled: true})

	primary := selectBindingTurnPrimary(context.Background(), c, rootA,
		[]string{filepath.Join(rootB, "b.go"), filepath.Join(rootA, "a.go")}, true)
	rootAIdentity, ok := binding.ResolveRepoForPath(context.Background(), filepath.Join(rootA, ".git"))
	if !ok {
		t.Fatal("resolve repo A")
	}
	if primary != rootAIdentity.CommonDir || !bindingTurnKeepsTokenUsage(context.WithValue(context.Background(), bindingTurnCollectorKey{}, c), rootAIdentity.CommonDir) {
		t.Fatalf("latest local edit should keep parent token usage: primary=%q current=%q", primary, rootAIdentity.CommonDir)
	}

	primary = selectBindingTurnPrimary(context.Background(), c, rootA,
		[]string{filepath.Join(rootA, "a.go"), filepath.Join(rootB, "b.go")}, true)
	if primary != repoB.CommonDir || bindingTurnKeepsTokenUsage(context.WithValue(context.Background(), bindingTurnCollectorKey{}, c), rootAIdentity.CommonDir) {
		t.Fatalf("latest foreign edit should suppress parent token usage: primary=%q foreign=%q", primary, repoB.CommonDir)
	}
}

func TestFilterBindingReplayNewFiles_RequiresTranscriptEvidence(t *testing.T) {
	got := filterBindingReplayNewFiles(
		[]string{"agent-new.go", "preexisting.tmp", "also-agent.txt"},
		[]string{"also-agent.txt", "tracked.go", "agent-new.go"},
	)
	want := []string{"agent-new.go", "also-agent.txt"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("filtered new files = %v, want %v", got, want)
	}
}

func TestReplayedTurnEnd_CheckpointsOnlyEvidencedFiles(t *testing.T) {
	t.Setenv(bindingReplayEnv, "1")
	t.Setenv(bindingReplayPrimaryEnv, "1")
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	root := newBindingRepo(t)
	testutil.WriteFile(t, root, "tracked.txt", "base\n")
	testutil.GitAdd(t, root, "tracked.txt")
	testutil.GitCommit(t, root, "initial")
	testutil.WriteFile(t, root, "preexisting.tmp", "leave me alone\n")
	testutil.WriteFile(t, root, "agent.go", "package agent\n")
	t.Chdir(root)
	paths.ClearWorktreeRootCache()

	transcriptPath := filepath.Join(root, "transcript.jsonl")
	transcriptData := []byte(`{"type":"user","message":"write agent.go"}` + "\n")
	if err := os.WriteFile(transcriptPath, transcriptData, 0o600); err != nil {
		t.Fatal(err)
	}
	sessionID := "binding-replay-checkpoint"
	now := time.Now()
	state := &session.State{
		SessionID:             sessionID,
		BaseCommit:            testutil.GetHeadHash(t, root),
		AttributionBaseCommit: testutil.GetHeadHash(t, root),
		WorktreePath:          root,
		StartedAt:             now.Add(-time.Minute),
		LastInteractionTime:   &now,
		Phase:                 session.PhaseActive,
		AgentType:             types.AgentType("Mock Analyzer Agent"),
		TranscriptPath:        transcriptPath,
		FilesTouched:          []string{"agent.go"},
		UntrackedFilesAtStart: []string{"agent.go", "preexisting.tmp"},
	}
	store := session.NewStateStoreWithDir(filepath.Join(root, ".git", session.SessionStateDirName))
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}

	ag := &mockAnalyzerAgent{
		mockLifecycleAgent: &mockLifecycleAgent{
			name:           "mock-analyzer",
			agentType:      "Mock Analyzer Agent",
			transcriptData: transcriptData,
		},
		analyzerFiles: []string{filepath.Join(root, "agent.go")},
	}
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  now,
	}
	if err := handleLifecycleTurnEnd(context.Background(), ag, event); err != nil {
		t.Fatal(err)
	}

	got, err := strategy.LoadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.StepCount != 1 {
		t.Fatalf("replayed state = %+v, want one checkpointed step", got)
	}
	if !slices.Contains(got.FilesTouched, "agent.go") || slices.Contains(got.FilesTouched, "preexisting.tmp") {
		t.Fatalf("replayed files touched = %v, want agent.go without preexisting.tmp", got.FilesTouched)
	}
	if slices.Contains(got.UntrackedFilesAtStart, "agent.go") || !slices.Contains(got.UntrackedFilesAtStart, "preexisting.tmp") {
		t.Fatalf("replayed untracked baseline = %v, want only the pre-existing file", got.UntrackedFilesAtStart)
	}
}

func TestCappedBuffer_KeepsPrefixAndReportsFullWrite(t *testing.T) {
	t.Parallel()
	b := &cappedBuffer{limit: 5}
	n, err := b.Write([]byte("hello world"))
	if err != nil || n != len("hello world") {
		t.Fatalf("Write = %d, %v; must report the whole write consumed", n, err)
	}
	if _, err := b.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	if b.String() != "hello" {
		t.Fatalf("kept %q, want the first 5 bytes", b.String())
	}
}
