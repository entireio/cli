package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeTrustFixture sets up the .codex/hooks.json fixture and points
// CODEX_HOME at an isolated temp directory so HookTrustGaps resolves
// the user config without touching ~/.codex on the dev machine. Tests
// that need a config.toml write it themselves into CODEX_HOME after
// the call.
func writeTrustFixture(t *testing.T, hooksJSON string) (repoRoot, hooksPath string) {
	t.Helper()
	tmp := t.TempDir()
	repoRoot = filepath.Join(tmp, "repo")
	codexHome := filepath.Join(tmp, "codex-home")
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".codex"), 0o750))
	require.NoError(t, os.MkdirAll(codexHome, 0o750))

	hooksPath = filepath.Join(repoRoot, ".codex", "hooks.json")
	require.NoError(t, os.WriteFile(hooksPath, []byte(hooksJSON), 0o600))

	t.Setenv("CODEX_HOME", codexHome)
	return repoRoot, hooksPath
}

// TestHookTrustGaps_FlagsMissingEvent is the primary case: the user
// trusted three hooks last month, then entire shipped a fourth. The
// state.toml has three entries; the new event has no key. Detection
// must surface the missing event so the SessionStart banner can prompt
// the user to /hooks.
func TestHookTrustGaps_FlagsMissingEvent(t *testing.T) {
	hooksJSON := `{
  "hooks": {
    "SessionStart": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}],
    "UserPromptSubmit": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}],
    "Stop": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}],
    "PostToolUse": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}]
  }
}`
	repoRoot, hooksPath := writeTrustFixture(t, hooksJSON)

	configTOML := `[hooks.state."` + hooksPath + `:session_start:0:0"]
trusted_hash = "sha256:aaa"

[hooks.state."` + hooksPath + `:user_prompt_submit:0:0"]
trusted_hash = "sha256:bbb"

[hooks.state."` + hooksPath + `:stop:0:0"]
trusted_hash = "sha256:ccc"
`
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(configTOML), 0o600))

	gaps := HookTrustGaps(repoRoot)
	require.Equal(t, []string{"post_tool_use"}, gaps)
}

// TestHookTrustGaps_NoGapsWhenAllTrusted returns nil when every declared
// event has a state entry, even if extra entries exist for other paths.
func TestHookTrustGaps_NoGapsWhenAllTrusted(t *testing.T) {
	hooksJSON := `{
  "hooks": {
    "SessionStart": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}],
    "PostToolUse": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}]
  }
}`
	repoRoot, hooksPath := writeTrustFixture(t, hooksJSON)

	// Trust both, plus an unrelated entry from another repo to make sure
	// readCodexTrustedKeys doesn't get confused by parallel installs.
	configTOML := `[hooks.state."` + hooksPath + `:session_start:0:0"]
trusted_hash = "sha256:aaa"

[hooks.state."` + hooksPath + `:post_tool_use:0:0"]
trusted_hash = "sha256:bbb"

[hooks.state."/some/other/repo/.codex/hooks.json:session_start:0:0"]
trusted_hash = "sha256:ccc"
`
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(configTOML), 0o600))

	gaps := HookTrustGaps(repoRoot)
	require.Empty(t, gaps)
}

// TestHookTrustGaps_NilWhenHooksJSONMissing — Codex isn't enabled in
// this repo. Stay silent rather than mid-flow noise.
func TestHookTrustGaps_NilWhenHooksJSONMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)
	require.Nil(t, HookTrustGaps(tmp))
}

// TestHookTrustGaps_NilWhenConfigUnreadable — first-run users have no
// config.toml yet. Codex's own startup warning still fires for them, so
// our partial detection staying quiet is the right behavior; we'd
// otherwise duplicate the warning.
func TestHookTrustGaps_NilWhenConfigUnreadable(t *testing.T) {
	hooksJSON := `{"hooks":{"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"x","timeout":30}]}]}}`
	tmp := t.TempDir()
	codexHome := filepath.Join(tmp, "codex-home")
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "repo", ".codex"), 0o750))
	require.NoError(t, os.MkdirAll(codexHome, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "repo", ".codex", "hooks.json"), []byte(hooksJSON), 0o600))
	t.Setenv("CODEX_HOME", codexHome)

	require.Nil(t, HookTrustGaps(filepath.Join(tmp, "repo")))
}

// TestMissingEntireHooks_FlagsStaleFile — user enabled Codex on an
// older release that didn't include PostToolUse. Their hooks.json has
// the three legacy events but the CLI now installs four. Detection
// must surface the gap so doctor can prompt `entire enable`.
func TestMissingEntireHooks_FlagsStaleFile(t *testing.T) {
	hooksJSON := `{"hooks":{
		"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-start","timeout":30}]}],
		"UserPromptSubmit":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex user-prompt-submit","timeout":30}]}],
		"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop","timeout":30}]}]
	}}`
	repoRoot, _ := writeTrustFixture(t, hooksJSON)
	require.Equal(t, []string{"post_tool_use"}, MissingEntireHooks(repoRoot))
}

// TestMissingEntireHooks_NilWhenAllPresent returns nil when every
// canonical event has an Entire-managed hook command, even if the file
// also contains unrelated user-defined entries.
func TestMissingEntireHooks_NilWhenAllPresent(t *testing.T) {
	hooksJSON := `{"hooks":{
		"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-start","timeout":30}]}],
		"UserPromptSubmit":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex user-prompt-submit","timeout":30}]}],
		"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop","timeout":30}]},
		        {"matcher":null,"hooks":[{"type":"command","command":"my-custom-tool","timeout":30}]}],
		"PostToolUse":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex post-tool-use","timeout":30}]}]
	}}`
	repoRoot, _ := writeTrustFixture(t, hooksJSON)
	require.Empty(t, MissingEntireHooks(repoRoot))
}

// TestMissingEntireHooks_NilWhenFileMissing — Codex isn't enabled for
// this repo. Stay silent so doctor doesn't tell users to refresh hooks
// they never installed.
func TestMissingEntireHooks_NilWhenFileMissing(t *testing.T) {
	require.Nil(t, MissingEntireHooks(t.TempDir()))
}

// TestMissingEntireHooks_IgnoresNonEntireCommands — a hooks.json that
// declares the right events but with non-Entire commands (e.g. user's
// own scripts) should still flag those events as missing the
// CLI-managed install.
func TestMissingEntireHooks_IgnoresNonEntireCommands(t *testing.T) {
	hooksJSON := `{"hooks":{
		"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"my-other-tool","timeout":30}]}],
		"UserPromptSubmit":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex user-prompt-submit","timeout":30}]}],
		"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop","timeout":30}]}],
		"PostToolUse":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex post-tool-use","timeout":30}]}]
	}}`
	repoRoot, _ := writeTrustFixture(t, hooksJSON)
	require.Equal(t, []string{"session_start"}, MissingEntireHooks(repoRoot))
}

// TestHookTrustGaps_HandlesNonzeroHandlerIndex — the state-key prefix
// match uses "<path>:<event>:" so any group/handler index counts as
// trust. Pin that explicitly: a non-default index of `0:1` (second
// handler in first group) should still satisfy the gap check.
func TestHookTrustGaps_HandlesNonzeroHandlerIndex(t *testing.T) {
	hooksJSON := `{"hooks":{"PostToolUse":[{"matcher":null,"hooks":[{"type":"command","command":"x","timeout":30}]}]}}`
	repoRoot, hooksPath := writeTrustFixture(t, hooksJSON)
	configTOML := `[hooks.state."` + hooksPath + `:post_tool_use:0:1"]
trusted_hash = "sha256:aaa"
`
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(configTOML), 0o600))
	require.Empty(t, HookTrustGaps(repoRoot))
}
