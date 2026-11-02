package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests below set environment variables, so none of them runs in parallel.

const (
	attachHomeClaudeTranscript = `{"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1"}` + "\n"
	attachHomeCodexSessionID   = "019d6c43-1537-7343-9691-1f8cee04fe59"
	attachHomeCodexTranscript  = `{"timestamp":"2026-04-08T10:43:48.000Z","type":"session_meta","payload":{"id":"019d6c43-1537-7343-9691-1f8cee04fe59","timestamp":"2026-04-08T10:43:48.000Z"}}
{"timestamp":"2026-04-08T10:43:49.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"investigate attach failure"}]}}
`
)

// attachHomesEnv isolates HOME and the per-user registry, and points the
// active homes of Claude Code and Codex at fresh directories, clearing the
// test overrides that would bypass them. Agents whose home defaults beneath
// HOME (Copilot CLI, Droid, Pi) use the isolated HOME. It returns the config
// directory and the Claude home.
func attachHomesEnv(t *testing.T) (configDir, claudeHome string) {
	t.Helper()

	t.Setenv("HOME", t.TempDir())
	configDir = t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	claudeHome = canonicalDir(t)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	t.Setenv("CODEX_HOME", canonicalDir(t))
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", "")
	return configDir, claudeHome
}

// canonicalDir returns a fresh directory's canonical path.
func canonicalDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

// writeStaleFile writes content to path, backdated so attach does not wait for
// the transcript to settle.
func writeStaleFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	stale := time.Now().Add(-3 * time.Minute)
	require.NoError(t, os.Chtimes(path, stale, stale))
}

// attachAndLoad attaches sessionID with agentName and returns attach's output
// and the saved session state.
func attachAndLoad(t *testing.T, sessionID string, agentName types.AgentName) (string, *session.State) {
	t.Helper()

	var out bytes.Buffer
	require.NoError(t, runAttach(context.Background(), &out, &out, sessionID, agentName, attachOptions{Force: true}))
	store, err := session.NewStateStore(context.Background())
	require.NoError(t, err)
	state, err := store.Load(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	return out.String(), state
}

func TestAttach_RecordsTheActiveHomeHoldingTheTranscript(t *testing.T) {
	setupAttachTestRepo(t)
	_, claudeHome := attachHomesEnv(t)
	const sessionID = "attach-active-home"
	transcript := filepath.Join(claudeHome, "projects", "other-project", sessionID+".jsonl")
	writeStaleFile(t, transcript, attachHomeClaudeTranscript)

	out, state := attachAndLoad(t, sessionID, agent.AgentNameClaudeCode)

	assert.NotContains(t, out, "Found transcript under agent home")
	assert.Equal(t, transcript, state.TranscriptPath)
	assert.Equal(t, claudeHome, state.AgentHome)
	homes, err := agent.KnownAgentHomes(agent.AgentTypeClaudeCode)
	require.NoError(t, err)
	assert.Equal(t, []string{claudeHome}, homes)
}

func TestAttach_FindsATranscriptUnderARecordedHome(t *testing.T) {
	tests := []struct {
		name       string
		agentName  types.AgentName
		agentType  types.AgentType
		sessionID  string
		transcript func(home, sessionID string) string
		content    string
	}{
		{
			name: "claude project directory", agentName: agent.AgentNameClaudeCode, agentType: agent.AgentTypeClaudeCode,
			sessionID: "attach-recorded-home",
			transcript: func(home, sessionID string) string {
				return filepath.Join(home, "projects", "elsewhere", sessionID+".jsonl")
			},
			content: attachHomeClaudeTranscript,
		},
		{
			name: "codex archived rollout", agentName: agent.AgentNameCodex, agentType: agent.AgentTypeCodex,
			sessionID: attachHomeCodexSessionID,
			transcript: func(home, sessionID string) string {
				return filepath.Join(home, "archived_sessions", "rollout-2026-04-08T10-43-48-"+sessionID+".jsonl")
			},
			content: attachHomeCodexTranscript,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupAttachTestRepo(t)
			attachHomesEnv(t)
			recorded := canonicalDir(t)
			require.NoError(t, agent.RememberAgentHome(tt.agentType, recorded))
			transcript := tt.transcript(recorded, tt.sessionID)
			writeStaleFile(t, transcript, tt.content)

			out, state := attachAndLoad(t, tt.sessionID, tt.agentName)

			assert.Contains(t, out, "Found transcript under agent home "+recorded)
			assert.NotContains(t, out, "Auto-detected agent")
			assert.Equal(t, transcript, state.TranscriptPath)
			assert.Equal(t, recorded, state.AgentHome)
		})
	}
}

func TestAttach_AutoDetectsAnAgentUnderARecordedHome(t *testing.T) {
	setupAttachTestRepo(t)
	attachHomesEnv(t)
	recorded := canonicalDir(t)
	require.NoError(t, agent.RememberAgentHome(agent.AgentTypeCodex, recorded))
	transcript := filepath.Join(recorded, "sessions", "2026", "04", "08", "rollout-2026-04-08T10-43-48-"+attachHomeCodexSessionID+".jsonl")
	writeStaleFile(t, transcript, attachHomeCodexTranscript)

	// The default agent is Claude Code; the session is Codex's.
	out, state := attachAndLoad(t, attachHomeCodexSessionID, agent.AgentNameClaudeCode)

	assert.Contains(t, out, "Auto-detected agent: codex")
	assert.Contains(t, out, "Found transcript under agent home "+recorded)
	assert.Equal(t, agent.AgentTypeCodex, state.AgentType)
	assert.Equal(t, recorded, state.AgentHome)
}

func TestAttach_ReattachSearchesTheSessionAgentsRecordedHomes(t *testing.T) {
	setupAttachTestRepo(t)
	attachHomesEnv(t)
	recorded := canonicalDir(t)
	require.NoError(t, agent.RememberAgentHome(agent.AgentTypeCodex, recorded))
	transcript := filepath.Join(recorded, "sessions", "2026", "04", "08", "rollout-2026-04-08T10-43-48-"+attachHomeCodexSessionID+".jsonl")
	writeStaleFile(t, transcript, attachHomeCodexTranscript)
	store, err := session.NewStateStore(context.Background())
	require.NoError(t, err)
	require.NoError(t, store.Save(context.Background(), &session.State{
		SessionID: attachHomeCodexSessionID,
		AgentType: agent.AgentTypeCodex,
		StartedAt: time.Now(),
	}))

	// The existing state's agent type selects Codex despite the default flag.
	out, state := attachAndLoad(t, attachHomeCodexSessionID, agent.AgentNameClaudeCode)

	assert.NotContains(t, out, "Auto-detected agent")
	assert.Contains(t, out, "Found transcript under agent home "+recorded)
	assert.Equal(t, recorded, state.AgentHome)
}

func TestAttach_ReportsAnUnreadableRegistry(t *testing.T) {
	setupAttachTestRepo(t)
	configDir, _ := attachHomesEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "agent_homes.json"), []byte("{not json"), 0o600))

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, &out, "attach-unreadable-registry", agent.AgentNameClaudeCode, attachOptions{Force: true})
	require.ErrorContains(t, err, "other agent homes could not be searched")
	require.ErrorContains(t, err, filepath.Join(configDir, "agent_homes.json"))
	require.ErrorContains(t, err, "is the session ID correct?")
}

func TestResolveAgentAndTranscript_ReportsAnUnreadableRegistryAfterFetchFailure(t *testing.T) {
	setupAttachTestRepo(t)
	configDir, _ := attachHomesEnv(t)
	t.Setenv("ENTIRE_TEST_OPENCODE_MOCK_EXPORT", "1")
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "agent_homes.json"), []byte("{not json"), 0o600))

	var out bytes.Buffer
	_, _, err := resolveAgentAndTranscript(context.Background(), &out, "attach-fetch-unreadable-registry", agent.AgentNameOpenCode, nil)
	require.ErrorContains(t, err, "mock export file not found")
	require.ErrorContains(t, err, "other agent homes could not be searched")
	require.ErrorContains(t, err, filepath.Join(configDir, "agent_homes.json"))
	require.NotContains(t, err.Error(), "also tried auto-detecting")
}

func TestAttach_ClearsAHomeThatDoesNotHoldTheTranscript(t *testing.T) {
	setupAttachTestRepo(t)
	attachHomesEnv(t)
	const sessionID = "attach-stale-home"
	projectDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", projectDir)
	transcript := filepath.Join(projectDir, sessionID+".jsonl")
	writeStaleFile(t, transcript, attachHomeClaudeTranscript)
	store, err := session.NewStateStore(context.Background())
	require.NoError(t, err)
	// The transcript is unchanged, so only always recomputing the home clears it.
	require.NoError(t, store.Save(context.Background(), &session.State{
		SessionID:      sessionID,
		AgentType:      agent.AgentTypeClaudeCode,
		StartedAt:      time.Now(),
		TranscriptPath: transcript,
		AgentHome:      "/stale/agent/home",
	}))

	_, state := attachAndLoad(t, sessionID, agent.AgentNameClaudeCode)

	assert.Empty(t, state.AgentHome)
	homes, err := agent.KnownAgentHomes(agent.AgentTypeClaudeCode)
	require.NoError(t, err)
	assert.Empty(t, homes, "a home that holds no transcript is not recorded")
}

func TestSearchRecordedHomes_AgentLayouts(t *testing.T) {
	const sessionID = "019d6c43-1537-7343-9691-00000000abcd"
	tests := []struct {
		name      string
		agentName types.AgentName
		agentType types.AgentType
		// setup points the agent's active home somewhere, records home, and
		// returns where the transcript goes beneath it ("" for none).
		setup    func(t *testing.T, home string) string
		wantNone bool
	}{
		{
			name: "droid project directory", agentName: agent.AgentNameFactoryAIDroid, agentType: agent.AgentTypeFactoryAIDroid,
			setup: func(t *testing.T, home string) string {
				t.Helper()
				t.Setenv("FACTORY_HOME_OVERRIDE", t.TempDir())
				t.Setenv("ENTIRE_TEST_DROID_PROJECT_DIR", "")
				return filepath.Join(home, "sessions", "-some-project", sessionID+".jsonl")
			},
		},
		{
			name: "copilot session directory", agentName: agent.AgentNameCopilotCLI, agentType: agent.AgentTypeCopilotCLI,
			setup: func(t *testing.T, home string) string {
				t.Helper()
				t.Setenv("COPILOT_HOME", t.TempDir())
				t.Setenv("ENTIRE_TEST_COPILOT_SESSION_DIR", "")
				return filepath.Join(home, "session-state", sessionID, "events.jsonl")
			},
		},
		{
			name: "pi active home with a relocated session store", agentName: agent.AgentNamePi, agentType: agent.AgentTypePi,
			setup: func(t *testing.T, home string) string {
				t.Helper()
				// The active home is home, but its sessions are now kept
				// elsewhere, so the active search never covered home's store.
				t.Setenv("PI_CODING_AGENT_DIR", home)
				t.Setenv("PI_CODING_AGENT_SESSION_DIR", t.TempDir())
				t.Setenv("ENTIRE_TEST_PI_SESSION_DIR", "")
				return filepath.Join(home, "sessions", "--some-project--", "2026-10-08T10-00-00_"+sessionID+".jsonl")
			},
		},
		{
			name: "pi active home already searched", agentName: agent.AgentNamePi, agentType: agent.AgentTypePi,
			setup: func(t *testing.T, home string) string {
				t.Helper()
				t.Setenv("PI_CODING_AGENT_DIR", home)
				t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
				t.Setenv("ENTIRE_TEST_PI_SESSION_DIR", "")
				return filepath.Join(home, "sessions", "--some-project--", "2026-10-08T10-00-00_"+sessionID+".jsonl")
			},
			wantNone: true,
		},
		{
			name: "claude transcript deeper than a project directory", agentName: agent.AgentNameClaudeCode, agentType: agent.AgentTypeClaudeCode,
			setup: func(t *testing.T, home string) string {
				t.Helper()
				return filepath.Join(home, "projects", "p", "nested", sessionID+".jsonl")
			},
			wantNone: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupAttachTestRepo(t)
			attachHomesEnv(t)
			home := canonicalDir(t)
			transcript := tt.setup(t, home)
			writeStaleFile(t, transcript, "{}\n")
			require.NoError(t, agent.RememberAgentHome(tt.agentType, home))
			ag, err := agent.Get(tt.agentName)
			require.NoError(t, err)

			found, err := searchRecordedHomes(context.Background(), sessionID, ag)

			require.NoError(t, err)
			if tt.wantNone {
				assert.Equal(t, foundTranscript{}, found)
				return
			}
			assert.Equal(t, foundTranscript{Path: transcript, RecordedHome: home}, found)
		})
	}
}
