//go:build integration && !windows

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// A review profile's agent config, driven through the real binary against
// fake claude and codex launchers that record what they were started with.
// The branch commits its own hooks and MCP servers; the reviewers must get the
// profile's instead, still carry Entire's tracking hooks, and the run must
// report the tool each fake says was blocked. No real agent runs.

// fakeReviewer records each argument, and the contents of the files passed to
// --settings and --mcp-config (removed when the reviewer exits), to $ARGV_LOG,
// then prints output.
func fakeReviewer(t *testing.T, dir, name, output string) {
	t.Helper()
	script := `#!/bin/sh
prev=""
for arg in "$@"; do
	printf 'ARG %s\n' "$arg" >> "$ARGV_LOG"
	case "$prev" in --settings|--mcp-config) cat "$arg" >> "$ARGV_LOG"; printf '\n' >> "$ARGV_LOG" ;; esac
	prev="$arg"
done
cat > /dev/null
cat <<'EOF'
` + output + `
EOF
`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func TestReviewProfileConfig_ReviewersGetProfileConfig(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	enableReviewAgent(t, env, agentClaudeCode)
	enableReviewAgent(t, env, "codex")
	// The branch's own config, which the reviewers must not be handed.
	env.WriteFile(".mcp.json", `{"mcpServers":{"branchmcp":{"command":"/branch/mcp"}}}`)
	env.WriteSettings(map[string]any{
		"enabled":                true,
		"review_default_profile": "general",
		"review_profiles": map[string]any{
			"general": map[string]any{
				"task": "Review.",
				"agents": map[string]any{
					agentClaudeCode: map[string]any{"skills": []string{"/review"}},
					"codex":         map[string]any{"skills": []string{"/review"}},
				},
			},
		},
	})

	cfgDir := t.TempDir()
	writeConfig := func(name string, cfg any) string {
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(cfgDir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	claudeCfg := writeConfig("claude.json", map[string]any{
		"settings":    map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/opt/profile/stop-hook"}}}}}},
		"mcp_servers": map[string]any{"docs": map[string]any{"command": "/opt/profile/docs"}},
	})
	codexCfg := writeConfig("codex.json", map[string]any{
		"mcp_servers": map[string]any{"docs": map[string]any{"command": "/opt/profile/docs", "default_tools_approval_mode": "approve"}},
	})
	for _, set := range []string{agentClaudeCode + "=" + claudeCfg, "codex=" + codexCfg} {
		env.RunCLI("review", "--configure", "general", "--set-config", set)
	}

	fakeBin := t.TempDir()
	fakeReviewer(t, fakeBin, "claude",
		`{"type":"result","subtype":"success","is_error":false,"permission_denials":[{"tool_name":"mcp__docs__search"}]}`)
	fakeReviewer(t, fakeBin, "codex",
		`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"docs","tool":"search","status":"failed","error":{"message":"MCP tool call requires approval, but approval policy is never"}}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)

	review := func(agentName string) (log, output string) {
		argvLog := filepath.Join(t.TempDir(), "argv.log")
		cmd := execx.NonInteractive(context.Background(), getTestBinary(), "review", "general", "--agent", agentName)
		cmd.Dir = env.RepoDir
		cmd.Env = envWithOverrides(env.cliEnv(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"ARGV_LOG="+argvLog,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("entire review --agent %s: %v\n%s", agentName, err, out)
		}
		data, err := os.ReadFile(argvLog)
		if err != nil {
			t.Fatalf("%s was not started: %v\n%s", agentName, err, out)
		}
		return string(data), string(out)
	}
	expect := func(agentName, text string, want, notWant []string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: missing %q in:\n%s", agentName, w, text)
			}
		}
		for _, w := range notWant {
			if strings.Contains(text, w) {
				t.Errorf("%s: unexpected %q in:\n%s", agentName, w, text)
			}
		}
	}

	log, out := review(agentClaudeCode)
	expect(agentClaudeCode, log,
		[]string{
			"ARG --setting-sources\nARG user", "ARG --strict-mcp-config",
			"/opt/profile/stop-hook",   // the profile's hook, in the --settings file
			"entire hooks claude-code", // Entire's tracking hooks, added to it
			`"docs"`,                   // the profile's MCP server, in the --mcp-config file
		},
		[]string{"branchmcp"})
	expect(agentClaudeCode, out, []string{"using your profile's agent config", "blocked mcp__docs__search", "permissions.allow"}, nil)

	log, out = review("codex")
	expect("codex", log,
		[]string{
			`trust_level="untrusted"`,
			`ARG mcp_servers.docs.command="/opt/profile/docs"`,
			`ARG mcp_servers.docs.default_tools_approval_mode="approve"`,
			"ARG hooks.SessionStart=", "entire hooks codex",
			"ARG --dangerously-bypass-hook-trust",
		},
		[]string{"branchmcp"})
	expect("codex", out, []string{"using your profile's agent config", "blocked docs.search", "default_tools_approval_mode"}, nil)
}
