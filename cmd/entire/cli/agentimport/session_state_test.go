package agentimport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// fakeImporter is the minimal Importer needed to exercise writeSessionState.
type fakeImporter struct{}

func (fakeImporter) Name() string               { return string(agent.AgentNameClaudeCode) }
func (fakeImporter) AgentType() types.AgentType { return agent.AgentTypeClaudeCode }
func (fakeImporter) Discover(_, _ string, _ time.Time, _ []string) ([]SessionFile, error) {
	return nil, nil
}
func (fakeImporter) SplitTurns(_ SessionFile, _ []byte) ([]Turn, error) { return nil, nil }

// runFakeImporter feeds canned Discover/SplitTurns results so Run's full
// per-session path (including the writeSessionState call site) can be exercised.
type runFakeImporter struct {
	files []SessionFile
	turns []Turn
}

func (runFakeImporter) Name() string               { return string(agent.AgentNameClaudeCode) }
func (runFakeImporter) AgentType() types.AgentType { return agent.AgentTypeClaudeCode }
func (f runFakeImporter) Discover(_, _ string, _ time.Time, _ []string) ([]SessionFile, error) {
	return f.files, nil
}
func (f runFakeImporter) SplitTurns(_ SessionFile, _ []byte) ([]Turn, error) { return f.turns, nil }

// importRepo creates an isolated git repo and chdirs into it so session-state
// resolution (git common dir from cwd) targets it. Callers must not use
// t.Parallel (t.Chdir).
func importRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	return dir
}

// loadState reads a session state by id from the current repo's store.
func loadState(t *testing.T, sid string) *session.State {
	t.Helper()
	ctx := context.Background()
	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}
	st, err := store.Load(ctx, sid)
	if err != nil {
		t.Fatalf("Load %s: %v", sid, err)
	}
	return st
}

func TestRun_WritesSessionStateExceptDryRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dryRun    bool
		wantState bool
	}{
		{"writes imported session state", false, true},
		{"dry-run writes nothing", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := importRepo(t)
			testutil.WriteFile(t, dir, "f.txt", "x")
			testutil.GitAdd(t, dir, "f.txt")
			testutil.GitCommit(t, dir, "init")
			transcript := filepath.Join(dir, "session.jsonl")
			if err := os.WriteFile(transcript, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
				t.Fatalf("write transcript: %v", err)
			}
			repo, err := git.PlainOpen(dir)
			if err != nil {
				t.Fatalf("open repo: %v", err)
			}

			ctx := context.Background()
			const sid = "claude-run-session"
			imp := runFakeImporter{
				files: []SessionFile{{Path: transcript, SessionID: sid}},
				turns: []Turn{{UUID: "a", Prompt: "hello", CreatedAt: time.Now().Add(-time.Hour)}},
			}
			if _, err := Run(ctx, repo, imp, Options{RepoRoot: dir, Now: time.Now(), DryRun: tc.dryRun}); err != nil {
				t.Fatalf("Run: %v", err)
			}

			st := loadState(t, sid)
			switch {
			case tc.wantState && (st == nil || st.Kind != session.KindImported):
				t.Fatalf("want imported session state, got %+v", st)
			case !tc.wantState && st != nil:
				t.Fatalf("dry-run must not write session state, got %+v", st)
			}
		})
	}
}

func TestWriteSessionState_CreatesListableImportedState(t *testing.T) {
	importRepo(t)
	ctx := context.Background()
	started := time.Now().Add(-48 * time.Hour)
	ended := started.Add(30 * time.Minute)
	sf := SessionFile{Path: "session.jsonl", SessionID: "claude-basic-session"}
	turns := []Turn{
		{UUID: "a", Prompt: "opening prompt", Model: "claude-x", CreatedAt: started, Tokens: &types.TokenUsage{InputTokens: 10, OutputTokens: 5}},
		{UUID: "b", Prompt: "latest prompt", Model: "claude-x", CreatedAt: ended, Tokens: &types.TokenUsage{InputTokens: 3, OutputTokens: 2}},
	}

	if err := writeSessionState(ctx, fakeImporter{}, sf, turns); err != nil {
		t.Fatalf("writeSessionState: %v", err)
	}

	st := loadState(t, sf.SessionID)
	if st == nil {
		t.Fatal("no session state written")
	}
	if st.Kind != session.KindImported {
		t.Errorf("Kind = %q, want %q", st.Kind, session.KindImported)
	}
	if st.BaseCommit != "" {
		t.Errorf("BaseCommit = %q, want empty (never HEAD-pinned)", st.BaseCommit)
	}
	if st.AgentType != agent.AgentTypeClaudeCode {
		t.Errorf("AgentType = %q, want %q", st.AgentType, agent.AgentTypeClaudeCode)
	}
	if !st.StartedAt.Equal(started) || st.EndedAt == nil || !st.EndedAt.Equal(ended) {
		t.Errorf("timestamps = [%v, %v], want [%v, %v] (earliest/latest turn)", st.StartedAt, st.EndedAt, started, ended)
	}
	if st.StepCount != 2 {
		t.Errorf("StepCount = %d, want 2", st.StepCount)
	}
	if got := sessionTokenTotal(st); got != 20 {
		t.Errorf("token total = %d, want 20", got)
	}
	if st.LastPrompt != "latest prompt" {
		t.Errorf("LastPrompt = %q, want the most recent turn's prompt", st.LastPrompt)
	}
}

func TestWriteSessionState_CollapsesAndTruncatesLastPrompt(t *testing.T) {
	importRepo(t)
	ctx := context.Background()

	longPrompt := "please   fix\n\n\tthe   login   bug " + strings.Repeat("x", 300)
	sf := SessionFile{Path: "session.jsonl", SessionID: "claude-long-prompt-session"}
	if err := writeSessionState(ctx, fakeImporter{}, sf, []Turn{{UUID: "a", Prompt: longPrompt, CreatedAt: time.Now()}}); err != nil {
		t.Fatalf("writeSessionState: %v", err)
	}

	got := loadState(t, sf.SessionID).LastPrompt
	if n := utf8.RuneCountInString(got); n > session.MaxLastPromptRunes {
		t.Errorf("LastPrompt rune count = %d, want <= %d", n, session.MaxLastPromptRunes)
	}
	if strings.ContainsAny(got, "\n\t") || strings.Contains(got, "  ") {
		t.Errorf("LastPrompt not whitespace-collapsed: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("LastPrompt should be truncated with ellipsis: %q", got)
	}
}

func TestWriteSessionState_DoesNotClobberLiveSession(t *testing.T) {
	importRepo(t)
	ctx := context.Background()

	const sid = "claude-live-session"
	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}
	if err := store.Save(ctx, &session.State{SessionID: sid, Phase: session.PhaseActive, StartedAt: time.Now()}); err != nil {
		t.Fatalf("seed live state: %v", err)
	}

	sf := SessionFile{Path: "session.jsonl", SessionID: sid}
	if err := writeSessionState(ctx, fakeImporter{}, sf, []Turn{{UUID: "a", Prompt: "p", CreatedAt: time.Now()}}); err != nil {
		t.Fatalf("writeSessionState: %v", err)
	}

	got := loadState(t, sid)
	if got == nil || got.Kind == session.KindImported || got.Phase != session.PhaseActive {
		t.Fatalf("import clobbered a live session: %+v", got)
	}
}

func TestWriteSessionState_SurvivesListingWhenOld(t *testing.T) {
	importRepo(t)
	ctx := context.Background()

	old := time.Now().Add(-30 * 24 * time.Hour) // 30 days > 7-day stale threshold
	sf := SessionFile{Path: "session.jsonl", SessionID: "claude-old-session"}
	if err := writeSessionState(ctx, fakeImporter{}, sf, []Turn{{UUID: "a", Prompt: "p", CreatedAt: old}}); err != nil {
		t.Fatalf("writeSessionState: %v", err)
	}

	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		t.Fatalf("ListSessionStates: %v", err)
	}
	for _, s := range states {
		if s.SessionID == sf.SessionID {
			return
		}
	}
	t.Fatal("30-day-old imported session was not returned by ListSessionStates")
}

func sessionTokenTotal(s *session.State) int {
	if s.TokenUsage == nil {
		return 0
	}
	return s.TokenUsage.InputTokens + s.TokenUsage.OutputTokens +
		s.TokenUsage.CacheCreationTokens + s.TokenUsage.CacheReadTokens
}
