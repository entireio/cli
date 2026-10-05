package codex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeTrustFixture sets up the .codex/hooks.json fixture and points
// CODEX_HOME at an isolated temp directory so HookTrustGaps resolves
// the user config without touching ~/.codex on the dev machine. Tests
// that need a config.toml write it themselves into CODEX_HOME after
// the call.
func writeTrustFixture(t *testing.T, hooksJSON string) string {
	t.Helper()
	tmp := t.TempDir()
	repoRoot := filepath.Join(tmp, "repo")
	codexHome := filepath.Join(tmp, "codex-home")
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".codex"), 0o750))
	require.NoError(t, os.MkdirAll(codexHome, 0o750))

	hooksPath := filepath.Join(repoRoot, ".codex", "hooks.json")
	require.NoError(t, os.WriteFile(hooksPath, []byte(hooksJSON), 0o600))

	t.Setenv("CODEX_HOME", codexHome)
	return hooksPath
}

// tomlBasicEscape escapes s for a TOML basic (double-quoted) string, so
// fixtures stay valid TOML when paths contain backslashes on Windows.
func tomlBasicEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
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
	hooksPath := writeTrustFixture(t, hooksJSON)

	configTOML := `[hooks.state."` + tomlBasicEscape(hooksPath) + `:session_start:0:0"]
trusted_hash = "sha256:aaa"

[hooks.state."` + tomlBasicEscape(hooksPath) + `:user_prompt_submit:0:0"]
trusted_hash = "sha256:bbb"

[hooks.state."` + tomlBasicEscape(hooksPath) + `:stop:0:0"]
trusted_hash = "sha256:ccc"
`
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(configTOML), 0o600))

	gaps := inspectHookTrust(hooksPath).Gaps
	require.Equal(t, []string{"post_tool_use"}, gaps)
}

// TestHookTrustGaps_FlagsUntrustedSessionEnd is the live instance of the case
// above: SessionEnd shipped after users had already trusted the other four, so
// every existing repo has an untrusted session_end entry. Codex silently skips
// untrusted hooks and `codex exec` can never prompt, so without this the new
// hook would do nothing and nothing would say why.
func TestHookTrustGaps_FlagsUntrustedSessionEnd(t *testing.T) {
	hooksJSON := `{
  "hooks": {
    "SessionStart": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}],
    "SessionEnd": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":3}]}],
    "UserPromptSubmit": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}],
    "Stop": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}],
    "PostToolUse": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}]
  }
}`
	hooksPath := writeTrustFixture(t, hooksJSON)

	// The trust state a user carries over from before SessionEnd existed.
	configTOML := `[hooks.state."` + tomlBasicEscape(hooksPath) + `:session_start:0:0"]
trusted_hash = "sha256:aaa"

[hooks.state."` + tomlBasicEscape(hooksPath) + `:user_prompt_submit:0:0"]
trusted_hash = "sha256:bbb"

[hooks.state."` + tomlBasicEscape(hooksPath) + `:stop:0:0"]
trusted_hash = "sha256:ccc"

[hooks.state."` + tomlBasicEscape(hooksPath) + `:post_tool_use:0:0"]
trusted_hash = "sha256:ddd"
`
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(configTOML), 0o600))

	require.Equal(t, []string{"session_end"}, inspectHookTrust(hooksPath).Gaps)
}

// TestInspectHookConfig_FlagsAbsentSessionEnd covers the other half of the
// upgrade path: a repo enabled before SessionEnd existed has no such hook in
// hooks.json at all, which `entire doctor` must report as drift. That install
// also predates the subagent hooks, so all three are reported.
func TestInspectHookConfig_FlagsAbsentSessionEnd(t *testing.T) {
	hooksJSON := `{
  "hooks": {
    "SessionStart": [{"matcher": null, "hooks": [{"type":"command","command":"entire hooks codex session-start","timeout":30}]}],
    "UserPromptSubmit": [{"matcher": null, "hooks": [{"type":"command","command":"entire hooks codex user-prompt-submit","timeout":30}]}],
    "Stop": [{"matcher": null, "hooks": [{"type":"command","command":"entire hooks codex stop","timeout":30}]}],
    "PostToolUse": [{"matcher": null, "hooks": [{"type":"command","command":"entire hooks codex post-tool-use","timeout":30}]}]
  }
}`
	hooksPath := writeTrustFixture(t, hooksJSON)

	require.Equal(t, []string{"session_end", "subagent_start", "subagent_stop"}, inspectHookConfigAt(context.Background(), hooksPath).Missing)
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
	hooksPath := writeTrustFixture(t, hooksJSON)

	// Trust both, plus an unrelated entry from another repo to make sure
	// readCodexTrustedKeys doesn't get confused by parallel installs.
	configTOML := `[hooks.state."` + tomlBasicEscape(hooksPath) + `:session_start:0:0"]
trusted_hash = "sha256:aaa"

[hooks.state."` + tomlBasicEscape(hooksPath) + `:post_tool_use:0:0"]
trusted_hash = "sha256:bbb"

[hooks.state."/some/other/repo/.codex/hooks.json:session_start:0:0"]
trusted_hash = "sha256:ccc"
`
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(configTOML), 0o600))

	gaps := inspectHookTrust(hooksPath).Gaps
	require.Empty(t, gaps)
}

// TestHookTrustGaps_NilWhenHooksJSONMissing — Codex isn't enabled in
// this repo. Stay silent rather than mid-flow noise.
func TestHookTrustGaps_NilWhenHooksJSONMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)
	require.Empty(t, inspectHookTrust(filepath.Join(tmp, "hooks.json")).Gaps)
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

	require.Empty(t, inspectHookTrust(filepath.Join(tmp, "repo", ".codex", "hooks.json")).Gaps)
}

// TestInspectHookConfig_FlagsStaleFile — user enabled Codex on an older
// release that predates PostToolUse, SessionEnd and the subagent hooks. Their
// hooks.json has the three oldest events; detection must surface every event
// added since so doctor can prompt `entire enable`.
func TestInspectHookConfig_FlagsStaleFile(t *testing.T) {
	hooksJSON := `{"hooks":{
		"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-start","timeout":30}]}],
		"UserPromptSubmit":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex user-prompt-submit","timeout":30}]}],
		"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop","timeout":30}]}]
	}}`
	hooksPath := writeTrustFixture(t, hooksJSON)
	require.Equal(t, []string{"session_end", "post_tool_use", "subagent_start", "subagent_stop"}, inspectHookConfigAt(context.Background(), hooksPath).Missing)
}

// TestInspectHookConfig_NoMissingWhenAllPresent returns nil when every
// canonical event has an Entire-managed hook command, even if the file
// also contains unrelated user-defined entries.
func TestInspectHookConfig_NoMissingWhenAllPresent(t *testing.T) {
	hooksJSON := `{"hooks":{
		"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-start","timeout":30}]}],
		"SessionEnd":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-end","timeout":3}]}],
		"UserPromptSubmit":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex user-prompt-submit","timeout":30}]}],
		"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop","timeout":30}]},
		        {"matcher":null,"hooks":[{"type":"command","command":"my-custom-tool","timeout":30}]}],
		"PostToolUse":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex post-tool-use","timeout":30}]}],
		"SubagentStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex subagent-start","timeout":30}]}],
		"SubagentStop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex subagent-stop","timeout":30}]}]
	}}`
	hooksPath := writeTrustFixture(t, hooksJSON)
	require.Empty(t, inspectHookConfigAt(context.Background(), hooksPath).Missing)
}

// TestInspectHookConfig_NoMissingWhenFileMissing — Codex isn't enabled for
// this repo. Stay silent so doctor doesn't tell users to refresh hooks
// they never installed.
func TestInspectHookConfig_NoMissingWhenFileMissing(t *testing.T) {
	require.Nil(t, inspectHookConfigAt(context.Background(), filepath.Join(t.TempDir(), "hooks.json")).Missing)
}

// TestInspectHookConfig_IgnoresNonEntireCommands — a hooks.json that
// declares the right events but with non-Entire commands (e.g. user's
// own scripts) should still flag those events as missing the
// CLI-managed install.
func TestInspectHookConfig_IgnoresNonEntireCommands(t *testing.T) {
	hooksJSON := `{"hooks":{
		"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"my-other-tool","timeout":30}]}],
		"SessionEnd":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-end","timeout":3}]}],
		"UserPromptSubmit":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex user-prompt-submit","timeout":30}]}],
		"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop","timeout":30}]}],
		"PostToolUse":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex post-tool-use","timeout":30}]}],
		"SubagentStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex subagent-start","timeout":30}]}],
		"SubagentStop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex subagent-stop","timeout":30}]}]
	}}`
	hooksPath := writeTrustFixture(t, hooksJSON)
	require.Equal(t, []string{"session_start"}, inspectHookConfigAt(context.Background(), hooksPath).Missing)
}

func TestInspectHookConfig_UserOnlyFileIsNotEntireDrift(t *testing.T) {
	hooksPath := writeTrustFixture(t, `{"hooks":{"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"my-user-hook"}]}]}}`)
	require.Nil(t, inspectHookConfigAt(context.Background(), hooksPath).Missing)
}

// TestHookTrustGaps_HandlesNonzeroHandlerIndex — the state-key prefix
// match uses "<path>:<event>:" so any group/handler index counts as
// trust. Pin that explicitly: a non-default index of `0:1` (second
// handler in first group) should still satisfy the gap check.
func TestHookTrustGaps_HandlesNonzeroHandlerIndex(t *testing.T) {
	hooksJSON := `{"hooks":{"PostToolUse":[{"matcher":null,"hooks":[{"type":"command","command":"x","timeout":30}]}]}}`
	hooksPath := writeTrustFixture(t, hooksJSON)
	configTOML := `[hooks.state."` + tomlBasicEscape(hooksPath) + `:post_tool_use:0:1"]
trusted_hash = "sha256:aaa"
`
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(configTOML), 0o600))
	require.Empty(t, inspectHookTrust(hooksPath).Gaps)
}

func TestHookTrustGaps_MatchesLogicalSymlinkPath(t *testing.T) {
	if runtime.GOOS == testWindowsOS {
		t.Skip("directory symlinks require privileges on Windows")
	}
	tmp := t.TempDir()
	physicalRoot := filepath.Join(tmp, "physical", "repo")
	logicalRoot := filepath.Join(tmp, "logical-repo")
	codexHome := filepath.Join(tmp, "codex-home")
	require.NoError(t, os.MkdirAll(filepath.Join(physicalRoot, ".codex"), 0o750))
	require.NoError(t, os.MkdirAll(codexHome, 0o750))
	require.NoError(t, os.Symlink(physicalRoot, logicalRoot))

	physicalHooksPath := filepath.Join(physicalRoot, ".codex", HooksFileName)
	require.NoError(t, os.WriteFile(physicalHooksPath, []byte(`{
  "hooks": {
    "SessionStart": [{"matcher": null, "hooks": [{"type":"command","command":"x","timeout":30}]}]
  }
}`), 0o600))
	logicalHooksPath := filepath.Join(logicalRoot, ".codex", HooksFileName)
	configTOML := `[hooks.state."` + tomlBasicEscape(logicalHooksPath) + `:session_start:0:0"]
trusted_hash = "sha256:aaa"
`
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(configTOML), 0o600))
	t.Setenv("CODEX_HOME", codexHome)

	require.Empty(t, inspectHookTrust(physicalHooksPath).Gaps)
}

// codexTrustQuoteStyles are the two TOML quoted-key forms Codex's writer
// emits: a basic string on Unix, and a literal string whenever the key
// contains a backslash (every Windows path).
var codexTrustQuoteStyles = []struct {
	name   string
	header func(key string) string
}{
	{"double-quoted", func(key string) string { return `[hooks.state."` + tomlBasicEscape(key) + `"]` }},
	{"single-quoted", func(key string) string { return `[hooks.state.'` + key + `']` }},
}

// TestHookTrustGaps_EitherQuoteStyle runs the trust check with every approval
// record written in each quoting style Codex uses, alongside an unrelated
// Windows-style record. Both styles must resolve to the same keys.
func TestHookTrustGaps_EitherQuoteStyle(t *testing.T) {
	hooksJSON := `{"hooks":{
		"SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"x","timeout":30}]}],
		"PostToolUse":[{"matcher":null,"hooks":[{"type":"command","command":"x","timeout":30}]}]
	}}`
	for _, style := range codexTrustQuoteStyles {
		t.Run(style.name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				trusted []string
				gaps    []string
			}{
				{"all trusted", []string{"session_start", "post_tool_use"}, nil},
				{"one missing", []string{"session_start"}, []string{"post_tool_use"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					hooksPath := writeTrustFixture(t, hooksJSON)
					var configTOML strings.Builder
					configTOML.WriteString(style.header(`C:\Users\someone\project\.codex\hooks.json:post_tool_use:0:0`) + "\ntrusted_hash = \"sha256:000\"\n\n")
					for _, ev := range tc.trusted {
						configTOML.WriteString(style.header(hooksPath+":"+ev+":0:0") + "\ntrusted_hash = \"sha256:aaa\"\n\n")
					}
					require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(configTOML.String()), 0o600))

					inspection := inspectHookTrust(hooksPath)
					require.True(t, inspection.Known)
					require.Equal(t, tc.gaps, inspection.Gaps)
				})
			}
		})
	}
}

// TestHookTrustGaps_UnescapesDoubleQuotedKeys — a double-quoted key is a TOML
// basic string, so a Windows path in one has escaped backslashes that must be
// decoded before comparing with the hooks.json path.
func TestHookTrustGaps_UnescapesDoubleQuotedKeys(t *testing.T) {
	hooksJSON := `{"hooks":{"PostToolUse":[{"matcher":null,"hooks":[{"type":"command","command":"x","timeout":30}]}]}}`
	hooksPath := writeTrustFixture(t, hooksJSON)
	// A symlink-free alias of hooksPath that contains a backslash on every OS:
	// on Windows filepath separators are backslashes already; elsewhere add a
	// directory whose name contains one.
	aliasPath := hooksPath
	if runtime.GOOS != testWindowsOS {
		dir := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(hooksPath))), `back\slash`, ".codex")
		require.NoError(t, os.MkdirAll(dir, 0o750))
		aliasPath = filepath.Join(dir, HooksFileName)
		require.NoError(t, os.WriteFile(aliasPath, []byte(hooksJSON), 0o600))
	}
	require.Contains(t, aliasPath, `\`)
	configTOML := `[hooks.state."` + tomlBasicEscape(aliasPath) + `:post_tool_use:0:0"]
trusted_hash = "sha256:aaa"
`
	require.Contains(t, configTOML, `\\`)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(configTOML), 0o600))

	require.Empty(t, inspectHookTrust(aliasPath).Gaps)
}

// TestReadCodexTrustedKeys_DecodesQuotedKeys pins the key text for each TOML
// quoting style Codex may write, and rejects bare keys and escapes TOML does
// not define (including Go-only ones strconv.Unquote would accept).
func TestReadCodexTrustedKeys_DecodesQuotedKeys(t *testing.T) {
	t.Parallel()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	configTOML := `[hooks.state.'C:\Users\Victor\repo\.codex\hooks.json:post_tool_use:0:0']
[hooks.state."C:\\Users\\Victor\\repo\\.codex\\hooks.json:stop:0:0"]
[hooks.state."/repo/say \"hi\"/.codex/hooks.json:stop:0:0"]
[hooks.state.bare-key]
[hooks.state."/repo/caf\u00e9/.codex/hooks.json:stop:0:0"]
[hooks.state."bad \q escape:stop:0:0"]
[hooks.state."C:\x5cUsers\x5cVictor\x5crepo\x5c.codex\x5chooks.json:session_start:0:0"]
[hooks.state."/repo/go-only \a \v \101 escapes:stop:0:0"]
`
	require.NoError(t, os.WriteFile(configPath, []byte(configTOML), 0o600))

	keys, ok := readCodexTrustedKeys(configPath)
	require.True(t, ok)
	require.Equal(t, map[string]struct{}{
		`C:\Users\Victor\repo\.codex\hooks.json:post_tool_use:0:0`: {},
		`C:\Users\Victor\repo\.codex\hooks.json:stop:0:0`:          {},
		`/repo/say "hi"/.codex/hooks.json:stop:0:0`:                {},
		`/repo/café/.codex/hooks.json:stop:0:0`:                    {},
	}, keys)
}
