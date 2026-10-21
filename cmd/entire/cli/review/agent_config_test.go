package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestValidateAgentConfig(t *testing.T) {
	t.Parallel()

	mcp := func(def string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"docs": json.RawMessage(def)}
	}
	tests := []struct {
		name    string
		agent   string
		cfg     reviewtypes.AgentConfig
		wantErr string
	}{
		// Commands are the user's own and are not vetted, launchers included.
		{"claude hook with a launcher", "claude-code", reviewtypes.AgentConfig{Settings: json.RawMessage(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"yarn lint"}]}]}}`)}, ""},
		{"MCP command", "codex", reviewtypes.AgentConfig{MCPServers: mcp(`{"command":"npx","args":["server"]}`)}, ""},
		{"MCP url", "claude-code", reviewtypes.AgentConfig{MCPServers: mcp(`{"url":"https://mcp.example"}`)}, ""},
		{"MCP command and url", "codex", reviewtypes.AgentConfig{MCPServers: mcp(`{"command":"/opt/mcp","url":"https://mcp.example"}`)}, "not both"},
		{"MCP without command or url", "claude-code", reviewtypes.AgentConfig{MCPServers: mcp(`{"args":["x"]}`)}, "needs a command or a url"},
		{"codex MCP env", "codex", reviewtypes.AgentConfig{MCPServers: mcp(`{"command":"/opt/mcp","env":{"TOKEN":"x"}}`)}, "env values aren't supported for Codex"},
		{"codex MCP header dropped", "codex", reviewtypes.AgentConfig{MCPServers: mcp(`{"url":"https://mcp.example","headers":{"Authorization":"x"}}`)}, `"headers" isn't supported for Codex`},
		{"codex MCP type", "codex", reviewtypes.AgentConfig{MCPServers: mcp(`{"type":"stdio","command":"/opt/mcp","default_tools_approval_mode":"approve"}`)}, ""},
		{"claude MCP env", "claude-code", reviewtypes.AgentConfig{MCPServers: mcp(`{"command":"/opt/mcp","env":{"TOKEN":"x"}}`)}, ""},
		{"MCP bad name", "codex", reviewtypes.AgentConfig{MCPServers: map[string]json.RawMessage{"a.b": json.RawMessage(`{"url":"x"}`)}}, "letters, digits"},
		{"settings null", "claude-code", reviewtypes.AgentConfig{Settings: json.RawMessage(`null`)}, "not null"},
		{"settings hooks null", "claude-code", reviewtypes.AgentConfig{Settings: json.RawMessage(`{"hooks":null}`)}, "settings hooks"},
		{"settings hook event not a list", "claude-code", reviewtypes.AgentConfig{Settings: json.RawMessage(`{"hooks":{"Stop":{}}}`)}, "hooks Stop"},
		{"settings hook event null", "claude-code", reviewtypes.AgentConfig{Settings: json.RawMessage(`{"hooks":{"Stop":null}}`)}, "hooks Stop"},
		{"settings not an object", "claude-code", reviewtypes.AgentConfig{Settings: json.RawMessage(`[]`)}, "settings"},
		{"codex settings unsupported", "codex", reviewtypes.AgentConfig{Settings: json.RawMessage(`{}`)}, `does not support "settings"`},
		{"pi MCP unsupported", "pi", reviewtypes.AgentConfig{MCPServers: mcp(`{"url":"x"}`)}, `does not support "mcp_servers"`},
		{"pi extension relative", "pi", reviewtypes.AgentConfig{Extensions: []string{".pi/ext.ts"}}, "absolute path"},
		{"pi extension", "pi", reviewtypes.AgentConfig{Extensions: []string{"/opt/ext.ts"}}, ""},
		{"unknown agent", "cursor", reviewtypes.AgentConfig{}, "does not support a review profile config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			err := ValidateAgentConfig(tt.agent, &cfg)
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

// Values naming a path inside the checkout are warned about, not refused.
func TestAgentConfigWarnings(t *testing.T) {
	t.Parallel()

	roots := []string{"/repo"}
	tests := []struct {
		name string
		cfg  reviewtypes.AgentConfig
		want int
	}{
		{"hook in checkout", reviewtypes.AgentConfig{Settings: json.RawMessage(`{"hooks":{"Stop":[{"hooks":[{"command":"/repo/bin/hook --x"}]}]}}`)}, 1},
		{"project dir variable", reviewtypes.AgentConfig{Settings: json.RawMessage(`{"statusLine":{"command":"$CLAUDE_PROJECT_DIR/s.sh"}}`)}, 1},
		{"MCP env PATH into checkout", reviewtypes.AgentConfig{MCPServers: map[string]json.RawMessage{"x": json.RawMessage(`{"command":"tool","env":{"PATH":"/usr/bin:/repo/bin"}}`)}}, 1},
		{"flag value in checkout", reviewtypes.AgentConfig{Settings: json.RawMessage(`{"apiKeyHelper":"/opt/key --config=/repo/k.json"}`)}, 1},
		{"extension in checkout", reviewtypes.AgentConfig{Extensions: []string{"/repo/.pi/x.ts"}}, 1},
		{"similar prefix", reviewtypes.AgentConfig{Extensions: []string{"/repository/x.ts"}}, 0},
		{"outside", reviewtypes.AgentConfig{Settings: json.RawMessage(`{"hooks":{"Stop":[{"hooks":[{"command":"yarn lint"}]}]}}`)}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			if got := AgentConfigWarnings(&cfg, roots); len(got) != tt.want {
				t.Fatalf("AgentConfigWarnings() = %q, want %d", got, tt.want)
			}
		})
	}
}

// A path outside the checkout that links into it is inside it.
func TestAgentConfigWarningsFollowSymlinks(t *testing.T) {
	t.Parallel()

	checkout := t.TempDir()
	testutil.WriteFile(t, checkout, ".pi/ext.ts", "x")
	link := filepath.Join(t.TempDir(), "ext.ts")
	if err := os.Symlink(filepath.Join(checkout, ".pi", "ext.ts"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if got := AgentConfigWarnings(&reviewtypes.AgentConfig{Extensions: []string{link}}, []string{resolved}); len(got) != 1 {
		t.Fatalf("AgentConfigWarnings() = %q, want one warning", got)
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
	if title != "Load this branch's skills during the review?" || !strings.Contains(body, "uses your profile's config") || strings.Contains(body, "Pi") {
		t.Fatalf("title/body = %q / %q", title, body)
	}
	inv.IsolatedAgents = []string{"claude-code", "pi"}
	if _, body := trustConfirmText(subject, inv, "x"); !strings.Contains(body, "Pi settings") {
		t.Errorf("isolated Pi reviewer, body doesn't mention Pi settings: %q", body)
	}
	if title, body := trustConfirmText(subject, TrustInventory{Isolated: true}, "x"); title != "Review this branch?" || !strings.Contains(body, "uses your profile's config") || strings.Contains(body, "nothing from it runs") {
		t.Fatalf("empty isolated title/body = %q / %q", title, body)
	}
}

// Only a reviewer that will run, in exactly this profile, stops the review.
func TestUnverifiedReviewerConfig(t *testing.T) {
	t.Parallel()

	profile := settings.ReviewProfileConfig{Agents: map[string]settings.ReviewConfig{
		"claude-code": {Agent: "claude-code"}, "codex": {Agent: "codex"},
	}}
	unverified := []string{"review_profiles.a.b.agents.claude-code.config", "review_profiles.a.agents.claude-code.config"}
	if field, ok := unverifiedReviewerConfig(unverified, "a", profile, ""); !ok || field != unverified[1] {
		t.Errorf("all reviewers: got %q, %v", field, ok)
	}
	if _, ok := unverifiedReviewerConfig(unverified, "a", profile, "codex"); ok {
		t.Error("--agent codex was stopped by claude-code's config")
	}
	if _, ok := unverifiedReviewerConfig(unverified[:1], "a", profile, ""); ok {
		t.Error("profile a was stopped by profile a.b's config")
	}
}

func TestPrintBlockedTools(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	printBlockedTools(&b, "general", reviewtypes.RunSummary{AgentRuns: []reviewtypes.AgentRun{
		{Name: "claude-code", Buffer: []reviewtypes.Event{reviewtypes.ToolsBlocked{Names: []string{"mcp__docs__search"}}}},
		{Name: "codex"},
	}})
	out := b.String()
	if !strings.Contains(out, "claude-code: its permission settings blocked mcp__docs__search") || !strings.Contains(out, "--configure general --set-config") || strings.Contains(out, "codex") {
		t.Fatalf("output:\n%s", out)
	}
}
