package review

import (
	"encoding/json"
	"strings"
	"testing"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

func TestValidateAgentConfig(t *testing.T) {
	t.Parallel()

	roots := []string{"/repo", "/repo/.entire/worktrees/review-x"}
	hook := func(command string) json.RawMessage {
		return json.RawMessage(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":` + quote(command) + `}]}]}}`)
	}
	mcp := func(def string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"docs": json.RawMessage(def)}
	}
	tests := []struct {
		name    string
		agent   string
		cfg     reviewtypes.AgentConfig
		wantErr string
	}{
		{"absolute hook outside the checkout", "claude-code", reviewtypes.AgentConfig{Settings: hook("/usr/local/bin/notify --quiet")}, ""},
		{"bare tool name", "claude-code", reviewtypes.AgentConfig{Settings: hook("jq .")}, ""},
		{"relative hook path", "claude-code", reviewtypes.AgentConfig{Settings: hook("./scripts/hook.sh")}, "relative path"},
		{"hook inside the checkout", "claude-code", reviewtypes.AgentConfig{Settings: hook("/repo/scripts/hook.sh")}, "inside a checkout"},
		{"project dir variable", "claude-code", reviewtypes.AgentConfig{Settings: hook(`"$CLAUDE_PROJECT_DIR"/x.sh`)}, "project directory"},
		{"project launcher", "claude-code", reviewtypes.AgentConfig{Settings: hook("npx some-tool")}, "resolves tools from the reviewed project"},
		{"argument into checkout", "claude-code", reviewtypes.AgentConfig{Settings: hook("/usr/bin/node ./tools/x.js")}, "points into a checkout"},
		{"apiKeyHelper relative", "claude-code", reviewtypes.AgentConfig{Settings: json.RawMessage(`{"apiKeyHelper":"bin/key"}`)}, "relative path"},
		{"MCP absolute", "codex", reviewtypes.AgentConfig{MCPServers: mcp(`{"command":"/opt/mcp/bin/docs","args":["--stdio"]}`)}, ""},
		{"MCP url", "claude-code", reviewtypes.AgentConfig{MCPServers: mcp(`{"url":"https://mcp.example"}`)}, ""},
		{"MCP inside checkout", "codex", reviewtypes.AgentConfig{MCPServers: mcp(`{"command":"/repo/node_modules/.bin/mcp"}`)}, "inside a checkout"},
		{"MCP bad name", "codex", reviewtypes.AgentConfig{MCPServers: map[string]json.RawMessage{"a.b": json.RawMessage(`{"url":"x"}`)}}, "letters, digits"},
		{"codex settings unsupported", "codex", reviewtypes.AgentConfig{Settings: json.RawMessage(`{}`)}, `does not support "settings"`},
		{"pi MCP unsupported", "pi", reviewtypes.AgentConfig{MCPServers: mcp(`{"url":"x"}`)}, `does not support "mcp_servers"`},
		{"pi extension relative", "pi", reviewtypes.AgentConfig{Extensions: []string{"ext.ts"}}, "absolute path"},
		{"pi extension in checkout", "pi", reviewtypes.AgentConfig{Extensions: []string{"/repo/.pi/x.ts"}}, "inside a checkout"},
		{"unknown agent", "cursor", reviewtypes.AgentConfig{}, "does not support a review profile config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			err := ValidateAgentConfig(tt.agent, &cfg, roots)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateAgentConfig() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateAgentConfig() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// An agent counts as isolated only when every worker on it has a profile
// config, so a mixed profile is never under-reported by the gate.
func TestProfileTrustAgents(t *testing.T) {
	t.Parallel()

	profile := settings.ReviewProfileConfig{Agents: map[string]settings.ReviewConfig{
		"claude-a": {Agent: "claude-code", Config: &settings.ReviewAgentConfig{}},
		"claude-b": {Agent: "claude-code"},
		"pi":       {Config: &settings.ReviewAgentConfig{}},
	}}
	got := profileTrustAgents(profile, "")
	want := map[string]bool{"claude-code": false, "pi": true}
	if len(got) != len(want) {
		t.Fatalf("profileTrustAgents() = %+v", got)
	}
	for _, a := range got {
		if want[a.Name] != a.Isolated {
			t.Errorf("%s isolated = %v, want %v", a.Name, a.Isolated, want[a.Name])
		}
	}
	if only := profileTrustAgents(profile, "claude-a"); len(only) != 1 || !only[0].Isolated {
		t.Errorf("--agent claude-a = %+v, want one isolated agent", only)
	}
}

func TestTrustConfirmTextForProfileConfig(t *testing.T) {
	t.Parallel()

	subject := foreignSubject()
	inv := TrustInventory{Isolated: true, Entries: []TrustEntry{
		{Agent: "claude-code", Kind: TrustKindSkill, Name: "review", Command: ".claude/skills/review", Source: ".claude/skills"},
	}}
	title, body := trustConfirmText(subject, inv, "x")
	if title != "Load this branch's skills during the review?" || !strings.Contains(body, "uses your profile's config") {
		t.Fatalf("title/body = %q / %q", title, body)
	}
	if title, body := trustConfirmText(subject, TrustInventory{Isolated: true}, "x"); title != "Review this branch?" || !strings.Contains(body, "uses your profile's config") {
		t.Fatalf("empty isolated title/body = %q / %q", title, body)
	}
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
