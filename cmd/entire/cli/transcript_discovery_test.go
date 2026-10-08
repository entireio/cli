package cli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// homeLayoutEnv names, for each agent with a home layout, the variable that
// relocates its home and the test overrides that would bypass it.
var homeLayoutEnv = map[types.AgentType]struct {
	home      string
	overrides []string
}{
	agent.AgentTypeClaudeCode:     {home: "CLAUDE_CONFIG_DIR", overrides: []string{"ENTIRE_TEST_CLAUDE_PROJECT_DIR"}},
	agent.AgentTypeCodex:          {home: "CODEX_HOME", overrides: []string{"ENTIRE_TEST_CODEX_SESSION_DIR"}},
	agent.AgentTypeCopilotCLI:     {home: "COPILOT_HOME", overrides: []string{"ENTIRE_TEST_COPILOT_SESSION_DIR"}},
	agent.AgentTypeFactoryAIDroid: {home: "FACTORY_HOME_OVERRIDE", overrides: []string{"ENTIRE_TEST_DROID_PROJECT_DIR"}},
	agent.AgentTypePi:             {home: "PI_CODING_AGENT_DIR", overrides: []string{"ENTIRE_TEST_PI_SESSION_DIR", "PI_CODING_AGENT_SESSION_DIR"}},
}

// relocateAgentHome points ag's home at a fresh directory, clears the overrides
// that would bypass it, and returns the directory.
func relocateAgentHome(t *testing.T, agentType types.AgentType) string {
	t.Helper()

	env, ok := homeLayoutEnv[agentType]
	if !ok {
		t.Fatalf("no home relocation variable recorded for %s; add it to homeLayoutEnv", agentType)
	}
	home := t.TempDir()
	t.Setenv(env.home, home)
	for _, name := range env.overrides {
		t.Setenv(name, "")
	}
	return home
}

func TestHomeLayout_GetSessionDirLiesInTheFirstStore(t *testing.T) {
	repo := t.TempDir()
	found := 0
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		provider, ok := agent.AsHomeLayoutProvider(ag)
		if !ok {
			continue
		}
		found++
		t.Run(string(name), func(t *testing.T) {
			relocated := relocateAgentHome(t, ag.Type())

			home, err := provider.SessionHome()
			if err != nil {
				t.Fatalf("SessionHome: %v", err)
			}
			if rel, err := filepath.Rel(relocated, home); err != nil || paths.IsRelativeTraversal(rel) {
				t.Fatalf("SessionHome = %q, want it inside the relocated home %q", home, relocated)
			}
			dir, err := ag.GetSessionDir(repo)
			if err != nil {
				t.Fatalf("GetSessionDir: %v", err)
			}
			layout := provider.HomeLayout()
			store, ok := layout.StoreContaining(home, dir)
			if !ok || store != layout.StoresUnder(home)[0] {
				t.Fatalf("GetSessionDir = %q, want it in the first store of %v", dir, layout.StoresUnder(home))
			}
		})
	}
	if found == 0 {
		t.Fatal("no registered agent has a home layout")
	}
}

// writeTranscriptFile creates path with stale content, so agents that wait
// for a transcript to settle do not poll.
func writeTranscriptFile(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestResolveAndValidateTranscript_FindsArchivedCodexRollout(t *testing.T) {
	const sessionID = "019a0000-0000-7000-8000-00000000c0de"
	const name = "rollout-2026-09-30T10-00-00-" + sessionID + ".jsonl"
	tests := []struct {
		name string
		// dir is the rollout's directory below archived_sessions.
		dir []string
	}{
		// Codex archives a rollout directly under archived_sessions, keeping
		// its file name.
		{name: "flat", dir: nil},
		{name: "dated", dir: []string{"2026", "09", "30"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupAttachTestRepo(t)
			home := relocateAgentHome(t, agent.AgentTypeCodex)
			parts := append([]string{home, "archived_sessions"}, tt.dir...)
			archived := filepath.Join(append(parts, name)...)
			writeTranscriptFile(t, archived)
			ag, err := agent.Get(agent.AgentNameCodex)
			if err != nil {
				t.Fatal(err)
			}

			got, err := resolveAndValidateTranscript(context.Background(), sessionID, ag, lookupLocalOnly)
			if err != nil {
				t.Fatalf("resolveAndValidateTranscript: %v", err)
			}
			if got != archived {
				t.Fatalf("transcript = %q, want the archived rollout %q", got, archived)
			}
		})
	}
}

func TestResolveAndValidateTranscript_SkipsUnusableNewestCandidate(t *testing.T) {
	const sessionID = "019a0000-0000-7000-8000-0000000000a1"
	tests := []struct {
		name      string
		agentName types.AgentName
		agentType types.AgentType
		// files returns the newest and an older candidate for the session.
		files func(t *testing.T, home string, ag agent.Agent) (newest, older string)
	}{
		{
			name: "codex dated rollouts", agentName: agent.AgentNameCodex, agentType: agent.AgentTypeCodex,
			files: func(_ *testing.T, home string, _ agent.Agent) (string, string) {
				dir := filepath.Join(home, "sessions", "2026", "10")
				return filepath.Join(dir, "05", "rollout-2026-10-05T10-00-00-"+sessionID+".jsonl"),
					filepath.Join(dir, "01", "rollout-2026-10-01T10-00-00-"+sessionID+".jsonl")
			},
		},
		{
			name: "pi timestamped sessions", agentName: agent.AgentNamePi, agentType: agent.AgentTypePi,
			files: func(t *testing.T, _ string, ag agent.Agent) (string, string) {
				t.Helper()
				worktree, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				dir, err := ag.GetSessionDir(worktree)
				if err != nil {
					t.Fatal(err)
				}
				return filepath.Join(dir, "2026-10-05T10-00-00_"+sessionID+".jsonl"),
					filepath.Join(dir, "2026-10-01T10-00-00_"+sessionID+".jsonl")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupAttachTestRepo(t)
			home := relocateAgentHome(t, tt.agentType)
			ag, err := agent.Get(tt.agentName)
			if err != nil {
				t.Fatal(err)
			}
			newest, older := tt.files(t, home, ag)
			// The newest candidate is a directory, not a transcript.
			if err := os.MkdirAll(newest, 0o750); err != nil {
				t.Fatal(err)
			}
			writeTranscriptFile(t, older)

			got, err := resolveAndValidateTranscript(context.Background(), sessionID, ag, lookupLocalOnly)
			if err != nil {
				t.Fatalf("resolveAndValidateTranscript: %v", err)
			}
			if got != older {
				t.Fatalf("transcript = %q, want the older regular file %q", got, older)
			}
		})
	}
}

func TestResolveAndValidateTranscript_CursorFallsBackToFlatLayout(t *testing.T) {
	setupAttachTestRepo(t)
	dir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", dir)
	const sessionID = "cursor-flat-session"
	// The nested directory exists but holds no transcript.
	if err := os.MkdirAll(filepath.Join(dir, sessionID), 0o750); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(dir, sessionID+".jsonl")
	writeTranscriptFile(t, flat)
	ag, err := agent.Get(agent.AgentNameCursor)
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolveAndValidateTranscript(context.Background(), sessionID, ag, lookupLocalOnly)
	if err != nil {
		t.Fatalf("resolveAndValidateTranscript: %v", err)
	}
	if got != flat {
		t.Fatalf("transcript = %q, want the flat transcript %q", got, flat)
	}
}

func TestResolveAndValidateTranscript_FollowsSymlinkedTranscript(t *testing.T) {
	setupAttachTestRepo(t)
	dir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", dir)
	const sessionID = "linked-session"
	target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	writeTranscriptFile(t, target)
	link := filepath.Join(dir, sessionID+".jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	ag, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolveAndValidateTranscript(context.Background(), sessionID, ag, lookupLocalOnly)
	if err != nil {
		t.Fatalf("resolveAndValidateTranscript: %v", err)
	}
	if got != link {
		t.Fatalf("transcript = %q, want the linked transcript %q", got, link)
	}
}

// strayCandidateAgent has a one-store home and lists, ahead of its store's
// transcript, a candidate inside the home but outside that store.
type strayCandidateAgent struct {
	agent.Agent

	home string
	// homeErr, when set, is what SessionHome reports instead of home.
	homeErr error
}

func (a strayCandidateAgent) GetSessionDir(string) (string, error) {
	return filepath.Join(a.home, "sessions"), nil
}

func (a strayCandidateAgent) SessionHome() (string, error) {
	if a.homeErr != nil {
		return "", a.homeErr
	}
	return a.home, nil
}

func (a strayCandidateAgent) HomeLayout() agent.HomeLayout {
	return agent.HomeLayout{Stores: []string{"sessions"}}
}

func (a strayCandidateAgent) ResolveSessionFileCandidates(sessionDir, agentSessionID string) []string {
	return []string{
		filepath.Join(a.home, "stray", agentSessionID+".jsonl"),
		filepath.Join(sessionDir, agentSessionID+".jsonl"),
	}
}

func TestDiscoverTranscript_SkipsCandidateOutsideTheHomeStores(t *testing.T) {
	setupAttachTestRepo(t)
	codex, err := agent.Get(agent.AgentNameCodex)
	if err != nil {
		t.Fatal(err)
	}
	ag := strayCandidateAgent{Agent: codex, home: t.TempDir()}
	const sessionID = "stray-session"
	writeTranscriptFile(t, filepath.Join(ag.home, "stray", sessionID+".jsonl"))
	stored := filepath.Join(ag.home, "sessions", sessionID+".jsonl")
	writeTranscriptFile(t, stored)

	got, err := discoverTranscript(context.Background(), sessionID, ag)
	if err != nil {
		t.Fatalf("discoverTranscript: %v", err)
	}
	if got != stored {
		t.Fatalf("transcript = %q, want %q from the home's store", got, stored)
	}
}

func TestDiscoverTranscript_LogsUnavailableHomeAndSearchesSessionDir(t *testing.T) {
	setupAttachTestRepo(t)
	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	l, err := logging.New(logging.Config{Root: entiredir.OpenerAt(repoDir), Dir: logging.LogsName, Level: slog.LevelDebug})
	if err != nil {
		t.Fatal(err)
	}
	ctx := logging.WithLogger(context.Background(), l)
	codex, err := agent.Get(agent.AgentNameCodex)
	if err != nil {
		t.Fatal(err)
	}
	ag := strayCandidateAgent{Agent: codex, home: t.TempDir(), homeErr: errors.New("home is unset")}
	const sessionID = "homeless-session"
	stored := filepath.Join(ag.home, "sessions", sessionID+".jsonl")
	writeTranscriptFile(t, stored)

	got, err := discoverTranscript(ctx, sessionID, ag)
	if err != nil {
		t.Fatalf("discoverTranscript: %v", err)
	}
	if got != stored {
		t.Fatalf("transcript = %q, want %q from the session directory", got, stored)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(filepath.Join(repoDir, logging.LogsDir, logging.LogFileName))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logged), "home is unset") {
		t.Fatalf("log = %q, want the SessionHome error", logged)
	}
}
