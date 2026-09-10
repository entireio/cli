package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/globalhooks"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/stretchr/testify/require"
)

func selectTestHookInstallation(t *testing.T) globalhooks.Selection {
	t.Helper()
	path := filepath.Join(t.TempDir(), "selected-entire")
	require.NoError(t, os.WriteFile(path, []byte("selected installation"), 0o700))
	selected, err := globalhooks.New(path)
	require.NoError(t, err)
	require.NoError(t, globalhooks.Save(t.Context(), selected))
	return selected
}

func TestGlobalAndRepositoryIngressDispatchOnce(t *testing.T) {
	// Each case uses process-local CWD and user-config overrides.
	for _, name := range []types.AgentName{agent.AgentNameClaudeCode, agent.AgentNameGemini} {
		for _, state := range []string{"current", "repository-first", "partial", "disabled", "missing-selected", "unselected", "replay"} {
			t.Run(string(name)+"/"+state, func(t *testing.T) {
				setupStopTestRepo(t)
				root := mustGetwd(t)
				enableEntire(t, root)
				isolatedUserHome(t)
				ag, err := agent.Get(name)
				require.NoError(t, err)
				hooks, ok := agent.AsHookSupport(ag)
				require.True(t, ok)
				userHooks, ok := agent.AsUserHookSupport(ag)
				require.True(t, ok)
				_, err = hooks.InstallHooks(t.Context(), false)
				require.NoError(t, err)
				locator, ok := ag.(agent.HookConfigLocator)
				require.True(t, ok)
				dir := filepath.Dir(locator.HookConfigRelPath())
				verb, removedEvent := "user-prompt-submit", "Stop"
				transcriptBody := `{"type":"user","message":{"content":"hello"}}` + "\n"
				if name == agent.AgentNameGemini {
					verb, removedEvent = "before-agent", "AfterAgent"
					transcriptBody = `{"messages":[]}`
				}
				repoHooksPath := filepath.Join(root, dir, "settings.json")
				repoHooksBefore, err := os.ReadFile(repoHooksPath)
				require.NoError(t, err)
				writeUserSettings(t, `{"global":{"enabled":true}}`)
				selected := selectTestHookInstallation(t)
				_, err = userHooks.InstallUserHooks(t.Context())
				require.NoError(t, err)
				repoHooksAfter, err := os.ReadFile(repoHooksPath)
				require.NoError(t, err)
				require.Equal(t, repoHooksBefore, repoHooksAfter, "global migration must preserve existing repository hook strings")
				switch state {
				case "replay":
					t.Setenv("ENTIRE_BINDING_REPLAY", "1")
				case "partial":
					home, homeErr := os.UserHomeDir()
					require.NoError(t, homeErr)
					path := filepath.Join(home, dir, "settings.json")
					data, readErr := os.ReadFile(path)
					require.NoError(t, readErr)
					var config map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(data, &config))
					var events map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(config["hooks"], &events))
					delete(events, removedEvent)
					config["hooks"], err = json.Marshal(events)
					require.NoError(t, err)
					data, err = json.Marshal(config)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(path, data, 0o600))
				case "disabled":
					require.NoError(t, settings.ModifyUserSettings(t.Context(), func(s *settings.UserSettings) error { s.Global.Enabled = false; return nil }))
				case "missing-selected":
					require.NoError(t, os.Remove(selected.Executable))
				case "unselected":
					t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
				}
				transcript := filepath.Join(root, "transcript.jsonl")
				require.NoError(t, os.WriteFile(transcript, []byte(transcriptBody), 0o600))
				payload, err := json.Marshal(map[string]string{"session_id": "scope-once", "transcript_path": transcript, "prompt": "hello"})
				require.NoError(t, err)
				routes := [][]string{{"global", string(name), verb}, {string(name), verb}}
				if state == "replay" {
					routes = routes[1:]
				}
				if state == "repository-first" {
					routes[0], routes[1] = routes[1], routes[0]
				}
				for _, args := range routes {
					cmd := newHooksCmd()
					cmd.SetArgs(args)
					cmd.SetIn(bytes.NewReader(payload))
					cmd.SetOut(&bytes.Buffer{})
					cmd.SetErr(&bytes.Buffer{})
					require.NoError(t, cmd.ExecuteContext(context.Background()))
				}
				session, err := strategy.LoadSessionState(t.Context(), "scope-once")
				require.NoError(t, err)
				require.NotNil(t, session)
				runtimeRoot, err := entiredir.Open(t.Context())
				require.NoError(t, err)
				prompt, err := entiredir.ReadFile(runtimeRoot, sessionMetadataName("scope-once")+"/"+paths.PromptFileName)
				require.NoError(t, err)
				require.Equal(t, "hello", string(prompt), "one event across both scopes must append its prompt once")
			})
		}
	}
}
