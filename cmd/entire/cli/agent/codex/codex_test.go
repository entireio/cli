package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/require"
)

// Compile-time interface checks
var (
	_ agent.Agent              = (*CodexAgent)(nil)
	_ agent.HookSupport        = (*CodexAgent)(nil)
	_ agent.HookResponseWriter = (*CodexAgent)(nil)
)

func TestCodexAgent_Name(t *testing.T) {
	t.Parallel()
	ag := NewCodexAgent()
	require.Equal(t, types.AgentName("codex"), ag.Name())
}

func TestCodexAgent_Type(t *testing.T) {
	t.Parallel()
	ag := NewCodexAgent()
	require.Equal(t, types.AgentType("Codex"), ag.Type())
}

func TestCodexAgent_Description(t *testing.T) {
	t.Parallel()
	ag := NewCodexAgent()
	require.Contains(t, ag.Description(), "Codex")
}

func TestCodexAgent_IsPreview(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	require.True(t, ag.IsPreview())
}

func TestCodexAgent_ProtectedDirs(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	require.Equal(t, []string{".codex"}, ag.ProtectedDirs())
}

func TestCodexAgent_HookNames(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	names := ag.HookNames()
	require.Contains(t, names, "session-start")
	require.Contains(t, names, "user-prompt-submit")
	require.Contains(t, names, "stop")
	require.Contains(t, names, "pre-tool-use")
}

func TestCodexAgent_FormatResumeCommand(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	cmd := ag.FormatResumeCommand("550e8400-e29b-41d4-a716-446655440000")
	require.Equal(t, "codex resume 550e8400-e29b-41d4-a716-446655440000", cmd)
}

func TestCodexAgent_GetSessionDir(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", fakeHome)

	ag := &CodexAgent{}
	dir, err := ag.GetSessionDir("")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(fakeHome, ".codex", "sessions"), dir)
}

func TestCodexAgent_ResolveSessionFile_SessionTreeLayout(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	sessionID := "019d6c43-1537-7343-9691-1f8cee04fe59"
	sessionDir := t.TempDir()
	dayDir := filepath.Join(sessionDir, "2026", "04", "08")
	require.NoError(t, os.MkdirAll(dayDir, 0o750))

	expected := filepath.Join(dayDir, "rollout-2026-04-08T10-43-48-"+sessionID+".jsonl")
	require.NoError(t, os.WriteFile(expected, []byte(sampleRollout), 0o600))

	result := ag.ResolveSessionFile(sessionDir, sessionID)
	require.Equal(t, expected, result)
}

func TestCodexAgent_ResolveRestoredSessionFile(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	dir := t.TempDir()

	path, err := ag.ResolveRestoredSessionFile(dir, "019d24c3-1111-2222-3333-444444444444", []byte(sampleRollout))
	require.NoError(t, err)
	require.Equal(t,
		filepath.Join(dir, "2026", "03", "25", "rollout-2026-03-25T11-31-10-019d24c3-1111-2222-3333-444444444444.jsonl"),
		path,
	)
}

func TestCodexAgent_ResolveSessionFile_FindsNestedRollout(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	dir := t.TempDir()
	want := filepath.Join(dir, "2026", "03", "25", "rollout-2026-03-25T11-31-10-019d24c3.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(want), 0o755))
	require.NoError(t, os.WriteFile(want, []byte(sampleRollout), 0o600))

	got := ag.ResolveSessionFile(dir, "019d24c3")
	require.Equal(t, want, got)
}

func TestCodexAgent_ReadSession(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	path := writeSampleRollout(t)

	session, err := ag.ReadSession(&agent.HookInput{
		SessionID:  "codex-session-1",
		SessionRef: path,
	})
	require.NoError(t, err)
	require.Equal(t, "codex-session-1", session.SessionID)
	require.Equal(t, agent.AgentNameCodex, session.AgentName)
	require.Equal(t, path, session.SessionRef)
	require.Equal(t, time.Date(2026, time.March, 25, 11, 31, 10, 922000000, time.UTC), session.StartTime)
	require.ElementsMatch(t, []string{"hello.txt", "docs/readme.md"}, session.ModifiedFiles)
	requireJSONL(t, sampleRollout, string(session.NativeData))
}

func TestCodexAgent_ReadSession_InvalidSessionMeta(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"timestamp":"2026-03-25T11:31:11.754Z","type":"response_item","payload":{"type":"message"}}`), 0o600))

	_, err := ag.ReadSession(&agent.HookInput{
		SessionID:  "codex-session-1",
		SessionRef: path,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, `first transcript line is "response_item", want session_meta`)
}

func requireJSONL(t *testing.T, expected string, actual string) {
	t.Helper()

	expectedLines := strings.Split(strings.TrimSuffix(expected, "\n"), "\n")
	actualLines := strings.Split(strings.TrimSuffix(actual, "\n"), "\n")
	require.Len(t, actualLines, len(expectedLines))
	for i := range expectedLines {
		require.JSONEq(t, expectedLines[i], actualLines[i])
	}
}
