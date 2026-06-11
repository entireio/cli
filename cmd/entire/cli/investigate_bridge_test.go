package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestBuildInvestigateDeps_HasRequiredFields asserts that the bridge
// populates every dep field that NewCommand needs at runtime. Fields
// that are intentionally nil (PriorEntireContextFn, LoopRun) are not
// asserted — a future task can wire them.
func TestBuildInvestigateDeps_HasRequiredFields(t *testing.T) {
	t.Parallel()

	deps := buildInvestigateDeps()

	if deps.GetAgentsWithHooksInstalled == nil {
		t.Fatal("buildInvestigateDeps: GetAgentsWithHooksInstalled is nil")
	}
	if deps.NewSilentError == nil {
		t.Fatal("buildInvestigateDeps: NewSilentError is nil")
	}
	if deps.SpawnerFor == nil {
		t.Fatal("buildInvestigateDeps: SpawnerFor is nil")
	}
	if deps.LaunchFix == nil {
		t.Fatal("buildInvestigateDeps: LaunchFix is nil")
	}
}

// TestLaunchableSpawnerFor_KnownAgents validates the per-agent switch in
// the bridge. Launchable agents return non-nil spawners; non-launchable
// or unknown names return nil so verifyAgentsLaunchable can refuse them.
func TestLaunchableSpawnerFor_KnownAgents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		agent       string
		wantNil     bool
		description string
	}{
		{name: "claude-code", agent: "claude-code", wantNil: false, description: "launchable"},
		{name: "codex", agent: "codex", wantNil: false, description: "launchable"},
		{name: "gemini", agent: "gemini", wantNil: false, description: "launchable"},
		{name: "cursor", agent: "cursor", wantNil: true, description: "non-launchable"},
		{name: "opencode", agent: "opencode", wantNil: true, description: "non-launchable"},
		{name: "factoryai-droid", agent: "factoryai-droid", wantNil: true, description: "non-launchable"},
		{name: "copilot-cli", agent: "copilot-cli", wantNil: true, description: "non-launchable"},
		{name: "vogon", agent: "vogon", wantNil: true, description: "non-launchable"},
		{name: "empty", agent: "", wantNil: true, description: "empty string"},
		{name: "unknown", agent: "not-a-real-agent", wantNil: true, description: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := launchableSpawnerFor(tt.agent)
			if tt.wantNil && got != nil {
				t.Fatalf("launchableSpawnerFor(%q) = non-nil, want nil (%s)", tt.agent, tt.description)
			}
			if !tt.wantNil && got == nil {
				t.Fatalf("launchableSpawnerFor(%q) = nil, want non-nil (%s)", tt.agent, tt.description)
			}
		})
	}
}

// TestRootCommand_HasInvestigate confirms `entire investigate` is wired
// into the root command tree. It also checks that the command is
// Hidden (the experimental discovery happens via `entire labs`).
func TestRootCommand_HasInvestigate(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"investigate"})
	if err != nil {
		t.Fatalf("root.Find(investigate): %v", err)
	}
	if cmd == nil {
		t.Fatal("investigate command not registered on root")
	}
	if cmd.Name() != "investigate" {
		t.Fatalf("resolved command name = %q, want %q", cmd.Name(), "investigate")
	}
	if !cmd.Hidden {
		t.Fatal("investigate should be Hidden during maturation")
	}
}

// TestRootCommand_InvestigateHelpRuns smoke-tests that `entire
// investigate --help` produces output without error. This is the
// minimal functional confirmation that the bridge wired enough deps
// for cobra to parse the command.
func TestRootCommand_InvestigateHelpRuns(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"investigate", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("entire investigate --help failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "investigate") {
		t.Fatalf("help output missing 'investigate':\n%s", got)
	}
}

// TestLabs_ListsInvestigate confirms the labs overview now advertises
// the investigate command alongside review.
func TestLabs_ListsInvestigate(t *testing.T) {
	t.Parallel()

	got := labsOverview()
	for _, want := range []string{
		"entire investigate",
		"multi-agent investigation",
		"entire investigate --help",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("labsOverview missing %q:\n%s", want, got)
		}
	}
}
