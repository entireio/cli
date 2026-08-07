//go:build e2e

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/agents"
	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/require"
)

func TestCodexResumeRestoredSessionWithSanitizedCompactedHistory(t *testing.T) {
	testutil.ForEachAgent(t, 4*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		if s.Agent.Name() != "codex" {
			t.Skip("Codex-only native resume coverage")
		}

		mainBranch := testutil.GitOutput(t, s.Dir, "branch", "--show-current")
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Enable entire")
		s.Git(t, "checkout", "-b", "feature")

		session := s.StartSession(t, ctx)
		codexSession, ok := session.(*agents.CodexSession)
		require.True(t, ok, "expected Codex session type")

		s.WaitFor(t, session, s.Agent.PromptPattern(), 30*time.Second)
		s.Send(t, session, "create a file at docs/hello.md with a short paragraph about greetings. Do not commit. Do not ask for confirmation.")
		s.WaitFor(t, session, s.Agent.PromptPattern(), 90*time.Second)
		testutil.AssertFileExists(t, s.Dir, "docs/hello.md")

		rolloutPath := findCodexRollout(t, codexSession.Home())
		sessionID := readCodexSessionID(t, rolloutPath)
		appendCompactedEncryptedHistory(t, rolloutPath)

		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add hello doc")
		testutil.WaitForSessionIdle(t, s.Dir, 15*time.Second)
		testutil.WaitForCheckpoint(t, s, 30*time.Second)

		s.Git(t, "checkout", mainBranch)

		out, err := entire.ResumeWithEnv(s.Dir, "feature", []string{"CODEX_HOME=" + codexSession.Home()})
		require.NoError(t, err, "entire resume failed: %s", out)
		require.Contains(t, out, "codex resume "+sessionID)

		// Prove the restored rollout actually contains the shape under test before
		// asserting Codex tolerates it — otherwise a green run says nothing.
		assertRestoredCompactionStripped(t, findRestoredCodexRollout(t, codexSession.Home()))

		codexAgent, ok := s.Agent.(*agents.Codex)
		require.True(t, ok, "expected *agents.Codex agent, got %T", s.Agent)
		resumed, err := codexAgent.ResumeSession(ctx, s.Dir, codexSession.Home(), sessionID)
		require.NoError(t, err)
		defer resumed.Close()

		content, waitErr := resumed.WaitFor(s.Agent.PromptPattern(), 45*time.Second)
		require.NoError(t, waitErr, "resumed Codex session should reach prompt")
		require.NotContains(t, content, "invalid_encrypted_content")
	})
}

func findCodexRollout(t *testing.T, codexHome string) string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*.jsonl"))
	require.NoError(t, err)
	require.Len(t, matches, 1, "expected exactly one Codex rollout in isolated CODEX_HOME")
	return matches[0]
}

func readCodexSessionID(t *testing.T, rolloutPath string) string {
	t.Helper()

	data, err := os.ReadFile(rolloutPath)
	require.NoError(t, err)

	// Codex rollout files are JSONL; the session id lives in the payload of the
	// first "session_meta" line. Parse line-by-line rather than anchoring a
	// regex on field order — Codex reorders JSON keys between versions (the old
	// regex silently stopped matching on Codex 0.142.x). This mirrors the CLI's
	// own parser in cmd/entire/cli/agent/codex/transcript.go.
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var line struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(raw, &line); err != nil || line.Type != "session_meta" {
			continue
		}
		var payload struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(line.Payload, &payload))
		require.NotEmpty(t, payload.ID, "session_meta payload missing id")
		return payload.ID
	}

	t.Fatalf("session_meta line not found in rollout %s", rolloutPath)
	return ""
}

// appendCompactedEncryptedHistory appends the two encrypted-payload shapes Entire
// has to make replayable, so a resume of the restored transcript exercises both:
//
//   - a top-level `response_item` whose payload type is `compaction`. Sanitization
//     strips its encrypted_content but KEEPS the line, so the stored transcript
//     stays line-aligned with the rollout (offsets are counted on the rollout and
//     applied to the stored copy). This is the shape that decides whether Codex
//     tolerates a compaction item with no payload.
//   - a `compacted` line whose replacement_history carries reasoning and compaction
//     items. Those nested items are removed outright — they are array elements, so
//     removing them cannot shift line numbers.
func appendCompactedEncryptedHistory(t *testing.T, rolloutPath string) {
	t.Helper()

	lines := []map[string]any{
		{
			"timestamp": "2026-04-08T12:00:00.000Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":              "compaction",
				"encrypted_content": "REDACTED",
			},
		},
		{
			"timestamp": "2026-04-08T12:00:01.000Z",
			"type":      "compacted",
			"payload": map[string]any{
				"message": "",
				"replacement_history": []map[string]any{
					{
						"type": "message",
						"role": "user",
						"content": []map[string]any{
							{"type": "input_text", "text": "hello"},
						},
					},
					{
						"type":              "reasoning",
						"summary":           []map[string]any{{"text": "brief"}},
						"encrypted_content": "REDACTED",
					},
					{
						"type":              "compaction",
						"encrypted_content": "REDACTED",
					},
				},
			},
		},
	}

	f, err := os.OpenFile(rolloutPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	defer f.Close()

	for _, line := range lines {
		encoded, err := json.Marshal(line)
		require.NoError(t, err)
		_, err = f.Write(append(encoded, '\n'))
		require.NoError(t, err)
	}
}

// findRestoredCodexRollout returns the rollout `entire resume` wrote, which is the one
// the subsequent `codex resume` loads.
//
// Restore may either overwrite the agent's original file or add a second one, and
// which happens depends on the machine's timezone: Entire derives the restored path
// from the session start time in UTC (codex.restoredRolloutPath), while Codex names
// its own rollout in local time. On a UTC host (CI) the two names collide and restore
// overwrites in place; anywhere else they differ and both files remain.
//
// So do not key off "the path that is not the original" — that only holds off-UTC.
// Pick the most recently written rollout instead, which is the restored copy in both
// layouts.
func findRestoredCodexRollout(t *testing.T, codexHome string) string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*.jsonl"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "no Codex rollout found after resume")

	newest := newestFile(t, matches)
	t.Logf("rollouts after resume (%d): %v", len(matches), matches)
	t.Logf("asserting on most recently written: %s", newest)
	return newest
}

// newestFile returns the most recently modified of paths. Ties are broken by taking
// the later entry, which keeps the choice deterministic when a filesystem reports
// coarse timestamps.
func newestFile(t *testing.T, paths []string) string {
	t.Helper()
	require.NotEmpty(t, paths)

	newest := paths[0]
	newestMod := fileModTime(t, newest)
	for _, p := range paths[1:] {
		if mod := fileModTime(t, p); !mod.Before(newestMod) {
			newest, newestMod = p, mod
		}
	}
	return newest
}

func fileModTime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return fi.ModTime()
}

// assertRestoredCompactionStripped verifies the restored rollout carries a
// top-level compaction item whose encrypted payload was stripped while its LINE
// survived. That is the shape the subsequent `codex resume` has to tolerate, and
// asserting it here stops the test from passing vacuously if the fixture, the
// sanitizer, or the restore path ever stops producing it.
func assertRestoredCompactionStripped(t *testing.T, rolloutPath string) {
	t.Helper()

	data, err := os.ReadFile(rolloutPath)
	require.NoError(t, err)
	require.NotContains(t, string(data), "encrypted_content",
		"restored rollout still carries encrypted payloads")

	var sawStrippedCompaction bool
	var sawCompactedLine bool
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var line struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(raw, &line) != nil || line.Type != "response_item" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(line.Payload, &payload) != nil {
			continue
		}
		if payload["type"] == "compaction" {
			sawStrippedCompaction = true
			t.Logf("restored top-level compaction item, payload keys: %v", payloadKeys(payload))
			require.NotContains(t, payload, "encrypted_content",
				"compaction item kept its encrypted payload")
		}
	}

	for _, raw := range bytes.Split(data, []byte("\n")) {
		if bytes.Contains(raw, []byte(`"type":"compacted"`)) {
			sawCompactedLine = true
		}
	}
	require.True(t, sawCompactedLine, "restored rollout lost the compacted line")

	require.True(t, sawStrippedCompaction,
		"restored rollout has no top-level compaction item — the test did not exercise "+
			"the payload-less compaction shape, so its result is meaningless")
}

func payloadKeys(payload map[string]any) []string {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestFindRestoredCodexRollout_HandlesOverwriteAndSecondFile pins the selection rule
// against both layouts `entire resume` can produce, without needing a real agent.
//
// This exists because the original helper assumed restore always adds a second file
// and identified it as "the path that is not the original". That assumption is
// timezone-dependent — Entire derives the restored path in UTC while Codex names its
// own rollout in local time — so it held on a developer laptop and broke on CI, where
// the names collide and restore overwrites in place.
func TestFindRestoredCodexRollout_HandlesOverwriteAndSecondFile(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, home, name string, modAt time.Time) string {
		t.Helper()
		dir := filepath.Join(home, "sessions", "2026", "08", "06")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte("{}\n"), 0o644))
		require.NoError(t, os.Chtimes(p, modAt, modAt))
		return p
	}

	base := time.Now().Add(-time.Hour)

	t.Run("overwritten in place (UTC host)", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		only := write(t, home, "rollout-2026-08-06T10-54-17-sess.jsonl", base)

		require.Equal(t, only, findRestoredCodexRollout(t, home),
			"with a single rollout the helper must use it rather than demanding a second file")
	})

	t.Run("second file added (non-UTC host)", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		// The agent's own file, named in local time, written first.
		write(t, home, "rollout-2026-08-06T12-54-17-sess.jsonl", base)
		// Restore's copy, named in UTC, written afterwards.
		restored := write(t, home, "rollout-2026-08-06T10-54-17-sess.jsonl", base.Add(time.Minute))

		require.Equal(t, restored, findRestoredCodexRollout(t, home),
			"with two rollouts the helper must pick the one restore wrote, not the lexically first")
	})
}
