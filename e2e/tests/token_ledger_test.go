//go:build e2e

package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/pi" // the e2e harness links every other built-in agent
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/e2e/testutil"
)

// TestTokenLedger_EveryCallCountedOnce runs turns that changed files, a turn
// that changed nothing and a commit made by the agent mid-turn, then checks
// every session against the agent's own files: what the checkpoints stored
// plus what is still pending equals the session total, and that total equals
// a fresh whole read of the agent's files. Nothing dropped, nothing counted
// twice.
func TestTokenLedger_EveryCallCountedOnce(t *testing.T) {
	testutil.ForEachAgent(t, 6*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		ag, err := agent.Get(types.AgentName(s.Agent.EntireAgent()))
		if err != nil {
			t.Skipf("%s is not a built-in agent", s.Agent.Name())
		}
		if _, ok := agent.AsUsageCounterSource(ag); !ok {
			t.Skipf("%s keeps the per-turn token accounting", s.Agent.Name())
		}

		for _, prompt := range []string{
			"Create a file called a.txt containing the word alpha. Do not commit. Do not ask for confirmation.",
			"What is 2+2? Reply with just the number. Do not create or change any files.",
			"Create a file called b.txt containing the word beta, then run `git add b.txt && git commit -m 'add b'`. Then reply done. Do not ask for confirmation.",
		} {
			if _, err := s.RunPrompt(t, ctx, prompt); err != nil {
				t.Fatalf("agent failed: %v", err)
			}
		}
		testutil.AssertFileExists(t, s.Dir, "a.txt")
		s.Git(t, "add", "a.txt")
		s.Git(t, "commit", "-m", "add a")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		testutil.WaitForSessionIdle(t, s.Dir, 30*time.Second)

		stored := storedTokensBySession(t, s.Dir)
		checked := 0
		for _, state := range sessionStates(t, s.Dir) {
			ledger := state.TokenLedger
			if !ledger.IsActive() {
				continue
			}
			checked++
			// Child sessions (Droid Workers, OpenCode children) are credited
			// beside the ledger and stored with it.
			total := session.WithChildSessionTokens(ledger.Total(), state.ChildSessionTokens)
			sum := addUsage(stored[state.SessionID], session.WithChildSessionTokens(ledger.Pending(), state.PendingChildSessionTokens()))
			assertSameTokens(t, state.SessionID+": checkpoints + pending vs session total", total, sum)

			fresh, _, err := agent.ReadCumulativeUsage(ctx, ag, agent.CumulativeTokenInput{
				SessionID: state.SessionID, TranscriptPath: state.TranscriptPath, Home: state.AgentHome,
				Subagents: strategy.SubagentReferences(state),
			}, nil)
			if errors.Is(err, fs.ErrNotExist) {
				// The runner's agent home is gone (pi runs each prompt in its own).
				t.Logf("%s: agent files cleaned up, skipping the fresh read", state.SessionID)
				continue
			}
			if err != nil {
				t.Fatalf("%s: reading the agent's files: %v", state.SessionID, err)
			}
			read := types.AddTokenUsage(fresh.Total, nil)
			for _, child := range fresh.Children {
				if read == nil {
					read = &agent.TokenUsage{}
				}
				read.SubagentTokens = types.AddTokenUsage(read.SubagentTokens, child)
			}
			assertSameTokens(t, state.SessionID+": session total vs a fresh read of the agent's files", ledger.Total(), read)
			t.Logf("%s: %+v", state.SessionID, total)
		}
		if checked == 0 {
			t.Fatal("no session counted its tokens with the ledger")
		}
	})
}

func sessionStates(t *testing.T, dir string) []*session.State {
	t.Helper()
	stateDir := filepath.Join(dir, ".git", "entire-sessions")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read session states: %v", err)
	}
	var states []*session.State
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stateDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var state session.State
		if err := json.Unmarshal(data, &state); err != nil {
			continue // not a session state
		}
		states = append(states, &state)
	}
	return states
}

// storedTokensBySession sums every checkpoint's token usage per session.
func storedTokensBySession(t *testing.T, dir string) map[string]*agent.TokenUsage {
	t.Helper()
	stored := map[string]*agent.TokenUsage{}
	for _, cp := range testutil.CheckpointIDs(t, dir) {
		for i := range testutil.ReadCheckpointMetadata(t, dir, cp).Sessions {
			meta := testutil.ReadSessionMetadata(t, dir, cp, i)
			stored[meta.SessionID] = addUsage(stored[meta.SessionID], toUsage(&meta.TokenUsage))
		}
	}
	return stored
}

func toUsage(u *testutil.TokenUsage) *agent.TokenUsage {
	if u == nil {
		return nil
	}
	return &agent.TokenUsage{
		InputTokens: u.InputTokens, CacheCreationTokens: u.CacheCreationTokens, CacheReadTokens: u.CacheReadTokens,
		OutputTokens: u.OutputTokens, APICallCount: u.APICallCount, SubagentTokens: toUsage(u.SubagentTokens),
	}
}

func addUsage(a, b *agent.TokenUsage) *agent.TokenUsage { return types.AddTokenUsage(a, b) }

func assertSameTokens(t *testing.T, label string, want, got *agent.TokenUsage) {
	t.Helper()
	flat := func(u *agent.TokenUsage) [5]int {
		if u == nil {
			return [5]int{}
		}
		return [5]int{u.InputTokens, u.CacheCreationTokens, u.CacheReadTokens, u.OutputTokens, u.APICallCount}
	}
	if flat(want) != flat(got) {
		t.Errorf("%s:\n want %+v\n got  %+v", label, want, got)
	}
	var ws, gs *agent.TokenUsage
	if want != nil {
		ws = want.SubagentTokens
	}
	if got != nil {
		gs = got.SubagentTokens
	}
	if flat(ws) != flat(gs) {
		t.Errorf("%s (subagents):\n want %+v\n got  %+v", label, ws, gs)
	}
}
