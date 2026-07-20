package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Shared hooks.json fixtures. The two-event form exercises the
// trusted/untrusted split; the one-event form is for tests where only
// config.toml parsing behavior matters.
const (
	hooksJSONSessionStartAndPostToolUse = `{
  "hooks": {
    "SessionStart": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}],
    "PostToolUse": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}]
  }
}`
	hooksJSONSessionStartOnly = `{"hooks":{"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"x","timeout":30}]}]}}`
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

	gaps, ok := HookTrustGaps(repoRoot)
	require.True(t, ok)
	require.Equal(t, []string{"post_tool_use"}, gaps)
}

// TestHookTrustGaps_NoGapsWhenAllTrusted returns nil when every declared
// event has a state entry, even if extra entries exist for other paths.
func TestHookTrustGaps_NoGapsWhenAllTrusted(t *testing.T) {
	hooksJSON := hooksJSONSessionStartAndPostToolUse
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

	gaps, ok := HookTrustGaps(repoRoot)
	require.True(t, ok)
	require.Empty(t, gaps)
}

// TestHookTrustGaps_NilWhenHooksJSONMissing — Codex isn't enabled in
// this repo. Stay silent rather than mid-flow noise.
func TestHookTrustGaps_NilWhenHooksJSONMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)
	gaps, ok := HookTrustGaps(tmp)
	require.Nil(t, gaps)
	require.False(t, ok)
}

// TestHookTrustGaps_NilWhenConfigUnreadable — first-run users have no
// config.toml yet. Codex's own startup warning still fires for them, so
// our partial detection staying quiet is the right behavior; we'd
// otherwise duplicate the warning.
func TestHookTrustGaps_NilWhenConfigUnreadable(t *testing.T) {
	hooksJSON := hooksJSONSessionStartOnly
	tmp := t.TempDir()
	codexHome := filepath.Join(tmp, "codex-home")
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "repo", ".codex"), 0o750))
	require.NoError(t, os.MkdirAll(codexHome, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "repo", ".codex", "hooks.json"), []byte(hooksJSON), 0o600))
	t.Setenv("CODEX_HOME", codexHome)

	gaps, ok := HookTrustGaps(filepath.Join(tmp, "repo"))
	require.Nil(t, gaps)
	require.False(t, ok)
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
	gaps, ok := HookTrustGaps(repoRoot)
	require.True(t, ok)
	require.Empty(t, gaps)
}

// TestHookTrustGaps_SingleQuotedStateKeys — current Codex serializes
// state keys as TOML literal (single-quoted) strings, the natural form
// on Windows where backslashes need no escaping. The trust check must
// recognize them the same as double-quoted keys (issue #1761).
func TestHookTrustGaps_SingleQuotedStateKeys(t *testing.T) {
	hooksJSON := hooksJSONSessionStartAndPostToolUse
	repoRoot, hooksPath := writeTrustFixture(t, hooksJSON)

	configTOML := `[hooks.state.'` + hooksPath + `:session_start:0:0']
trusted_hash = "sha256:aaa"

[hooks.state.'` + hooksPath + `:post_tool_use:0:0']
trusted_hash = "sha256:bbb"
`
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(configTOML), 0o600))

	gaps, ok := HookTrustGaps(repoRoot)
	require.True(t, ok)
	require.Empty(t, gaps)
}

// TestHookTrustGaps_SingleQuotedFlagsMissingEvent — literal-quoted keys
// must not only count as trust; a genuinely missing event must still be
// flagged when the other entries are single-quoted.
func TestHookTrustGaps_SingleQuotedFlagsMissingEvent(t *testing.T) {
	hooksJSON := hooksJSONSessionStartAndPostToolUse
	repoRoot, hooksPath := writeTrustFixture(t, hooksJSON)

	configTOML := `[hooks.state.'` + hooksPath + `:session_start:0:0']
trusted_hash = "sha256:aaa"
`
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(configTOML), 0o600))

	gaps, ok := HookTrustGaps(repoRoot)
	require.True(t, ok)
	require.Equal(t, []string{"post_tool_use"}, gaps)
}

// TestHookTrustGaps_MalformedConfigStaysSilent — a config.toml that
// isn't valid TOML is a "can't tell" case, not a "nothing trusted"
// case. The old regex scraper degraded it to zero trusted keys and
// flagged every hook; structural parsing must report ok=false instead
// (same contract as an unreadable file) so doctor can say "not
// verified" rather than claiming OK.
func TestHookTrustGaps_MalformedConfigStaysSilent(t *testing.T) {
	hooksJSON := hooksJSONSessionStartOnly
	repoRoot, _ := writeTrustFixture(t, hooksJSON)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte("[hooks.state.\"unterminated\n"), 0o600))
	gaps, ok := HookTrustGaps(repoRoot)
	require.Nil(t, gaps)
	require.False(t, ok)
}

// TestHookTrustGaps_ConfigWithoutStateSection — a parseable config that
// simply has no hooks.state table is the genuine "fresh clone, nothing
// approved yet" state: the gap warning must fire.
func TestHookTrustGaps_ConfigWithoutStateSection(t *testing.T) {
	hooksJSON := hooksJSONSessionStartOnly
	repoRoot, _ := writeTrustFixture(t, hooksJSON)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte("model = \"gpt-5\"\n"), 0o600))
	gaps, ok := HookTrustGaps(repoRoot)
	require.True(t, ok)
	require.Equal(t, []string{"session_start"}, gaps)
}

// The readCodexTrustedKeys tests below hardcode Windows-shaped keys.
// Full-flow HookTrustGaps tests can't exercise backslash paths on a
// POSIX dev machine (filepath.Join emits "/"), but key parsing is
// OS-independent — this is exactly the layer issue #1761 lives in.

// TestReadCodexTrustedKeys_WindowsLiteralKeys — the reported bug: a
// literal (single-quoted) key holding a Windows path must come back
// verbatim, backslashes intact.
func TestReadCodexTrustedKeys_WindowsLiteralKeys(t *testing.T) {
	t.Parallel()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	configTOML := `[hooks.state.'C:\repo\.codex\hooks.json:session_start:0:0']
trusted_hash = "sha256:aaa"
`
	require.NoError(t, os.WriteFile(configPath, []byte(configTOML), 0o600))

	keys, ok := readCodexTrustedKeys(configPath)
	require.True(t, ok)
	require.Contains(t, keys, `C:\repo\.codex\hooks.json:session_start:0:0`)
}

// TestReadCodexTrustedKeys_WindowsBasicEscapedKeys — the latent second
// bug from issue #1761: a basic (double-quoted) key holding a Windows
// path carries escaped backslashes in the raw file. The parser must
// unescape them so the key compares equal to filepath.Join output. The
// old regex captured the raw `C:\\repo\\...` text and could never
// match.
func TestReadCodexTrustedKeys_WindowsBasicEscapedKeys(t *testing.T) {
	t.Parallel()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	configTOML := `[hooks.state."C:\\repo\\.codex\\hooks.json:stop:0:0"]
trusted_hash = "sha256:bbb"
`
	require.NoError(t, os.WriteFile(configPath, []byte(configTOML), 0o600))

	keys, ok := readCodexTrustedKeys(configPath)
	require.True(t, ok)
	require.Contains(t, keys, `C:\repo\.codex\hooks.json:stop:0:0`)
}

// TestReadCodexTrustedKeys_DottedAssignmentForm — trust entries written
// as dotted-key assignments under a [hooks.state] table instead of one
// header per entry are the same data in valid TOML; structural parsing
// handles the shape the old header regex could never see.
func TestReadCodexTrustedKeys_DottedAssignmentForm(t *testing.T) {
	t.Parallel()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	configTOML := `[hooks.state]
'C:\repo\.codex\hooks.json:post_tool_use:0:0'.trusted_hash = "sha256:ccc"
`
	require.NoError(t, os.WriteFile(configPath, []byte(configTOML), 0o600))

	keys, ok := readCodexTrustedKeys(configPath)
	require.True(t, ok)
	require.Contains(t, keys, `C:\repo\.codex\hooks.json:post_tool_use:0:0`)
}

// TestReadCodexTrustedKeys_UnrelatedSectionsIgnored — keys elsewhere in
// the config (top-level settings, other tables) must not leak into the
// trusted set.
func TestReadCodexTrustedKeys_UnrelatedSectionsIgnored(t *testing.T) {
	t.Parallel()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	configTOML := `model = "gpt-5"

[projects."/some/repo"]
trust_level = "trusted"

[hooks.state."/repo/.codex/hooks.json:stop:0:0"]
trusted_hash = "sha256:ddd"
`
	require.NoError(t, os.WriteFile(configPath, []byte(configTOML), 0o600))

	keys, ok := readCodexTrustedKeys(configPath)
	require.True(t, ok)
	require.Len(t, keys, 1)
	require.Contains(t, keys, "/repo/.codex/hooks.json:stop:0:0")
}
