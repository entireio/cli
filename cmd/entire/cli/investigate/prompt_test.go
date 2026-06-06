package investigate

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "update golden files in testdata/")

// assertGoldenString writes/reads a golden file under testdata/. When
// -update is passed it overwrites the golden, otherwise it compares.
func assertGoldenString(t *testing.T, goldenPath, got string) {
	t.Helper()
	abs, err := filepath.Abs(goldenPath)
	if err != nil {
		t.Fatalf("abs golden path: %v", err)
	}
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	wantBytes, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read golden %s: %v (run go test ./... -update to create)", goldenPath, err)
	}
	if want := string(wantBytes); want != got {
		t.Errorf("prompt mismatch (golden=%s)\nWANT:\n%s\n\nGOT:\n%s", goldenPath, want, got)
	}
}

func TestComposeInvestigatePrompt_FirstRound(t *testing.T) {
	t.Parallel()

	got := ComposeInvestigatePrompt(ComposeInput{
		Topic:     "Why is checkout flaky?",
		AgentName: "claude-code",
		Round:     1,
		MaxTurns:  3,
		Turn:      1,
		Files: Files{
			Findings: "/abs/repo/.git/entire-investigations/abcdef012345/findings.md",
			State:    "/abs/repo/.git/entire-investigations/abcdef012345/state.json",
		},
	})

	assertGoldenString(t, "testdata/prompt-first-round.txt", got)

	// Sanity checks the golden doesn't catch on its own.
	for _, want := range []string{
		"autonomous multi-agent investigation",
		"You are agent: claude-code",
		"Round: 1 of 3",
		"(turn 1 overall in this session)",
		"Findings: /abs/repo/.git/entire-investigations/abcdef012345/findings.md",
		"Use Entire tools deliberately",
		"Audit both sides for failure-rate questions",
		"Keep the TLDR section accurate every turn",
		"Do NOT add a \"## Recommendations\"",
		"marvin plan --from-investigation",
		"pending_turn",
		"approve",
		"request-changes",
		"reject",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing substring %q", want)
		}
	}
}

func TestComposeInvestigatePrompt_MidLoop(t *testing.T) {
	t.Parallel()

	got := ComposeInvestigatePrompt(ComposeInput{
		Topic:     "Why is checkout flaky?",
		AgentName: "codex",
		Round:     2,
		MaxTurns:  3,
		Turn:      5,
		Files: Files{
			Findings: "/abs/repo/.git/entire-investigations/abcdef012345/findings.md",
			State:    "/abs/repo/.git/entire-investigations/abcdef012345/state.json",
		},
	})

	assertGoldenString(t, "testdata/prompt-mid-loop.txt", got)

	if !strings.Contains(got, "Round: 2 of 3") {
		t.Errorf("expected mid-loop round/max coordinates")
	}
	if !strings.Contains(got, "(turn 5 overall in this session)") {
		t.Errorf("expected mid-loop overall turn coordinate")
	}
	if !strings.Contains(got, "You are agent: codex") {
		t.Errorf("expected codex as the rendered agent")
	}
}

func TestComposeInvestigatePrompt_WithAlwaysPrompt(t *testing.T) {
	t.Parallel()

	got := ComposeInvestigatePrompt(ComposeInput{
		Topic:        "Why is checkout flaky?",
		AgentName:    "claude-code",
		Round:        1,
		MaxTurns:     3,
		Turn:         1,
		AlwaysPrompt: "Project rule: cite test names in evidence.",
		Files: Files{
			Findings: "/abs/findings.md",
			State:    "/abs/state.json",
		},
	})

	assertGoldenString(t, "testdata/prompt-with-always.txt", got)

	if !strings.Contains(got, "Project rule: cite test names in evidence.") {
		t.Errorf("AlwaysPrompt was not appended verbatim")
	}
	// Should appear AFTER the main body — guard against accidental prepend.
	idxAlways := strings.Index(got, "Project rule: cite test names in evidence.")
	idxBody := strings.Index(got, "Exit once you've written")
	if idxAlways < idxBody {
		t.Errorf("AlwaysPrompt rendered before body (idxAlways=%d idxBody=%d)", idxAlways, idxBody)
	}
}
