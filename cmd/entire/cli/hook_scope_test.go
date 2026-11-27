package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// scopedHookCmd is a hook verb under `entire hooks` with --scope parsed.
func scopedHookCmd(t *testing.T, scope string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "hooks"}
	root.PersistentFlags().String(hooksScopeFlag, "", "")
	verb := &cobra.Command{Use: "stop", RunE: func(*cobra.Command, []string) error { return nil }}
	root.AddCommand(verb)
	args := []string{"stop"}
	if scope != "" {
		args = append(args, "--"+hooksScopeFlag+"="+scope)
	}
	root.SetArgs(args)
	root.SetContext(context.Background())
	require.NoError(t, root.Execute())
	return verb
}

// A Codex hook from the user-level hooks.json steps aside exactly when the
// checkout's own Entire hooks are installed and approved; a project hook
// (no --scope) never does.
func TestUserScopeStepsAside_CodexFollowsTheTrustedProjectInstall(t *testing.T) {
	t.Cleanup(osroot.ResetShared)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	repo := t.TempDir()
	testutil.InitRepo(t, repo)
	t.Chdir(repo)

	userScope := codex.UserScopePrefix + "1"
	assert.False(t, userScopeStepsAside(scopedHookCmd(t, userScope), agent.AgentNameCodex), "no project install")

	_, err := (&codex.CodexAgent{}).InstallHooks(context.Background(), false)
	require.NoError(t, err)
	assert.False(t, userScopeStepsAside(scopedHookCmd(t, userScope), agent.AgentNameCodex), "project hooks not approved")

	discovered := codex.ResolveHookDiscovery(context.Background()).DiscoveredHooks.Path()
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(codexApprovalRecords(t, discovered)), 0o600))

	assert.True(t, userScopeStepsAside(scopedHookCmd(t, userScope), agent.AgentNameCodex), "approved project hooks take over")
	assert.False(t, userScopeStepsAside(scopedHookCmd(t, ""), agent.AgentNameCodex), "a project hook never steps aside")
}

// With global enable on, every agent's hooks run in every directory the
// agent is started in, including ones outside any git repository. There they
// do nothing and fail nothing.
func TestAgentHooks_OutsideARepositoryDoNothing(t *testing.T) {
	entireDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", entireDir)
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, usersettings.FileName), []byte(`{"global_enable": true}`), 0o600))
	isolateAgentUserHomes(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "absent"))
	dir := t.TempDir()
	t.Chdir(dir)

	for _, args := range [][]string{
		{"hooks", "claude-code", "session-start"},
		{"hooks", "claude-code", "user-prompt-submit"},
		{"hooks", "codex", "--scope=" + codex.UserScopePrefix + "1", "session-start"},
		{"hooks", "pi", "session_start"},
		{"hooks", "opencode", "session-start"},
	} {
		root := NewRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetIn(strings.NewReader(`{"session_id":"s1","prompt":"hi"}`))
		root.SetArgs(args)
		require.NoError(t, root.Execute(), "%v: %s", args, out.String())
		assert.Empty(t, out.String(), "%v", args)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing written outside a repository")
}

// The lazy setup on an agent's first prompt never installs git hooks where
// the repository is off despite global enable: a committed `enabled: false`,
// or a `repos` entry from `entire disable`.
func TestTurnStart_GlobalEnableDoesNotSetUpADisabledRepository(t *testing.T) {
	for name, disable := range map[string]func(t *testing.T, repo string){
		"control: not disabled": nil,
		"committed enabled false": func(t *testing.T, repo string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(repo, ".entire"), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(repo, ".entire", "settings.json"), []byte(`{"enabled": false}`), 0o600))
		},
		"repos entry disable": func(t *testing.T, repo string) {
			t.Helper()
			_, err := settings.SetRepoEnabled(context.Background(), repo, false)
			require.NoError(t, err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			entireDir := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", entireDir)
			require.NoError(t, os.WriteFile(filepath.Join(entireDir, usersettings.FileName), []byte(`{"global_enable": true}`), 0o600))
			isolateAgentUserHomes(t)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "absent"))
			t.Cleanup(settings.ClearOriginKeyCache)
			repo := t.TempDir()
			testutil.InitRepo(t, repo)
			t.Chdir(repo)
			if disable != nil {
				disable(t, repo)
			}
			settings.ClearOriginKeyCache()

			root := NewRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetIn(strings.NewReader(`{"session_id":"s1","prompt":"hi","transcript_path":""}`))
			root.SetArgs([]string{"hooks", "claude-code", "user-prompt-submit"})
			require.NoError(t, root.Execute(), out.String())
			if disable == nil {
				assert.True(t, strategy.IsGitHookInstalled(context.Background()), "the control repository is set up")
				return
			}
			assert.False(t, strategy.IsGitHookInstalled(context.Background()), "git hooks installed in a disabled repository")
			assert.NoDirExists(t, filepath.Join(repo, ".entire", "metadata"))
		})
	}
}

// A turn-start is skipped as a second install's repeat only where a second
// install can fire: a user-level hook (--scope=user) or an agent that sends
// a turn ID. Claude Code is covered by TestTurnStartMayRepeat_ClaudeCode.
func TestTurnStartMayRepeat(t *testing.T) {
	t.Parallel()
	userCtx := withUserScopeHook(context.Background(), true)
	for name, tc := range map[string]struct {
		ctx    context.Context
		agent  agent.Agent
		turnID string
		want   bool
	}{
		"codex project":         {ctx: context.Background(), agent: mustAgent(t, agent.AgentNameCodex)},
		"codex user-level":      {ctx: userCtx, agent: mustAgent(t, agent.AgentNameCodex), want: true},
		"opencode with turn ID": {ctx: context.Background(), agent: mustAgent(t, agent.AgentNameOpenCode), turnID: "msg-1", want: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := turnStartMayRepeat(tc.ctx, tc.agent, &agent.Event{TurnID: tc.turnID})
			assert.Equal(t, tc.want, got)
		})
	}
}

// Claude Code runs one identical hook command once itself, so a repeat there
// is the developer sending the prompt again, even from a user-level hook with
// a turn ID; only a differing project copy of the prompt hook can fire twice.
func TestTurnStartMayRepeat_ClaudeCode(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	repo := t.TempDir()
	testutil.InitRepo(t, repo)
	t.Chdir(repo)
	claude := mustAgent(t, agent.AgentNameClaudeCode)
	ctx := withUserScopeHook(context.Background(), true)
	event := &agent.Event{TurnID: "t1"}
	current := agent.WrapProductionSilentHookCommand("entire hooks claude-code user-prompt-submit")
	writePromptHook := func(dir, command string) {
		t.Helper()
		content := `{"hooks":{"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":` + strconv.Quote(command) + `}]}]}}`
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0o600))
	}

	writePromptHook(userDir, current)
	assert.False(t, turnStartMayRepeat(ctx, claude, event), "user-level copy only")

	writePromptHook(filepath.Join(repo, ".claude"), current)
	assert.False(t, turnStartMayRepeat(ctx, claude, event), "identical commands run once")

	writePromptHook(filepath.Join(repo, ".claude"), "entire hooks claude-code user-prompt-submit")
	assert.True(t, turnStartMayRepeat(ctx, claude, event), "an older project command fires too")
}

func mustAgent(t *testing.T, name types.AgentName) agent.Agent {
	t.Helper()
	ag, err := agent.Get(name)
	require.NoError(t, err)
	return ag
}

// codexApprovalRecords returns the config.toml records Codex writes once the
// developer approves every hook in the hooks file at hooksPath: each keeps the
// hash of the approved definition (sorted-key compact JSON of the handler
// under its event label, SHA-256). Entire's handlers carry only type, command
// and timeout, under a null matcher.
func codexApprovalRecords(t *testing.T, hooksPath string) string {
	t.Helper()
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Timeout uint64 `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	type handler struct {
		Async   bool   `json:"async"`
		Command string `json:"command"`
		Timeout uint64 `json:"timeout"`
		Type    string `json:"type"`
	}
	var records strings.Builder
	for _, spec := range codex.HookEventSpecs() {
		for gi, group := range doc.Hooks[spec.Event] {
			for hi, h := range group.Hooks {
				var identity bytes.Buffer
				encoder := json.NewEncoder(&identity)
				encoder.SetEscapeHTML(false)
				require.NoError(t, encoder.Encode(struct {
					EventName string    `json:"event_name"`
					Hooks     []handler `json:"hooks"`
				}{spec.Label, []handler{{Command: h.Command, Timeout: h.Timeout, Type: "command"}}}))
				sum := sha256.Sum256(bytes.TrimSuffix(identity.Bytes(), []byte("\n")))
				fmt.Fprintf(&records, "[hooks.state.%q]\ntrusted_hash = \"sha256:%s\"\n",
					fmt.Sprintf("%s:%s:%d:%d", hooksPath, spec.Label, gi, hi), hex.EncodeToString(sum[:]))
			}
		}
	}
	return records.String()
}
