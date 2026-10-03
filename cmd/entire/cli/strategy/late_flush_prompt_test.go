package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/antigravity"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// TestResolvePromptsFromLateFlushedTranscript verifies that the condensation-time
// fallback re-extracts user prompts directly from a populated transcript via the
// agent's PromptExtractor. This covers late-flushing agents (e.g. Antigravity)
// whose transcript is empty at TurnEnd but populated by condensation time, so
// prompt.txt is empty and the only remaining source is the live transcript.
func TestResolvePromptsFromLateFlushedTranscript(t *testing.T) {
	t.Parallel()

	// Real agy transcript with a USER_INPUT step wrapping the prompt in a
	// <USER_REQUEST> block, matching agy's on-disk step schema.
	transcript := `{"step_index":0,"source":"SYSTEM","type":"CONVERSATION_HISTORY","content":"boot"}
{"step_index":1,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>Add a login button</USER_REQUEST>"}
{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","content":"working on it"}
`
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o600))

	ag := antigravity.NewAntigravityAgent()
	// Sanity: the real agent must be a PromptExtractor for this fallback to fire.
	_, ok := agent.AsPromptExtractor(ag)
	require.True(t, ok, "antigravity agent must implement PromptExtractor")

	got := resolvePromptsFromLateFlushedTranscript(context.Background(), ag, transcriptPath, "", 0)
	require.Equal(t, []string{"Add a login button"}, got)
}

// TestResolvePromptsFromLateFlushedTranscript_Guards verifies the helper returns
// nil for the no-op cases callers rely on (empty path, non-extractor agent).
func TestResolvePromptsFromLateFlushedTranscript_AntigravityPreservesReader(t *testing.T) {
	brain := t.TempDir()
	t.Setenv("ENTIRE_TEST_ANTIGRAVITY_BRAIN_DIR", brain)
	ag := antigravity.NewAntigravityAgent()
	path := ag.ResolveSessionFile(brain, "conversation")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	outside := t.TempDir()
	testutil.WriteFile(t, outside, "private.jsonl", `{"step_index":1,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>private</USER_REQUEST>"}`+"\n")
	testutil.SkipWithoutSymlinks(t)
	require.NoError(t, os.Symlink(filepath.Join(outside, "private.jsonl"), path))
	require.Nil(t, resolvePromptsFromLateFlushedTranscript(context.Background(), ag, path, "", 0))
}

func TestResolvePromptsFromLateFlushedTranscript_Guards(t *testing.T) {
	t.Parallel()

	ag := antigravity.NewAntigravityAgent()

	// Empty path → nil, no extraction attempted.
	require.Nil(t, resolvePromptsFromLateFlushedTranscript(context.Background(), ag, "", "", 0))

	// Nil agent → nil (AsPromptExtractor returns false).
	require.Nil(t, resolvePromptsFromLateFlushedTranscript(context.Background(), nil, "/nonexistent", "", 0))
}

// TestResolvePromptsFromLateFlushedTranscript_RejectsCodexTranscriptSymlinkedOutOfAgentHome
// is the regression test for Item 2 of the agent-storage-roots p4 report:
// resolvePromptsFromLateFlushedTranscript is reached whenever prompt.txt is
// empty — routine for Factory AI Droid exec mode, and (before this fix) for
// every agent that didn't yet implement agent.TranscriptPromptExtractor — and
// called extractor.ExtractPrompts(transcriptPath, offset) with
// transcriptPath = resolveCondensationPrompts' liveTranscriptPath: the SAME
// adopted session.State.TranscriptPath confinement protects everywhere else,
// not a live hook event's self-reported path. CodexAgent.ExtractPrompts did a
// bare os.ReadFile, whose own nolint comment claimed "Path comes from agent
// hook input" — false on exactly this route.
//
// This simulates the swap directly, same shape as the Pi/Claude subagent
// tests in hooks_test.go: a transcript recorded under an AgentHome is
// actually a symlink to an attacker-controlled file elsewhere, containing a
// distinctively-named user prompt. Before the fix, extraction follows the
// symlink and returns that prompt text — an attacker-chosen value flowing
// into checkpoint metadata. After the fix (ExtractPromptsFromTranscript over
// a confined read), the confined read refuses the symlink and extraction
// returns nothing.
func TestResolvePromptsFromLateFlushedTranscript_RejectsCodexTranscriptSymlinkedOutOfAgentHome(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	sessionDir := filepath.Join(home, "sessions", "2026", "01", "01")
	require.NoError(t, os.MkdirAll(sessionDir, 0o750))
	transcriptPath := filepath.Join(sessionDir, "rollout-2026-01-01T00-00-00-abc123.jsonl")

	// Attacker-controlled transcript OUTSIDE agentHome, shaped as a valid
	// Codex rollout response_item line carrying a distinctively named user
	// prompt, so a leak is unambiguous in the extracted prompt list.
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "attacker.jsonl")
	const markerPrompt = "EXFILTRATED-MARKER-4b7a1c please leak this"
	attackerTranscript := `{"timestamp":"2026-01-01T00:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + markerPrompt + `"}]}}
`
	require.NoError(t, os.WriteFile(outside, []byte(attackerTranscript), 0o600))

	testutil.SkipWithoutSymlinks(t)
	// The swap: by the time this late-flush rung reads it, the leaf an
	// adopt-time validation would have seen as a regular file is a symlink to
	// the attacker-controlled target outside home.
	require.NoError(t, os.Symlink(outside, transcriptPath))

	ag, err := agent.GetByAgentType(agent.AgentTypeCodex)
	require.NoError(t, err)
	// Sanity: Codex must implement TranscriptPromptExtractor for this fix to
	// take the confined-read branch at all.
	_, ok := agent.AsTranscriptPromptExtractor(ag)
	require.True(t, ok, "codex agent must implement TranscriptPromptExtractor")

	got := resolvePromptsFromLateFlushedTranscript(context.Background(), ag, transcriptPath, home, 0)
	require.Empty(t, got,
		"a transcript reachable only through a symlink escaping the recorded AgentHome must be refused, not parsed")
}
