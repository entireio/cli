//go:build e2e

package tests

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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

		resumed, err := s.Agent.(*agents.Codex).ResumeSession(ctx, s.Dir, codexSession.Home(), sessionID)
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

	re := regexp.MustCompile(`(?m)^\{"timestamp":".*","type":"session_meta","payload":\{"id":"([^"]+)"`)
	m := re.FindSubmatch(data)
	require.Len(t, m, 2, "session_meta id not found in rollout")
	return string(m[1])
}

func appendCompactedEncryptedHistory(t *testing.T, rolloutPath string) {
	t.Helper()

	line := map[string]any{
		"timestamp": "2026-04-08T12:00:00.000Z",
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
	}

	encoded, err := json.Marshal(line)
	require.NoError(t, err)

	f, err := os.OpenFile(rolloutPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	defer f.Close()

	_, err = f.Write(append(encoded, '\n'))
	require.NoError(t, err)
}
