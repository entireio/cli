package agent_test

import (
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"

	// Blank-import every built-in agent so their init() registers them in the
	// shared registry. The point of this test is that the absolute-path guard
	// holds for ALL agents, including any added in the future — a new agent
	// package picked up here (and in production wiring) is covered automatically.
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/codex"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/pi"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/vogon"
)

// TestResolveSessionFile_AllAgentsRejectAbsolute is the registry-wide security
// contract: every registered agent's ResolveSessionFile MUST reject an absolute
// agentSessionID with an error rather than joining or returning it. An absolute
// ID is attacker-influenceable (it can originate from checkpoint metadata on the
// shared entire/checkpoints/v1 branch), and the resume/rewind write path would
// otherwise let it overwrite files outside the session directory.
//
// This guards the whole class: a future agent that forgets the
// validation.RejectAbsoluteSessionID guard fails here.
func TestResolveSessionFile_AllAgentsRejectAbsolute(t *testing.T) {
	t.Parallel()

	// Platform-appropriate absolute paths. The Windows drive form is only
	// recognized as absolute on Windows, so only assert it there.
	absInputs := []string{"/etc/evil.jsonl"}
	if runtime.GOOS == "windows" {
		absInputs = append(absInputs, `C:\Windows\evil.jsonl`)
	}

	names := agent.List()
	if len(names) == 0 {
		t.Fatal("no agents registered; blank imports may have been dropped")
	}

	for _, name := range names {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			ag, err := agent.Get(name)
			if err != nil {
				t.Fatalf("Get(%q) error: %v", name, err)
			}
			for _, abs := range absInputs {
				got, resolveErr := ag.ResolveSessionFile("/home/u/.agent/sessions", abs)
				if resolveErr == nil {
					t.Errorf("ResolveSessionFile(%q) = %q, nil; want error rejecting the absolute path", abs, got)
				}
				if got != "" {
					t.Errorf("ResolveSessionFile(%q) returned path %q alongside error; want empty path", abs, got)
				}
			}
		})
	}
}
